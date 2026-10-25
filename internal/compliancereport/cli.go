/*
Copyright 2026.

Licensed under the Business Source License 1.1 (the "License"). You may not
use this file except in compliance with the License, a copy of which is in the
LICENSE file at the root of this repository. Production use requires a
commercial license from the Licensor. On the Change Date (2030-10-17) the
work becomes available under the Change License (Apache-2.0).
*/

package compliancereport

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"
)

// Exit codes. Zero means a report was written, whatever it says: the exit code
// is about whether the tool worked, not about the cluster.
const (
	ExitOK    = 0
	ExitError = 1
)

const usage = `Usage: pg-compliance [flags] [NAME]

Reads the compliance status the postgres-operator wrote for one PostgresCluster,
and the emergency access requests made for it, and prints a report. It only
reads. It never connects to a database and never reads a Secret.

Read from a cluster (needs get on postgresclusters and list on
postgresemergencyaccesses, in the namespace):
  pg-compliance -n NAMESPACE NAME

Read from a saved capture of  kubectl get postgrescluster,postgresemergencyaccess -o json:
  pg-compliance -f capture.json
  kubectl get postgrescluster,postgresemergencyaccess -o json | pg-compliance -f -

The report says what the operator's checks do and do not mean. That wording is
part of every format and cannot be turned off.

Flags:
`

// Env is what the command runs against, so that it can be tested.
type Env struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	Now    func() time.Time
	// StdoutIsTerminal says whether colour may be used.
	StdoutIsTerminal bool
	// LookupEnv reads an environment variable.
	LookupEnv func(string) (string, bool)
	// Live reads from the cluster. It is replaced in tests.
	Live func(context.Context, LiveOptions) (Input, error)
	// Version is printed by --version.
	Version string
}

// DefaultEnv is the process's own environment.
func DefaultEnv(version string) Env {
	stat, err := os.Stdout.Stat()
	return Env{
		Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, Now: time.Now,
		StdoutIsTerminal: err == nil && stat.Mode()&os.ModeCharDevice != 0,
		LookupEnv:        os.LookupEnv,
		Live:             LoadLive,
		Version:          version,
	}
}

// Run is the command. It returns the exit code.
func Run(ctx context.Context, args []string, env Env) int {
	fs := flag.NewFlagSet("pg-compliance", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	fs.Usage = func() {
		_, _ = fmt.Fprint(fs.Output(), usage)
		fs.PrintDefaults()
	}
	var (
		namespace  = fs.String("namespace", "", "namespace of the cluster; with a cluster, the one in the kubeconfig context; with a file, any")
		name       = fs.String("name", "", "name of the cluster (or give it as the argument)")
		file       = fs.String("f", "", "read a saved capture from this file, or - for standard input, instead of the cluster")
		output     = fs.String("output", string(FormatText), "format: text, markdown or json")
		noColor    = fs.Bool("no-color", false, "do not use colour (also off when NO_COLOR is set or output is not a terminal)")
		at         = fs.String("at", "", "measure the age of the report at this time (RFC 3339), not now; for a saved capture, its capture time")
		kubeconfig = fs.String("kubeconfig", "", "path to the kubeconfig")
		kubeCtx    = fs.String("context", "", "kubeconfig context to use")
		as         = fs.String("as", "", "user to impersonate, to see what a read-only user would see")
		version    = fs.Bool("version", false, "print the version")
	)
	fs.StringVar(namespace, "n", "", "short for --namespace")
	fs.StringVar(output, "o", string(FormatText), "short for --output")
	// Flags may come before or after the name.
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			if err == flag.ErrHelp {
				return ExitOK
			}
			return ExitError
		}
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if *version {
		_, _ = fmt.Fprintln(env.Stdout, "pg-compliance", env.Version)
		return ExitOK
	}

	fail := func(format string, a ...any) int {
		_, _ = fmt.Fprintf(env.Stderr, "pg-compliance: "+format+"\n", a...)
		return ExitError
	}
	format, err := ParseFormat(*output)
	if err != nil {
		return fail("%v", err)
	}
	if *name == "" && len(positional) == 1 {
		*name = positional[0]
	} else if len(positional) > 0 {
		return fail("give the cluster name once, as --name or as the argument")
	}

	readAt := env.Now()
	if *at != "" {
		if readAt, err = time.Parse(time.RFC3339, *at); err != nil {
			return fail("--at must be an RFC 3339 time such as 2026-10-25T03:42:40Z: %v", err)
		}
	}

	var in Input
	if *file != "" {
		in, err = loadFile(*file, env, *namespace, *name)
	} else {
		if *name == "" {
			return fail("say which cluster: pg-compliance -n NAMESPACE NAME, or read a capture with -f")
		}
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		in, err = env.Live(ctx, LiveOptions{
			Kubeconfig: *kubeconfig, Context: *kubeCtx, As: *as, Namespace: *namespace, Name: *name,
		})
	}
	if err != nil {
		return fail("%v", err)
	}

	report, err := Build(in, readAt)
	if err != nil {
		return fail("%s/%s: %v", in.Cluster.Namespace, in.Cluster.Name, err)
	}

	_, noColorSet := env.LookupEnv("NO_COLOR")
	color := format == FormatText && env.StdoutIsTerminal && !*noColor && !noColorSet
	if err := Render(env.Stdout, report, Options{Format: format, Color: color}); err != nil {
		return fail("writing the report: %v", err)
	}
	return ExitOK
}

func loadFile(path string, env Env, namespace, name string) (Input, error) {
	var (
		data   []byte
		err    error
		source string
	)
	if path == "-" {
		data, err = io.ReadAll(env.Stdin)
		source = "standard input"
	} else {
		data, err = os.ReadFile(path)
		source = "file " + path
	}
	if err != nil {
		return Input{}, fmt.Errorf("reading %s: %w", source, err)
	}
	objs, err := ParseObjects(data)
	if err != nil {
		return Input{}, fmt.Errorf("%s: %w", source, err)
	}
	return objs.Select(namespace, name, source)
}

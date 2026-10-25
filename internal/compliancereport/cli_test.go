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
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type run struct {
	code        int
	out, errOut string
}

func runCLI(t *testing.T, stdin string, tweak func(*Env), args ...string) run {
	t.Helper()
	var out, errOut bytes.Buffer
	env := Env{
		Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errOut,
		Now:       func() time.Time { return time.Date(2026, 10, 25, 3, 42, 40, 0, time.UTC) },
		LookupEnv: func(string) (string, bool) { return "", false },
		Live: func(context.Context, LiveOptions) (Input, error) {
			return Input{}, errors.New("the cluster was not expected to be read")
		},
		Version: "test",
	}
	if tweak != nil {
		tweak(&env)
	}
	code := Run(context.Background(), args, env)
	return run{code, out.String(), errOut.String()}
}

func fixturePath(name string) string { return filepath.Join("testdata", name+".json") }

func TestReadsACaptureFromAFile(t *testing.T) {
	r := runCLI(t, "", nil, "-f", fixturePath("not-met"))
	if r.code != ExitOK {
		t.Fatalf("exit %d: %s", r.code, r.errOut)
	}
	if !strings.Contains(r.out, "authentication") || !strings.Contains(oneLine(r.out), PresetIsNotCompliance) {
		t.Errorf("unexpected report:\n%s", r.out)
	}
}

func TestReadsACaptureFromStandardInput(t *testing.T) {
	data, err := os.ReadFile(fixturePath("all-met"))
	if err != nil {
		t.Fatal(err)
	}
	r := runCLI(t, string(data), nil, "-f", "-", "-o", "json")
	if r.code != ExitOK {
		t.Fatalf("exit %d: %s", r.code, r.errOut)
	}
	if !strings.Contains(r.out, `"source": "standard input"`) {
		t.Errorf("the source is not named:\n%s", r.out)
	}
}

// A report with controls that are not met, or out of date, is still a report.
func TestTheExitCodeIsAboutTheToolNotTheCluster(t *testing.T) {
	for _, name := range []string{"not-met", "operator-down", "fresh"} {
		if r := runCLI(t, "", nil, "-f", fixturePath(name)); r.code != ExitOK {
			t.Errorf("%s: exit %d: %s", name, r.code, r.errOut)
		}
	}
}

func TestThereIsNoFlagToFailOnTheResult(t *testing.T) {
	r := runCLI(t, "", nil, "-f", fixturePath("not-met"), "--fail-on-notmet")
	if r.code != ExitError || !strings.Contains(r.errOut, "flag provided but not defined") {
		t.Errorf("exit %d: %s", r.code, r.errOut)
	}
}

func TestTheAgeIsMeasuredNowOrAtTheTimeGiven(t *testing.T) {
	now := runCLI(t, "", nil, "-f", fixturePath("all-met"), "-o", "json")
	if !strings.Contains(now.out, `"freshness": "current"`) {
		t.Errorf("two minutes after the check it should be current:\n%s", now.out)
	}
	later := runCLI(t, "", func(e *Env) { e.Now = func() time.Time { return time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC) } },
		"-f", fixturePath("all-met"), "-o", "json")
	if !strings.Contains(later.out, `"freshness": "out-of-date"`) {
		t.Errorf("a day later it should be out of date:\n%s", later.out)
	}
	atCapture := runCLI(t, "", func(e *Env) { e.Now = func() time.Time { return time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC) } },
		"-f", fixturePath("all-met"), "-o", "json", "--at", "2026-10-25T03:45:00Z")
	if !strings.Contains(atCapture.out, `"freshness": "current"`) || !strings.Contains(atCapture.out, `"readAt": "2026-10-25T03:45:00Z"`) {
		t.Errorf("--at was not used:\n%s", atCapture.out)
	}
}

func TestFailuresSayWhatWentWrongAndExitNonZero(t *testing.T) {
	two := filepath.Join(t.TempDir(), "two.json")
	a, _ := os.ReadFile(fixturePath("all-met"))
	b := strings.Replace(string(a), `"name": "orders"`, `"name": "billing"`, 1)
	list := `{"kind":"List","items":[` + string(a) + `,` + b + `]}`
	if err := os.WriteFile(two, []byte(list), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		stdin string
		args  []string
		want  string
	}{
		{"unknown format", "", []string{"-f", fixturePath("all-met"), "-o", "pdf"}, "unknown format"},
		{"bad time", "", []string{"-f", fixturePath("all-met"), "--at", "yesterday"}, "RFC 3339"},
		{"no cluster named", "", nil, "say which cluster"},
		{"missing file", "", []string{"-f", "no-such-file.json"}, "no-such-file.json"},
		{"not a resource", "[1, 2", []string{"-f", "-"}, "neither JSON nor YAML"},
		{"no cluster in the file", `{"kind":"List","items":[]}`, []string{"-f", "-"}, "no PostgresCluster"},
		{"two clusters", "", []string{"-f", two}, "say which with --namespace and --name"},
		{"the named one is not there", "", []string{"-f", fixturePath("all-met"), "nope"}, "named nope"},
		{"name twice", "", []string{"-f", fixturePath("all-met"), "--name", "a", "b"}, "once"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := runCLI(t, c.stdin, nil, c.args...)
			if r.code != ExitError {
				t.Errorf("exit %d, want %d", r.code, ExitError)
			}
			if !strings.Contains(r.errOut, c.want) {
				t.Errorf("stderr %q lacks %q", r.errOut, c.want)
			}
			if r.out != "" {
				t.Errorf("a failure wrote a report:\n%s", r.out)
			}
		})
	}

	// With the name, one of two clusters is picked.
	if r := runCLI(t, "", nil, "-f", two, "billing", "-o", "json"); r.code != ExitOK || !strings.Contains(r.out, `"name": "billing"`) {
		t.Errorf("exit %d: %s%s", r.code, r.errOut, r.out)
	}
}

func TestAClusterWithNoPresetIsRefusedByName(t *testing.T) {
	raw := `{"kind":"PostgresCluster","metadata":{"namespace":"shop","name":"plain"},"spec":{}}`
	r := runCLI(t, raw, nil, "-f", "-")
	if r.code != ExitError || !strings.Contains(r.errOut, "shop/plain") || !strings.Contains(r.errOut, "no compliance preset") {
		t.Errorf("exit %d: %s", r.code, r.errOut)
	}
}

func TestTheClusterIsReadWithTheOptionsGiven(t *testing.T) {
	data, _ := os.ReadFile(fixturePath("not-met"))
	objs, err := ParseObjects(data)
	if err != nil {
		t.Fatal(err)
	}
	in, err := objs.Select("", "", "live cluster")
	if err != nil {
		t.Fatal(err)
	}
	var got LiveOptions
	r := runCLI(t, "", func(e *Env) {
		e.Live = func(_ context.Context, o LiveOptions) (Input, error) { got = o; return in, nil }
	}, "-n", testNamespace, "--kubeconfig", "/k", "--context", "kind", "--as", "auditor@example.com", "orders")
	if r.code != ExitOK {
		t.Fatalf("exit %d: %s", r.code, r.errOut)
	}
	want := LiveOptions{Kubeconfig: "/k", Context: "kind", As: "auditor@example.com", Namespace: testNamespace, Name: "orders"}
	if got != want {
		t.Errorf("read with %+v, want %+v", got, want)
	}
}

func TestAClusterThatCannotBeReadIsAnError(t *testing.T) {
	r := runCLI(t, "", func(e *Env) {
		e.Live = func(context.Context, LiveOptions) (Input, error) { return Input{}, errors.New("not allowed to list") }
	}, "-n", testNamespace, "orders")
	if r.code != ExitError || !strings.Contains(r.errOut, "not allowed to list") || r.out != "" {
		t.Errorf("exit %d: %s / %s", r.code, r.errOut, r.out)
	}
}

func TestColourNeedsATerminalAndNoOptOut(t *testing.T) {
	cases := []struct {
		name     string
		terminal bool
		env      map[string]string
		flag     []string
		want     bool
	}{
		{"terminal", true, nil, nil, true},
		{"piped", false, nil, nil, false},
		{"NO_COLOR", true, map[string]string{"NO_COLOR": "1"}, nil, false},
		{"--no-color", true, nil, []string{"--no-color"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			args := append([]string{"-f", fixturePath("not-met")}, c.flag...)
			r := runCLI(t, "", func(e *Env) {
				e.StdoutIsTerminal = c.terminal
				e.LookupEnv = func(k string) (string, bool) { v, ok := c.env[k]; return v, ok }
			}, args...)
			if got := strings.Contains(r.out, "\x1b["); got != c.want {
				t.Errorf("colour %v, want %v", got, c.want)
			}
		})
	}
	// JSON is never painted.
	r := runCLI(t, "", func(e *Env) { e.StdoutIsTerminal = true }, "-f", fixturePath("not-met"), "-o", "json")
	if strings.Contains(r.out, "\x1b[") {
		t.Error("JSON was painted")
	}
}

func TestVersion(t *testing.T) {
	if r := runCLI(t, "", nil, "--version"); r.code != ExitOK || !strings.Contains(r.out, "test") {
		t.Errorf("exit %d: %s", r.code, r.out)
	}
}

func TestTheNamespaceChoosesBetweenClustersWithTheSameName(t *testing.T) {
	a, _ := os.ReadFile(fixturePath("all-met"))
	b := strings.Replace(string(a), `"namespace": "live13"`, `"namespace": "staging"`, 1)
	list := `{"kind":"List","items":[` + string(a) + `,` + b + `]}`
	r := runCLI(t, list, nil, "-f", "-", "-n", "staging", "orders", "-o", "json")
	if r.code != ExitOK || !strings.Contains(r.out, `"namespace": "staging"`) {
		t.Errorf("exit %d: %s%s", r.code, r.errOut, r.out)
	}
	if r := runCLI(t, list, nil, "-f", "-", "orders"); r.code != ExitError {
		t.Errorf("two clusters named orders were not refused: exit %d", r.code)
	}
}

func TestYAMLCapturesAreRead(t *testing.T) {
	raw := `
apiVersion: v1
kind: List
items:
- kind: ConfigMap
  metadata: {name: ignored}
- kind: PostgresCluster
  metadata: {namespace: shop, name: orders}
  spec: {compliance: {standard: soc2}}
`
	r := runCLI(t, raw, nil, "-f", "-", "-o", "json")
	if r.code != ExitOK || !strings.Contains(r.out, `"standard": "soc2"`) {
		t.Errorf("exit %d: %s%s", r.code, r.errOut, r.out)
	}
}

//go:build e2e

/*
Copyright 2026.

Licensed under the Business Source License 1.1 (the "License"). You may not
use this file except in compliance with the License, a copy of which is in the
LICENSE file at the root of this repository. Production use requires a
commercial license from the Licensor. On the Change Date (2030-10-17) the
work becomes available under the Change License (Apache-2.0).
*/

package e2e

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// kubectlOut runs kubectl and returns its standard output only. The shared
// utils.Run merges standard error into the result, and kubectl writes
// deprecation warnings there (core Endpoints, which Patroni uses, draw one on
// every read), which would corrupt anything parsed from the output.
func kubectlOut(args ...string) (string, error) {
	return kubectlIn("", args...)
}

// kubectlIn is kubectlOut with the given text on standard input.
func kubectlIn(stdin string, args ...string) (string, error) {
	cmd := exec.Command("kubectl", args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return strings.TrimSpace(string(out)), fmt.Errorf("kubectl %s: %w: %s",
			strings.Join(args, " "), errors.Unwrap(err), strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}

// inCluster is kubectl scoped to the namespace the test clusters live in.
func inCluster(args ...string) (string, error) {
	return kubectlOut(append([]string{"-n", clusterNamespace}, args...)...)
}

// clusterField reads one field of the PostgresCluster with a JSONPath.
func clusterField(path string) (string, error) {
	return inCluster("get", "postgrescluster", clusterName, "-o", "jsonpath={"+path+"}")
}

// conditionStatus is the status of one condition of the PostgresCluster.
func conditionStatus(conditionType string) (string, error) {
	return clusterField(fmt.Sprintf(".status.conditions[?(@.type==%q)].status", conditionType))
}

// runInPod runs a bash script in the postgres container of a pod. The script
// receives args as $1, $2, ... so values never need quoting into it.
func runInPod(pod, script string, args ...string) (string, error) {
	cmd := append([]string{"exec", pod, "-c", "postgres", "--", "bash", "-c", script, "bash"}, args...)
	return inCluster(cmd...)
}

// psql runs one statement as the superuser from inside a pod. host is a
// Service or pod address, or empty to use the pod's local socket. The
// password is the one the pod already holds in its environment, so it never
// passes through the test.
func psql(pod, host, sql string) (string, error) {
	return runInPod(pod,
		`PGPASSWORD="$PGPASSWORD_SUPERUSER" psql ${1:+-h "$1"} -U postgres -d postgres -tAX -c "$2"`,
		host, sql)
}

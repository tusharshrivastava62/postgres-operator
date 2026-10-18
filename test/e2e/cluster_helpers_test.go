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
	"strconv"
	"strings"
	"time"
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

// writerManifest is a pod outside the database that inserts a row every
// 100ms through the read-write Service, as an application would, and logs
// "ok <ms>" or "fail <ms>" for each attempt. It runs as the same non-root
// user, so it is admitted by the namespace's strict Pod Security level.
func writerManifest(image string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: writer
  namespace: %s
spec:
  restartPolicy: Never
  securityContext:
    runAsNonRoot: true
    runAsUser: 101
    runAsGroup: 103
    seccompProfile: {type: RuntimeDefault}
  containers:
  - name: writer
    image: %s
    securityContext:
      allowPrivilegeEscalation: false
      capabilities: {drop: [ALL]}
    env:
    - {name: PGCONNECT_TIMEOUT, value: "2"}
    - name: PGPASSWORD
      valueFrom: {secretKeyRef: {name: %s-credentials, key: password}}
    command:
    - bash
    - -c
    - |
      while true; do
        s=$(date +%%s%%3N)
        if psql -h %s -U postgres -d postgres -tAX -c "insert into e2e_rows(v) values ('writer')" >/dev/null 2>&1; then
          echo "ok $s"
        else
          echo "fail $s"
        fi
        sleep 0.1
      done
`, clusterNamespace, image, clusterName, clusterName)
}

// writerResult summarises a writer's log.
type writerResult struct {
	acknowledged int
	failed       int
	// longestGap is the longest time between two acknowledged inserts, which
	// is how long an application would have seen the database refuse writes.
	longestGap time.Duration
}

func parseWriterLog(log string) writerResult {
	var res writerResult
	var last int64
	for line := range strings.SplitSeq(log, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		ms, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "ok":
			if last != 0 {
				if gap := time.Duration(ms-last) * time.Millisecond; gap > res.longestGap {
					res.longestGap = gap
				}
			}
			last = ms
			res.acknowledged++
		case "fail":
			res.failed++
		}
	}
	return res
}

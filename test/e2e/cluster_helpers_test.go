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
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/gomega"
)

// postgresContainer is the database container of a cluster pod.
const postgresContainer = "postgres"

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
	cmd := append([]string{"exec", pod, "-c", postgresContainer, "--", "bash", "-c", script, "bash"}, args...)
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

// clusterFieldOf is clusterField for a PostgresCluster other than the main one.
func clusterFieldOf(name, path string) (string, error) {
	return inCluster("get", "postgrescluster", name, "-o", "jsonpath={"+path+"}")
}

// conditionOf is conditionStatus for another PostgresCluster.
func conditionOf(name, conditionType string) (string, error) {
	return clusterFieldOf(name, fmt.Sprintf(".status.conditions[?(@.type==%q)].status", conditionType))
}

// kubectlDryRun sends a manifest through the API server and its admission
// webhooks without storing it, and returns everything kubectl printed, so that
// the warnings a webhook attaches to an accepted request are visible too.
func kubectlDryRun(manifest string) (string, error) {
	cmd := exec.Command("kubectl", "apply", "--dry-run=server", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// shippedAuditLog is what verifyShippedAuditLog found in the audit bucket.
type shippedAuditLog struct {
	Series     int             `json:"series"`
	Chunks     int             `json:"chunks"`
	Bytes      int             `json:"bytes"`
	Contiguous bool            `json:"contiguous"`
	Matches    bool            `json:"matches"`
	Markers    map[string]bool `json:"markers"`
}

// auditVerifyScript reads one pod's audit log back out of the bucket, from
// inside the pod's shipper container, which already holds the destination and
// its credentials. It checks that each series is made of contiguous byte
// ranges starting at zero, that the ranges joined are the start of the log file
// on the volume, and which of the given markers the shipped text contains.
const auditVerifyScript = `
import boto3, gzip, json, os, re, sys
from botocore.config import Config

style = "path" if os.environ["AUDIT_FORCE_PATH_STYLE"] == "true" else "auto"
client = boto3.client("s3", endpoint_url=os.environ["AUDIT_ENDPOINT"] or None,
                      region_name=os.environ["AUDIT_REGION"] or "us-east-1",
                      config=Config(s3={"addressing_style": style}))
prefix = "%s/%s/" % (os.environ["AUDIT_PREFIX"].strip("/"), os.environ["POD_NAME"])
keys, token = [], None
while True:
    args = {"Bucket": os.environ["AUDIT_BUCKET"], "Prefix": prefix}
    if token:
        args["ContinuationToken"] = token
    page = client.list_objects_v2(**args)
    keys += [item["Key"] for item in page.get("Contents", [])]
    if not page.get("IsTruncated"):
        break
    token = page["NextContinuationToken"]

series = {}
for key in keys:
    match = re.match(r"(.*/)(\d{12})-(\d{12})\.csv\.gz$", key)
    if match:
        series.setdefault(match.group(1), []).append((int(match.group(2)), int(match.group(3)), key))

result = {"series": len(series), "chunks": len(keys), "bytes": 0, "contiguous": True, "matches": True}
text = b""
for name, chunks in series.items():
    chunks.sort()
    covered = 0
    joined = b""
    for start, end, key in chunks:
        if start != covered:
            result["contiguous"] = False
        covered = end
        joined += gzip.decompress(client.get_object(Bucket=os.environ["AUDIT_BUCKET"], Key=key)["Body"].read())
    logfile = os.path.join(os.environ["LOG_DIR"], name.rstrip("/").split("/")[-2])
    if not open(logfile, "rb").read().startswith(joined):
        result["matches"] = False
    result["bytes"] += len(joined)
    text += joined
result["markers"] = {marker: marker.encode() in text for marker in sys.argv[1:]}
print(json.dumps(result))
`

// verifyShippedAuditLog runs auditVerifyScript against a pod's shipper.
func verifyShippedAuditLog(pod string, markers ...string) shippedAuditLog {
	args := append([]string{"exec", pod, "-c", "audit-shipper", "--", "python3", "-c", auditVerifyScript}, markers...)
	out, err := inCluster(args...)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), out)
	// The result is the script's last line; anything before it is not JSON.
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var result shippedAuditLog
	ExpectWithOffset(1, json.Unmarshal([]byte(lines[len(lines)-1]), &result)).To(Succeed(), out)
	return result
}

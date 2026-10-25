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
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/tusharshrivastava62/postgres-operator/test/utils"
)

// clientLabel marks a pod as an application that may connect to a cluster with
// compliance settings.
const (
	clientLabel      = "db.youroperator.io/client"
	clientLabelValue = "true"
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

// buildPGCompliance builds the pg-compliance command into a directory that is
// removed when the spec ends, and returns the path to it.
func buildPGCompliance() string {
	root, err := utils.GetProjectDir()
	Expect(err).NotTo(HaveOccurred())
	dir, err := os.MkdirTemp("", "pg-compliance-")
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func() { _ = os.RemoveAll(dir) })
	binary := filepath.Join(dir, "pg-compliance")
	build := exec.Command("go", "build", "-o", binary, "./cmd/pg-compliance")
	build.Dir = root
	out, err := build.CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), string(out))
	return binary
}

// complianceReport is what pg-compliance says about the cluster, as JSON.
type complianceReport struct {
	Freshness     string   `json:"freshness"`
	ReadThisFirst []string `json:"readThisFirst"`
	Summary       struct {
		Met, NotMet, Unknown, OutOfDate int
	} `json:"summary"`
	Controls []struct {
		ID, Status string
	} `json:"controls"`
	EmergencyAccess struct {
		Warning  *struct{ Statements []string } `json:"warning"`
		Requests []struct{ Name, State, RequestedBy, Role string }
	} `json:"emergencyAccess"`
}

// readComplianceReport runs the command against the cluster, reading as the
// current kubeconfig user, and returns the text report and the JSON one.
func readComplianceReport(binary string) (string, complianceReport, error) {
	var report complianceReport
	text, err := exec.Command(binary, "-n", clusterNamespace, clusterName).CombinedOutput()
	if err != nil {
		return "", report, fmt.Errorf("pg-compliance: %w: %s", err, text)
	}
	raw, err := exec.Command(binary, "-n", clusterNamespace, clusterName, "-o", "json").Output()
	if err != nil {
		return "", report, fmt.Errorf("pg-compliance -o json: %w", err)
	}
	return string(text), report, json.Unmarshal(raw, &report)
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
  # A client says so: a cluster with compliance settings admits only pods that do.
  labels: {db.youroperator.io/client: "true"}
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

// connectionProbe is a pod that tries once to open a TCP connection, and says
// whether it could. It is built to be admitted by the strictest Pod Security
// level, so it can run beside the database.
func connectionProbe(namespace, name, image string, podLabels map[string]string, host string, port int) string {
	labels := "{}"
	if len(podLabels) > 0 {
		pairs := make([]string, 0, len(podLabels))
		for k, v := range podLabels {
			pairs = append(pairs, fmt.Sprintf("%q: %q", k, v))
		}
		labels = "{" + strings.Join(pairs, ", ") + "}"
	}
	return fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %[2]s
  namespace: %[1]s
  labels: %[4]s
spec:
  restartPolicy: Never
  securityContext:
    runAsNonRoot: true
    runAsUser: 101
    runAsGroup: 103
    seccompProfile: {type: RuntimeDefault}
  containers:
  - name: probe
    image: %[3]s
    securityContext:
      allowPrivilegeEscalation: false
      capabilities: {drop: [ALL]}
    command:
    - python3
    - -c
    - |
      import socket, sys
      s = socket.socket()
      s.settimeout(5)
      try:
          s.connect((sys.argv[1], int(sys.argv[2])))
          print("OPEN")
      except Exception:
          print("CLOSED")
    - %[5]s
    - "%[6]d"
`, namespace, name, image, labels, host, port)
}

// canConnect runs a connectionProbe and reports whether the connection opened.
func canConnect(namespace, name, image string, podLabels map[string]string, host string, port int) bool {
	_, err := kubectlIn(connectionProbe(namespace, name, image, podLabels, host, port), "apply", "-f", "-")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	defer func() { _, _ = kubectlOut("-n", namespace, "delete", "pod", name, "--wait=false") }()
	var answer string
	EventuallyWithOffset(1, func(g Gomega) {
		phase, err := kubectlOut("-n", namespace, "get", "pod", name, "-o", "jsonpath={.status.phase}")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(phase).To(Equal("Succeeded"))
		answer, err = kubectlOut("-n", namespace, "logs", name)
		g.Expect(err).NotTo(HaveOccurred())
	}, 3*time.Minute, pollEvery).Should(Succeed())
	return strings.TrimSpace(answer) == "OPEN"
}

// controlResult is one compliance control as the cluster reports it.
type controlResult struct {
	Status   string
	Evidence string
}

// complianceState is the compliance part of a cluster's status.
type complianceState struct {
	Controls     map[string]controlResult
	LastChecked  string
	MetStatus    string // the ControlsMet condition's status, empty when there is none
	MetMessage   string
	ControlCount int
}

// readCompliance reads status.compliance and the ControlsMet condition.
func readCompliance() (complianceState, error) {
	out, err := inCluster("get", "postgrescluster", clusterName, "-o", "json")
	if err != nil {
		return complianceState{}, err
	}
	var cluster struct {
		Status struct {
			Compliance struct {
				LastChecked string `json:"lastChecked"`
				Controls    []struct {
					ID       string `json:"id"`
					Status   string `json:"status"`
					Evidence string `json:"evidence"`
				} `json:"controls"`
			} `json:"compliance"`
			Conditions []struct {
				Type    string `json:"type"`
				Status  string `json:"status"`
				Message string `json:"message"`
			} `json:"conditions"`
		} `json:"status"`
	}
	if err := json.Unmarshal([]byte(out), &cluster); err != nil {
		return complianceState{}, err
	}
	state := complianceState{Controls: map[string]controlResult{}, LastChecked: cluster.Status.Compliance.LastChecked}
	for _, c := range cluster.Status.Compliance.Controls {
		state.Controls[c.ID] = controlResult{Status: c.Status, Evidence: c.Evidence}
	}
	state.ControlCount = len(cluster.Status.Compliance.Controls)
	for _, c := range cluster.Status.Conditions {
		if c.Type == "ControlsMet" {
			state.MetStatus, state.MetMessage = c.Status, c.Message
		}
	}
	return state, nil
}

// runComplianceJobs runs the compliance check and the network probe now,
// instead of waiting for their schedule, and waits for both to finish.
func runComplianceJobs(suffix string) {
	jobs := []string{"check-" + suffix, "probe-" + suffix}
	for i, cronjob := range []string{clusterName + "-compliance-check", clusterName + "-network-probe"} {
		_, err := inCluster("create", "job", jobs[i], "--from=cronjob/"+cronjob)
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		job := jobs[i]
		DeferCleanup(func() { _, _ = inCluster("delete", "job", job, "--wait=false") })
	}
	for _, job := range jobs {
		EventuallyWithOffset(1, func() (string, error) {
			return inCluster("get", "job", job, "-o", "jsonpath={.status.succeeded}")
		}, 5*time.Minute, pollEvery).Should(Equal("1"), job)
	}
}

const monitorRole = "pgoperator_monitor"

// monitorRoleAttributes reads the monitor role's attributes back from the
// database: superuser, create database, create role, replication, bypass RLS,
// can log in, connection limit, and the roles it is a member of.
func monitorRoleAttributes(pod string) (string, error) {
	return psql(pod, "", "select rolsuper, rolcreatedb, rolcreaterole, rolreplication, rolbypassrls, rolcanlogin, rolconnlimit, "+
		"(select string_agg(pg_get_userbyid(roleid), ',') from pg_auth_members where member = r.oid) "+
		"from pg_roles r where rolname = '"+monitorRole+"'")
}

// cronJobSecrets lists the Secrets a CronJob's containers read as environment
// variables.
func cronJobSecrets(name string) (string, error) {
	return inCluster("get", "cronjob", name, "-o",
		"jsonpath={.spec.jobTemplate.spec.template.spec.containers[*].env[*].valueFrom.secretKeyRef.name}")
}

// asMonitor runs one statement on a database pod over TCP, as the monitor
// role with the password in its Secret. A statement the server refuses is not an
// error here: its answer is the output, so a test can read the refusal.
func asMonitor(pod, host, sql string) (string, error) {
	encoded, err := inCluster("get", "secret", clusterName+"-monitor", "-o", "jsonpath={.data.password}")
	if err != nil {
		return "", err
	}
	password, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", err
	}
	return runInPod(pod, fmt.Sprintf(`PGPASSWORD='%s' PGSSLMODE=require psql -X -At -h %s -U %s -d postgres -c "%s" 2>&1 || true`,
		password, host, monitorRole, sql))
}

// secretValue reads one entry of a Secret in the cluster's namespace.
func secretValue(secret, key string) (string, error) {
	encoded, err := inCluster("get", "secret", secret, "-o", "jsonpath={.data."+key+"}")
	if err != nil {
		return "", err
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	return string(decoded), err
}

// asRole runs one statement on a database pod over TCP, through a cluster's
// Service, as the application role whose details are in a Secret. A statement
// the server refuses is not an error here: its answer is the output.
func asRole(pod, cluster, secret, database, sql string) (string, error) {
	user, err := secretValue(secret, "username")
	if err != nil {
		return "", err
	}
	password, err := secretValue(secret, "password")
	if err != nil {
		return "", err
	}
	return runInPod(pod, fmt.Sprintf(`PGPASSWORD='%s' PGSSLMODE=require psql -X -At -h %s -U %s -d %s -c "%s" 2>&1 || true`,
		password, cluster, user, database, sql))
}

// roleState reads the state status gives a role of a cluster, or "" if it is not listed.
func roleState(cluster, role string) (string, error) {
	return clusterFieldOf(cluster, `.status.accessControl.roles[?(@.name=="`+role+`")].state`)
}

// roleWriterManifest is a pod that inserts a row every 100ms through the
// read-write Service as an application role, with the details in a Secret that
// it reads when it starts, and logs "ok <ms>" or "fail <ms>" for each attempt.
func roleWriterManifest(image, secret, database, table string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: role-writer
  namespace: %[1]s
spec:
  restartPolicy: Never
  securityContext:
    runAsNonRoot: true
    runAsUser: 101
    runAsGroup: 103
    seccompProfile: {type: RuntimeDefault}
  containers:
  - name: writer
    image: %[2]s
    securityContext:
      allowPrivilegeEscalation: false
      capabilities: {drop: [ALL]}
    env:
    - {name: PGCONNECT_TIMEOUT, value: "2"}
    - {name: PGSSLMODE, value: require}
    - {name: PGUSER, valueFrom: {secretKeyRef: {name: %[3]s, key: username}}}
    - {name: PGPASSWORD, valueFrom: {secretKeyRef: {name: %[3]s, key: password}}}
    command:
    - bash
    - -c
    - |
      while true; do
        s=$(date +%%s%%3N)
        if psql -h %[6]s -d %[4]s -tAX -c "insert into %[5]s values (1)" >/dev/null 2>&1; then
          echo "ok $s"
        else
          echo "fail $s"
        fi
        sleep 0.1
      done
`, clusterNamespace, image, secret, database, table, clusterName)
}

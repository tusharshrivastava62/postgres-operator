/*
Copyright 2026.

Licensed under the Business Source License 1.1 (the "License"). You may not
use this file except in compliance with the License, a copy of which is in the
LICENSE file at the root of this repository. Production use requires a
commercial license from the Licensor. On the Change Date (2030-10-17) the
work becomes available under the Change License (Apache-2.0).
*/

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus/testutil"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

const (
	noSuchLifecycle = "NoSuchLifecycleConfiguration"
	elevenAM        = "2026-10-19T11:00:00+00:00"
	keyDays         = "days"
	keyEnabled      = "enabled"
	keyPrefix       = "prefix"
)

var _ = Describe("The compliance check CronJob", func() {
	cluster := func() *dbv1alpha1.PostgresCluster {
		c := withCompliance(plainCluster(), dbv1alpha1.StandardHIPAA)
		c.Name, c.Namespace = sourceName, sourceNamespace
		c.Spec.Compliance.AuditLogs.Destination.Endpoint = "http://minio:9000"
		c.Spec.Compliance.AuditLogs.Destination.ForcePathStyle = true
		return c
	}
	spec := func() batchv1.CronJobSpec { return complianceCheckCronJobSpec(cluster(), "registry.example/spilo:1") }
	podOf := func(s batchv1.CronJobSpec) corev1.PodSpec { return s.JobTemplate.Spec.Template.Spec }

	It("runs every ten minutes, one at a time, and keeps the last few runs, whose pods hold the reports", func() {
		s := spec()
		Expect(s.Schedule).To(Equal("*/10 * * * *"))
		Expect(s.ConcurrencyPolicy).To(Equal(batchv1.ForbidConcurrent))
		Expect(*s.SuccessfulJobsHistoryLimit).To(BeNumerically(">=", 2))
		Expect(*s.FailedJobsHistoryLimit).To(BeNumerically(">=", 1))
		Expect(*s.JobTemplate.Spec.BackoffLimit).To(BeZero(), "never retried: the next run is ten minutes away")
		Expect(*s.JobTemplate.Spec.ActiveDeadlineSeconds).To(BeNumerically(">", 0))
	})

	It("is one of the cluster's own jobs, so the network policy admits it", func() {
		c := cluster()
		labels := spec().JobTemplate.Spec.Template.Labels
		Expect(labels[labelAppComponent]).To(Equal(componentComplianceCheck))
		policy, err := desiredNetworkPolicy(c, testOperator, nil)
		Expect(err).NotTo(HaveOccurred())
		jobs := policy.Spec.Ingress[2].From[0].PodSelector
		Expect(matches(jobs, labels)).To(BeTrue())
	})

	It("runs the cluster's image as the database user with the strictest settings and no API access", func() {
		pod := podOf(spec())
		Expect(pod.Containers[0].Image).To(Equal("registry.example/spilo:1"))
		Expect(*pod.SecurityContext.RunAsNonRoot).To(BeTrue())
		Expect(*pod.SecurityContext.RunAsUser).To(Equal(spiloUID))
		Expect(*pod.Containers[0].SecurityContext.AllowPrivilegeEscalation).To(BeFalse())
		Expect(*pod.AutomountServiceAccountToken).To(BeFalse())
		Expect(pod.Containers[0].Command).To(Equal([]string{"python3", "/scripts/compliance_check.py"}))
	})

	It("reads its script from the compliance scripts ConfigMap, read-only", func() {
		pod := podOf(spec())
		Expect(pod.Volumes[0].ConfigMap.Name).To(Equal("orders-compliance-scripts"))
		Expect(pod.Containers[0].VolumeMounts[0].ReadOnly).To(BeTrue())
	})

	It("reaches the database through the read-write Service over TLS, as the monitor role, with the Secret's password", func() {
		env := envByName(podOf(spec()).Containers[0].Env)
		Expect(env["PGHOST"].Value).To(Equal(sourceName))
		Expect(env["PGSSLMODE"].Value).To(Equal("require"))
		Expect(env["PGPASSWORD"].Value).To(BeEmpty())
		Expect(env["PGPASSWORD"].ValueFrom.SecretKeyRef.Name).To(Equal("orders-monitor"))
		Expect(env["PGPASSWORD"].ValueFrom.SecretKeyRef.Key).To(Equal(secretKeyPassword))
		Expect(env["PGUSER"].ValueFrom.SecretKeyRef.Name).To(Equal("orders-monitor"))
		Expect(env["PGUSER"].ValueFrom.SecretKeyRef.Key).To(Equal(secretKeyUsername))
	})

	It("reads the audit bucket with the audit destination's own credentials and path", func() {
		env := envByName(podOf(spec()).Containers[0].Env)
		Expect(env["AUDIT_BUCKET"].Value).To(Equal(auditBucket))
		Expect(env["AUDIT_PREFIX"].Value).To(Equal("audit/prod/orders"))
		Expect(env["AUDIT_ENDPOINT"].Value).To(Equal("http://minio:9000"))
		Expect(env["AUDIT_FORCE_PATH_STYLE"].Value).To(Equal("true"))
		Expect(env[secretKeyAccessKey].ValueFrom.SecretKeyRef.Name).To(Equal(auditSecret))
		Expect(env[secretKeySecretKey].ValueFrom.SecretKeyRef.Name).To(Equal(auditSecret))
	})
})

var _ = Describe("The network probe CronJob", func() {
	cluster := func() *dbv1alpha1.PostgresCluster {
		c := withCompliance(plainCluster(), dbv1alpha1.StandardHIPAA)
		c.Name, c.Namespace = sourceName, sourceNamespace
		return c
	}
	spec := func() batchv1.CronJobSpec { return networkProbeCronJobSpec(cluster(), "img") }

	It("is a stranger to the network policy: none of the labels it admits, so that it is refused", func() {
		c := cluster()
		labels := spec().JobTemplate.Spec.Template.Labels
		Expect(labels[labelAppComponent]).To(Equal(componentNetworkProbe))
		Expect(labels).NotTo(HaveKey(labelClient))
		policy, err := desiredNetworkPolicy(c, testOperator, nil)
		Expect(err).NotTo(HaveOccurred())
		for _, rule := range policy.Spec.Ingress {
			for _, peer := range rule.From {
				if peer.NamespaceSelector != nil {
					continue
				}
				Expect(matches(peer.PodSelector, labels)).To(BeFalse(), "the probe must not match %v", peer.PodSelector)
			}
		}
	})

	It("is nevertheless matched by the cluster's selectors where it must not be mistaken for a database pod", func() {
		c := cluster()
		labels := spec().JobTemplate.Spec.Template.Labels
		Expect(matches(&metav1.LabelSelector{MatchLabels: podSelectorLabels(c)}, labels)).To(BeFalse())
	})

	It("aims at the read-write Service's port, with a short wait", func() {
		env := envByName(spec().JobTemplate.Spec.Template.Spec.Containers[0].Env)
		Expect(env["TARGET_HOST"].Value).To(Equal(sourceName))
		Expect(env["TARGET_PORT"].Value).To(Equal("5432"))
		Expect(env["CONNECT_TIMEOUT"].Value).To(Equal("5"))
	})

	It("holds no credentials at all, since it never gets in", func() {
		for _, e := range spec().JobTemplate.Spec.Template.Spec.Containers[0].Env {
			Expect(e.ValueFrom).To(BeNil(), e.Name)
		}
	})

	It("runs on the same schedule as the check, as the database user, strictly, once", func() {
		s := spec()
		Expect(s.Schedule).To(Equal(complianceCheckSchedule))
		Expect(*s.JobTemplate.Spec.BackoffLimit).To(BeZero())
		pod := s.JobTemplate.Spec.Template.Spec
		Expect(*pod.SecurityContext.RunAsNonRoot).To(BeTrue())
		Expect(*pod.Containers[0].SecurityContext.AllowPrivilegeEscalation).To(BeFalse())
		Expect(pod.Containers[0].Command).To(Equal([]string{"python3", "/scripts/network_probe.py"}))
	})
})

var _ = Describe("The compliance pods' reports wake the reconciler", func() {
	for _, component := range []string{componentComplianceCheck, componentNetworkProbe} {
		It("maps a "+component+" pod to its cluster", func() {
			requests := clusterForObject(context.Background(), &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Namespace: sourceNamespace,
				Labels:    map[string]string{labelAppName: appNameValue, labelAppInstance: sourceName, labelAppComponent: component},
			}})
			Expect(requests).To(HaveLen(1))
			Expect(requests[0].Name).To(Equal(sourceName))
		})
	}
})

// The scripts themselves, run for real against stand-ins for psql and boto3.
var _ = Describe("The compliance check script", func() {
	const (
		fakePsql = `#!/bin/sh
if [ -n "$FAKE_PSQL_SLEEP" ]; then sleep "$FAKE_PSQL_SLEEP"; fi
if [ -n "$FAKE_PSQL_ERR" ]; then echo "$FAKE_PSQL_ERR" >&2; exit "${FAKE_PSQL_EXIT:-2}"; fi
cat "$FAKE_PSQL_OUT"
`
		fakeBoto3 = `import json, os
from datetime import datetime


class ClientError(Exception):
    def __init__(self, code):
        super().__init__(code)
        self.response = {"Error": {"Code": code}}


def client(service, **kwargs):
    with open(os.path.join(os.environ["FAKE_DIR"], "client.json"), "w") as handle:
        json.dump({"endpoint_url": kwargs.get("endpoint_url"), "region_name": kwargs.get("region_name"),
                   "config": kwargs["config"].kwargs}, handle)
    return Client()


class Client:
    def _objects(self):
        with open(os.path.join(os.environ["FAKE_DIR"], "objects.json")) as handle:
            return [(o["Key"], datetime.fromisoformat(o["LastModified"])) for o in json.load(handle)]

    def list_objects_v2(self, Bucket, Prefix="", Delimiter=None, ContinuationToken=None):
        if os.environ.get("FAKE_LIST_FAILS"):
            raise ClientError(os.environ["FAKE_LIST_FAILS"])
        entries = {}
        for key, when in sorted(self._objects()):
            if not key.startswith(Prefix):
                continue
            rest = key[len(Prefix):]
            if Delimiter and Delimiter in rest:
                entries[("p", Prefix + rest.split(Delimiter)[0] + Delimiter)] = None
            else:
                entries[("o", key)] = when
        ordered = sorted(entries.items(), key=lambda item: item[0][1])
        page = int(os.environ.get("FAKE_PAGE", "1000"))
        start = int(ContinuationToken or 0)
        chunk = ordered[start:start + page]
        out = {"CommonPrefixes": [{"Prefix": k[1]} for k, _ in chunk if k[0] == "p"],
               "Contents": [{"Key": k[1], "LastModified": v} for k, v in chunk if k[0] == "o"]}
        if start + page < len(ordered):
            out["IsTruncated"] = True
            out["NextContinuationToken"] = str(start + page)
        return out

    def _config(self, name):
        path = os.path.join(os.environ["FAKE_DIR"], name)
        if not os.path.exists(path):
            raise ClientError("NotImplemented")
        text = open(path).read()
        if text.startswith("error:"):
            raise ClientError(text[6:].strip())
        return json.loads(text)

    def get_bucket_lifecycle_configuration(self, Bucket):
        return self._config("lifecycle.json")

    def get_object_lock_configuration(self, Bucket):
        return self._config("lock.json")
`
		fakeConfig = `class Config:
    def __init__(self, **kwargs):
        self.kwargs = kwargs
`
		prefix = "audit/prod/orders"
	)
	hba := func(rows ...string) string {
		return "[" + strings.Join(rows, ",") + "]"
	}
	// rule is a pg_hba_file_rules row. A host rule carries a mask, which for a
	// single address is all ones; "all" and a local rule have none.
	ruleFor := func(typ, auth, db, user, addr string, mask ...string) string {
		netmask := "null"
		if len(mask) > 0 {
			netmask = fmt.Sprintf("%q", mask[0])
		}
		address := "null"
		if addr != "" {
			address = fmt.Sprintf("%q", addr)
		}
		return fmt.Sprintf(`{"type":%q,"auth":%q,"db":[%q],"user":[%q],"addr":%s,"mask":%s}`, typ, auth, db, user, address, netmask)
	}
	// rule is a rule for every database and user; replication is the pair for
	// the replication connection.
	rule := func(typ, auth, user, addr string, mask ...string) string {
		return ruleFor(typ, auth, "all", user, addr, mask...)
	}
	replication := func(typ, auth, addr string) string { return ruleFor(typ, auth, "replication", "replicator", addr) }
	const (
		v4Single = "255.255.255.255"
		v6Single = "ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff"
	)
	// The rules of a real cluster built from the Spilo image, as
	// pg_hba_file_rules shows them: read from one on Kind.
	spiloRules := []string{
		rule("local", "trust", "all", ""),
		rule("hostssl", "pam", "+zalandos", "127.0.0.1", v4Single),
		rule("host", "md5", "all", "127.0.0.1", v4Single),
		rule("hostssl", "pam", "+zalandos", "::1", v6Single),
		rule("host", "md5", "all", "::1", v6Single),
		replication("local", "trust", ""),
		replication("hostssl", "md5", "all"),
		rule("hostnossl", "reject", "all", "all"),
		rule("hostssl", "pam", "+zalandos", "all"),
		rule("hostssl", "md5", "all", "all"),
	}
	psqlJSON := func(settings string, rules []string, hbaErrors, nonScram int) string {
		return fmt.Sprintf(`{"settings":%s,"hba":%s,"hbaErrors":%d,"nonScramRoles":%d}`, settings, hba(rules...), hbaErrors, nonScram)
	}
	goodSettings := `{"shared_preload_libraries":"bg_mon,pgaudit","pgaudit.log":"read,write,ddl,role","log_connections":"on",` +
		`"log_disconnections":"on","ssl":"on","password_encryption":"scram-sha-256","data_checksums":"on"}`

	var dir string

	BeforeEach(func() {
		if _, err := exec.LookPath("python3"); err != nil {
			Skip("python3 is not available")
		}
		dir = GinkgoT().TempDir()
		for path, body := range map[string]string{
			"bin/psql":                  fakePsql,
			"fake/boto3/__init__.py":    fakeBoto3,
			"fake/botocore/__init__.py": "",
			"fake/botocore/config.py":   fakeConfig,
			"psql.json":                 psqlJSON(goodSettings, spiloRules, 0, 0),
			"objects.json":              "[]",
		} {
			full := filepath.Join(dir, path)
			ExpectWithOffset(1, os.MkdirAll(filepath.Dir(full), 0o755)).To(Succeed())  //nolint:gosec // a test directory
			ExpectWithOffset(1, os.WriteFile(full, []byte(body), 0o755)).To(Succeed()) //nolint:gosec // test scripts must be executable
		}
	})

	put := func(name, body string) {
		ExpectWithOffset(1, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644)).To(Succeed()) //nolint:gosec // test files
	}
	objects := func(items ...[2]string) {
		list := make([]map[string]string, 0, len(items))
		for _, item := range items {
			list = append(list, map[string]string{"Key": item[0], "LastModified": item[1]})
		}
		raw, err := json.Marshal(list)
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		put("objects.json", string(raw))
	}
	chunk := func(pod, file, generation string, start int) string {
		return fmt.Sprintf("%s/%s/%s/%s/%012d-%012d.csv.gz", prefix, pod, file, generation, start, start+1000)
	}
	run := func(extra ...string) (map[string]any, string) {
		cmd := exec.Command("python3", "scripts/compliance_check.py")
		cmd.Env = append(os.Environ(),
			"PATH="+filepath.Join(dir, "bin")+":"+os.Getenv("PATH"), "PYTHONPATH="+filepath.Join(dir, "fake"),
			"FAKE_DIR="+dir, "FAKE_PSQL_OUT="+filepath.Join(dir, "psql.json"), "TERMINATION_LOG="+filepath.Join(dir, "out.json"),
			"AUDIT_BUCKET=audit-logs", "AUDIT_PREFIX="+prefix, "COMMAND_TIMEOUT=60")
		cmd.Env = append(cmd.Env, extra...)
		out, err := cmd.CombinedOutput()
		ExpectWithOffset(1, err).NotTo(HaveOccurred(), "it always exits 0 once it has written its report: "+string(out))
		raw, readErr := os.ReadFile(filepath.Join(dir, "out.json"))
		ExpectWithOffset(1, readErr).NotTo(HaveOccurred())
		var report map[string]any
		ExpectWithOffset(1, json.Unmarshal(raw, &report)).To(Succeed())
		return report, string(raw)
	}
	sqlOf := func(r map[string]any) map[string]any { return r["sql"].(map[string]any) }
	bucketOf := func(r map[string]any) map[string]any { return r["bucket"].(map[string]any) }

	Describe("what it reads from the database", func() {
		It("reports the settings and finds nothing wrong with what Spilo configures by default", func() {
			report, _ := run()
			sql := sqlOf(report)
			Expect(sql["ok"]).To(BeTrue())
			Expect(sql["settings"]).To(HaveKeyWithValue("pgaudit.log", "read,write,ddl,role"))
			Expect(sql["plainRuleCount"]).To(BeEquivalentTo(0), "hostnossl ... reject is the rule that refuses plain connections")
			Expect(sql["weakRuleCount"]).To(BeEquivalentTo(0), "local trust is the unix socket in the pod, and md5 negotiates scram")
			Expect(sql["hbaErrors"]).To(BeEquivalentTo(0))
			Expect(sql["nonScramRoles"]).To(BeEquivalentTo(0))
		})

		It("finds a rule that can accept a connection without TLS", func() {
			put("psql.json", psqlJSON(goodSettings, append(spiloRules, rule("host", "md5", "all", "10.0.0.0", "255.0.0.0")), 0, 0))
			sql := sqlOf(func() map[string]any { r, _ := run(); return r }())
			Expect(sql["plainRuleCount"]).To(BeEquivalentTo(1))
			Expect(sql["plainRules"]).To(ConsistOf("host all all 10.0.0.0 md5"))
		})

		It("counts a plain rule that lets anyone in as both a plain rule and a weak one", func() {
			put("psql.json", psqlJSON(goodSettings, append(spiloRules, rule("hostnossl", "trust", "all", "all")), 0, 0))
			report, _ := run()
			Expect(sqlOf(report)["plainRuleCount"]).To(BeEquivalentTo(1))
			Expect(sqlOf(report)["weakRuleCount"]).To(BeEquivalentTo(1))
		})

		DescribeTable("finds a network rule that needs no password, or sends it unprotected",
			func(method string, weak bool) {
				put("psql.json", psqlJSON(goodSettings, append(spiloRules, rule("hostssl", method, "all", "10.0.0.0", "255.0.0.0")), 0, 0))
				report, _ := run()
				if weak {
					Expect(sqlOf(report)["weakRules"]).To(ConsistOf("hostssl all all 10.0.0.0 " + method))
				} else {
					Expect(sqlOf(report)["weakRuleCount"]).To(BeEquivalentTo(0))
				}
			},
			Entry("trust", "trust", true),
			Entry("password, which is sent in clear", "password", true),
			Entry("ident", "ident", true),
			Entry("md5, which negotiates scram", "md5", false),
			Entry("scram-sha-256", "scram-sha-256", false),
			Entry("cert", "cert", false),
			Entry("reject", "reject", false),
		)

		It("ignores a local rule, which is the socket inside the pod", func() {
			put("psql.json", psqlJSON(goodSettings, []string{rule("local", "trust", "all", ""), rule("local", "peer", "all", "")}, 0, 0))
			report, _ := run()
			Expect(sqlOf(report)["weakRuleCount"]).To(BeEquivalentTo(0))
		})

		DescribeTable("ignores a rule that can only match the pod itself, since no connection over loopback crosses a network",
			func(rules ...string) {
				put("psql.json", psqlJSON(goodSettings, append(spiloRules, rules...), 0, 0))
				report, _ := run()
				Expect(sqlOf(report)["plainRuleCount"]).To(BeEquivalentTo(0))
				Expect(sqlOf(report)["weakRuleCount"]).To(BeEquivalentTo(0))
			},
			Entry("a loopback address and a single-address mask, with no TLS needed",
				rule("host", "md5", "all", "127.0.0.1", v4Single)),
			Entry("the IPv6 loopback address", rule("host", "md5", "all", "::1", v6Single)),
			Entry("the whole 127.0.0.0/8 block", rule("host", "md5", "all", "127.0.0.0", "255.0.0.0")),
			Entry("the name localhost", rule("host", "md5", "all", "localhost")),
			Entry("loopback with no password at all", rule("hostssl", "trust", "all", "127.0.0.1", v4Single)),
		)

		DescribeTable("does not ignore a rule that only looks like loopback, or is not",
			func(typ, auth, addr, mask string, plain, weak int) {
				put("psql.json", psqlJSON(goodSettings, append(spiloRules, rule(typ, auth, "all", addr, mask)), 0, 0))
				report, _ := run()
				Expect(sqlOf(report)["plainRuleCount"]).To(BeEquivalentTo(plain))
				Expect(sqlOf(report)["weakRuleCount"]).To(BeEquivalentTo(weak))
			},
			Entry("a loopback address whose mask matches every address", "host", "md5", "127.0.0.1", "0.0.0.0", 1, 0),
			Entry("a loopback address with a mask wider than loopback", "host", "md5", "127.0.0.1", "128.0.0.0", 1, 0),
			Entry("an address next door to loopback", "host", "md5", "128.0.0.1", v4Single, 1, 0),
			Entry("a private network", "host", "md5", "10.0.0.0", "255.0.0.0", 1, 0),
			Entry("a host name", "host", "md5", "db.example.com", "", 1, 0),
			Entry("samehost, which includes the pod's own network address", "host", "md5", "samehost", "", 1, 0),
			Entry("every address, with no password", "hostnossl", "trust", "all", "", 1, 1),
		)

		It("reports roles with an old password hash, and connection rules that could not be read", func() {
			put("psql.json", psqlJSON(goodSettings, spiloRules, 2, 3))
			report, _ := run()
			Expect(sqlOf(report)["hbaErrors"]).To(BeEquivalentTo(2))
			Expect(sqlOf(report)["nonScramRoles"]).To(BeEquivalentTo(3))
		})

		It("lists at most a few rules, but counts them all", func() {
			many := make([]string, 0, 10)
			for i := range 10 {
				many = append(many, rule("hostssl", "trust", "all", fmt.Sprintf("10.0.%d.0", i), "255.255.255.0"))
			}
			put("psql.json", psqlJSON(goodSettings, many, 0, 0))
			report, _ := run()
			Expect(sqlOf(report)["weakRules"]).To(HaveLen(4))
			Expect(sqlOf(report)["weakRuleCount"]).To(BeEquivalentTo(10))
		})

		It("says why when psql fails, and still reports the bucket", func() {
			report, _ := run("FAKE_PSQL_ERR=connection refused")
			Expect(sqlOf(report)["ok"]).To(BeFalse())
			Expect(sqlOf(report)["error"]).To(ContainSubstring("connection refused"))
			Expect(bucketOf(report)["ok"]).To(BeTrue())
		})

		It("says so, instead of hanging, when psql does not answer", func() {
			report, _ := run("FAKE_PSQL_SLEEP=5", "COMMAND_TIMEOUT=1")
			Expect(sqlOf(report)["ok"]).To(BeFalse())
			Expect(sqlOf(report)["error"]).To(ContainSubstring("timed out"))
		})

		It("says so when psql's answer is not what it expected", func() {
			put("psql.json", "this is not json")
			report, _ := run()
			Expect(sqlOf(report)["error"]).To(ContainSubstring("unexpected psql output"))
		})

		It("asks the database for exactly the settings the controls use", func() {
			script, err := os.ReadFile("scripts/compliance_check.py")
			Expect(err).NotTo(HaveOccurred())
			for _, setting := range []string{"shared_preload_libraries", "pgaudit.log", settingLogConnections, "log_disconnections", "ssl",
				"password_encryption", "data_checksums", "pg_hba_file_rules", "pgoperator.non_scram_roles()",
				"netmask"} { // the mask is what tells a loopback-only rule from one that only starts at 127.0.0.1
				Expect(string(script)).To(ContainSubstring(setting))
			}
		})

		It("never reads pg_authid itself: the monitor role cannot, and password hashes stay out of reach", func() {
			script, err := os.ReadFile("scripts/compliance_check.py")
			Expect(err).NotTo(HaveOccurred())
			Expect(string(script)).NotTo(ContainSubstring("pg_authid"))
			Expect(string(script)).NotTo(ContainSubstring("pg_shadow"))
		})
	})

	Describe("what it reads from the audit bucket", func() {
		It("finds when each pod last shipped: the newest chunk of the newest generation of its files", func() {
			objects(
				[2]string{chunk("orders-0", "postgresql-3.csv", "20261013T000000000", 0), "2026-10-19T08:00:00+00:00"},
				[2]string{chunk("orders-0", "postgresql-7.csv", "20261018T000000000", 0), "2026-10-19T11:40:00+00:00"},
				[2]string{chunk("orders-0", "postgresql-7.csv", "20261018T000000000", 1000), "2026-10-19T11:55:00+00:00"},
				[2]string{chunk("orders-1", "postgresql-7.csv", "20261018T000000000", 0), "2026-10-19T11:57:30+00:00"},
			)
			report, _ := run()
			pods := bucketOf(report)["pods"].(map[string]any)
			Expect(pods).To(HaveLen(2))
			Expect(pods["orders-0"]).To(HaveKeyWithValue("newest", "2026-10-19T11:55:00Z"))
			Expect(pods["orders-0"]).To(HaveKeyWithValue("chunks", BeEquivalentTo(3)))
			Expect(pods["orders-1"]).To(HaveKeyWithValue("newest", "2026-10-19T11:57:30Z"))
		})

		It("reads only the newest generation of a file, which is where new chunks go", func() {
			objects(
				[2]string{chunk("orders-0", "postgresql-7.csv", "20261011T000000000", 0), "2026-10-19T12:30:00+00:00"},
				[2]string{chunk("orders-0", "postgresql-7.csv", "20261018T000000000", 0), elevenAM},
			)
			report, _ := run()
			pods := bucketOf(report)["pods"].(map[string]any)
			Expect(pods["orders-0"]).To(HaveKeyWithValue("newest", "2026-10-19T11:00:00Z"))
		})

		It("reports a pod directory with nothing in it as having shipped nothing", func() {
			objects([2]string{prefix + "/orders-2/", elevenAM})
			report, _ := run()
			Expect(bucketOf(report)["pods"].(map[string]any)["orders-2"]).To(HaveKeyWithValue("newest", BeNil()))
		})

		It("reports no pods for an empty prefix, and that is not an error", func() {
			report, _ := run()
			Expect(bucketOf(report)["ok"]).To(BeTrue())
			Expect(bucketOf(report)["pods"]).To(BeEmpty())
		})

		It("reads every page of a listing", func() {
			items := make([][2]string, 0, 6)
			for i := range 6 {
				items = append(items, [2]string{chunk(fmt.Sprintf("orders-%d", i), "postgresql-7.csv", "20261018T000000000", 0), elevenAM})
			}
			objects(items...)
			report, _ := run("FAKE_PAGE=2")
			Expect(bucketOf(report)["pods"]).To(HaveLen(6))
		})

		It("looks only under the cluster's own path", func() {
			objects(
				[2]string{chunk("orders-0", "postgresql-7.csv", "20261018T000000000", 0), elevenAM},
				[2]string{"audit/prod/billing/billing-0/postgresql-7.csv/20261018T000000000/000000000000-000000001000.csv.gz", "2026-10-19T11:30:00+00:00"},
				[2]string{"prod/orders/wal/17/something", "2026-10-19T11:45:00+00:00"},
			)
			report, _ := run()
			Expect(bucketOf(report)["pods"]).To(HaveLen(1))
		})

		It("says why when the bucket cannot be read, and still reports the database", func() {
			report, _ := run("FAKE_LIST_FAILS=AccessDenied")
			Expect(bucketOf(report)["ok"]).To(BeFalse())
			Expect(bucketOf(report)["error"]).To(ContainSubstring("AccessDenied"))
			Expect(sqlOf(report)["ok"]).To(BeTrue())
		})

		It("says so when it was not told which bucket", func() {
			report, _ := run("AUDIT_BUCKET=")
			Expect(bucketOf(report)["error"]).To(ContainSubstring("AUDIT_BUCKET is not set"))
		})

		It("addresses the store the way the destination says", func() {
			run("AUDIT_ENDPOINT=http://minio:9000", "AUDIT_FORCE_PATH_STYLE=true")
			raw, err := os.ReadFile(filepath.Join(dir, "client.json"))
			Expect(err).NotTo(HaveOccurred())
			Expect(string(raw)).To(And(ContainSubstring(`"http://minio:9000"`), ContainSubstring(`"addressing_style": "path"`), ContainSubstring("us-east-1")))
		})
	})

	Describe("what it reads of retention", func() {
		lifecycle := func() map[string]any { r, _ := run(); return bucketOf(r)["lifecycle"].(map[string]any) }
		lock := func() map[string]any { r, _ := run(); return bucketOf(r)["objectLock"].(map[string]any) }

		It("reports rules in whichever of the three S3 forms the prefix is written", func() {
			put("lifecycle.json", `{"Rules":[
				{"ID":"a","Status":"Enabled","Filter":{"Prefix":"audit/"},"Expiration":{"Days":30}},
				{"ID":"b","Status":"Enabled","Filter":{"And":{"Prefix":"audit/prod/","Tags":[{"Key":"k","Value":"v"}]}},"Expiration":{"Days":60}},
				{"ID":"c","Status":"Disabled","Prefix":"tmp/","Expiration":{"Days":1}},
				{"ID":"d","Status":"Enabled","Filter":{},"Expiration":{"Days":90}},
				{"ID":"e","Status":"Enabled","Filter":{"Prefix":"x/"},"Expiration":{"ExpiredObjectDeleteMarker":true}}]}`)
			got := lifecycle()
			Expect(got["supported"]).To(BeTrue())
			Expect(got["rules"]).To(Equal([]any{
				map[string]any{"id": "a", keyEnabled: true, keyPrefix: "audit/", keyDays: float64(30)},
				map[string]any{"id": "b", keyEnabled: true, keyPrefix: "audit/prod/", keyDays: float64(60)},
				map[string]any{"id": "c", keyEnabled: false, keyPrefix: "tmp/", keyDays: float64(1)},
				map[string]any{"id": "d", keyEnabled: true, keyPrefix: "", keyDays: float64(90)},
				map[string]any{"id": "e", keyEnabled: true, keyPrefix: "x/", keyDays: float64(0)},
			}))
		})

		It("reports no lifecycle configuration as no rules, which the store supports", func() {
			put("lifecycle.json", "error:"+noSuchLifecycle)
			got := lifecycle()
			Expect(got["supported"]).To(BeTrue())
			Expect(got["rules"]).To(BeEmpty())
		})

		It("reports a store that cannot say, with the code it gave", func() {
			put("lifecycle.json", "error:NotImplemented")
			got := lifecycle()
			Expect(got["supported"]).To(BeFalse())
			Expect(got["error"]).To(Equal("NotImplemented"))
		})

		It("reports an object lock's default retention in days, whether it is written in days or years", func() {
			put("lock.json", `{"ObjectLockConfiguration":{"ObjectLockEnabled":"Enabled","Rule":{"DefaultRetention":{"Mode":"COMPLIANCE","Days":90}}}}`)
			Expect(lock()).To(Equal(map[string]any{"supported": true, keyDays: float64(90), "mode": "COMPLIANCE"}))
			put("lock.json", `{"ObjectLockConfiguration":{"Rule":{"DefaultRetention":{"Mode":"GOVERNANCE","Years":6}}}}`)
			Expect(lock()[keyDays]).To(BeEquivalentTo(2190))
		})

		It("reports a bucket with no object lock as supported with no retention", func() {
			put("lock.json", "error:ObjectLockConfigurationNotFoundError")
			Expect(lock()).To(Equal(map[string]any{"supported": true, keyDays: float64(0)}))
		})

		It("reports a store without object lock as unable to say", func() {
			got := lock()
			Expect(got["supported"]).To(BeFalse())
		})
	})

	It("keeps its report inside the 4 KiB a termination message can hold, however much there is to say", func() {
		many := make([]string, 0, 80)
		for i := range 40 {
			many = append(many, rule("hostssl", "trust", "all", fmt.Sprintf("10.%d.0.0", i), "255.255.0.0"))
			many = append(many, rule("host", "md5", "all", fmt.Sprintf("172.%d.0.0", i), "255.255.0.0"))
		}
		put("psql.json", psqlJSON(goodSettings, many, 3, 5))
		items := make([][2]string, 0, 20)
		for i := range 20 {
			items = append(items, [2]string{chunk(fmt.Sprintf("orders-%d", i), "postgresql-7.csv", "20261018T000000000", 0), elevenAM})
		}
		objects(items...)
		rules := make([]string, 0, 40)
		for i := range 40 {
			rules = append(rules, fmt.Sprintf(`{"ID":"rule-with-a-fairly-long-identifier-%d","Status":"Enabled","Filter":{"Prefix":"p%d/"},"Expiration":{"Days":%d}}`, i, i, i+1))
		}
		put("lifecycle.json", `{"Rules":[`+strings.Join(rules, ",")+`]}`)
		_, raw := run()
		Expect(len(raw)).To(BeNumerically("<", 4096), "it was %d bytes", len(raw))
	})
})

var _ = Describe("The network probe script", func() {
	run := func(extra ...string) map[string]any {
		if _, err := exec.LookPath("python3"); err != nil {
			Skip("python3 is not available")
		}
		out := filepath.Join(GinkgoT().TempDir(), "out.json")
		cmd := exec.Command("python3", "scripts/network_probe.py")
		cmd.Env = append(os.Environ(), "TERMINATION_LOG="+out, "CONNECT_TIMEOUT=2")
		cmd.Env = append(cmd.Env, extra...)
		output, err := cmd.CombinedOutput()
		ExpectWithOffset(1, err).NotTo(HaveOccurred(), string(output))
		raw, readErr := os.ReadFile(out)
		ExpectWithOffset(1, readErr).NotTo(HaveOccurred())
		var report map[string]any
		ExpectWithOffset(1, json.Unmarshal(raw, &report)).To(Succeed())
		return report
	}

	It("says open when something accepts the connection", func() {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())
		defer listener.Close() //nolint:errcheck // a test listener
		port := listener.Addr().(*net.TCPAddr).Port
		report := run("TARGET_HOST=127.0.0.1", fmt.Sprintf("TARGET_PORT=%d", port))
		Expect(report["result"]).To(Equal("open"))
	})

	It("says refused when nothing is listening", func() {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())
		port := listener.Addr().(*net.TCPAddr).Port
		Expect(listener.Close()).To(Succeed())
		report := run("TARGET_HOST=127.0.0.1", fmt.Sprintf("TARGET_PORT=%d", port))
		Expect(report["result"]).To(Equal("refused"))
	})

	It("never says open for an address that does not answer", func() {
		start := time.Now()
		report := run("TARGET_HOST=10.255.255.1", "TARGET_PORT=5432", "CONNECT_TIMEOUT=0.5")
		Expect(report["result"]).To(BeElementOf("timeout", "unreachable", "error"))
		Expect(time.Since(start)).To(BeNumerically("<", 10*time.Second), "it gives up after its timeout")
	})

	It("says error, with the reason, for a name that does not resolve", func() {
		report := run("TARGET_HOST=nonexistent.invalid")
		Expect(report["result"]).To(Equal("error"))
		Expect(report["detail"]).To(ContainSubstring("could not resolve"))
	})

	It("says error when it was not told where to connect", func() {
		report := run()
		Expect(report["result"]).To(Equal("error"))
		Expect(report["detail"]).To(ContainSubstring("TARGET_HOST is not set"))
	})
})

var _ = Describe("The compliance status through the reconciler", func() {
	const namespace = "default"
	ctx := context.Background()
	key := func(name string) types.NamespacedName { return types.NamespacedName{Name: name, Namespace: namespace} }
	reconcileOnce := func(name string) {
		rec := &PostgresClusterReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Patroni: &fakePatroni{live: map[string]any{}}, Operator: testOperator,
		}
		_, err := rec.Reconcile(ctx, reconcile.Request{NamespacedName: key(name)})
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
	}
	create := func(name string, compliance bool) {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: auditSecret, Namespace: namespace},
			StringData: map[string]string{secretKeyAccessKey: "id", secretKeySecretKey: testSecretValue},
		}
		if err := k8sClient.Create(ctx, secret); err == nil {
			DeferCleanup(func() { ExpectWithOffset(1, client.IgnoreNotFound(k8sClient.Delete(ctx, secret))).To(Succeed()) })
		}
		c := &dbv1alpha1.PostgresCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: dbv1alpha1.PostgresClusterSpec{
				Version: "17", Instances: 3,
				Storage:   dbv1alpha1.StorageSpec{Size: resource.MustParse("1Gi")},
				Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}},
			},
		}
		if compliance {
			withCompliance(c, dbv1alpha1.StandardHIPAA)
		}
		ExpectWithOffset(1, k8sClient.Create(ctx, c)).To(Succeed())
		DeferCleanup(func() { ExpectWithOffset(1, client.IgnoreNotFound(k8sClient.Delete(ctx, c))).To(Succeed()) })
	}
	get := func(name string) *dbv1alpha1.PostgresCluster {
		c := &dbv1alpha1.PostgresCluster{}
		ExpectWithOffset(1, k8sClient.Get(ctx, key(name), c)).To(Succeed())
		return c
	}
	// finished adds a pod that has already finished, with a termination message.
	finished := func(name, component, message string, at time.Time) {
		cluster := &dbv1alpha1.PostgresCluster{ObjectMeta: metav1.ObjectMeta{Name: name}}
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-%s-%d", name, component, at.UnixNano()), Namespace: namespace,
				Labels: labelsWithComponent(cluster, component)},
			Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever, Containers: []corev1.Container{{Name: "c", Image: testPodImage}}},
		}
		ExpectWithOffset(1, k8sClient.Create(ctx, pod)).To(Succeed())
		DeferCleanup(func() {
			ExpectWithOffset(1, client.IgnoreNotFound(k8sClient.Delete(ctx, pod, client.GracePeriodSeconds(0)))).To(Succeed())
		})
		pod.Status.Phase = corev1.PodSucceeded
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name: "c", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: message, FinishedAt: metav1.NewTime(at)}},
		}}
		ExpectWithOffset(1, k8sClient.Status().Update(ctx, pod)).To(Succeed())
	}
	healthyMessage := func() string {
		return `{"sql":{"ok":true,"settings":{"shared_preload_libraries":"bg_mon,pgaudit","pgaudit.log":"read,write,ddl,role",` +
			`"log_connections":"on","log_disconnections":"on","ssl":"on","password_encryption":"scram-sha-256","data_checksums":"on"},` +
			`"plainRules":[],"plainRuleCount":0,"weakRules":[],"weakRuleCount":0,"hbaErrors":0,"nonScramRoles":0},` +
			`"bucket":{"ok":true,"pods":{},"lifecycle":{"supported":true,"rules":[]},"objectLock":{"supported":true,"days":0}}}`
	}
	control := func(c *dbv1alpha1.PostgresCluster, id string) dbv1alpha1.ComplianceControl {
		GinkgoHelper()
		for _, ctl := range c.Status.Compliance.Controls {
			if ctl.ID == id {
				return ctl
			}
		}
		Fail("no control " + id)
		return dbv1alpha1.ComplianceControl{}
	}

	It("creates the check, the probe and their scripts for a cluster with compliance settings, and removes them with the settings", func() {
		const name = "cc-lifecycle"
		create(name, true)
		reconcileOnce(name)
		for _, job := range []string{name + "-compliance-check", name + "-network-probe"} {
			cj := &batchv1.CronJob{}
			Expect(k8sClient.Get(ctx, key(job), cj)).To(Succeed(), job)
			Expect(cj.OwnerReferences).NotTo(BeEmpty(), job+" goes with the cluster")
		}
		cm := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, key(name+"-compliance-scripts"), cm)).To(Succeed())
		Expect(cm.Data).To(And(HaveKeyWithValue("audit_shipper.py", auditShipperScript),
			HaveKeyWithValue("compliance_check.py", complianceCheckScript), HaveKeyWithValue("network_probe.py", networkProbeScript)))

		c := get(name)
		c.Spec.Compliance = nil
		Expect(k8sClient.Update(ctx, c)).To(Succeed())
		reconcileOnce(name)
		for _, job := range []string{name + "-compliance-check", name + "-network-probe"} {
			Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key(job), &batchv1.CronJob{}))).To(BeTrue(), job)
		}
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key(name+"-compliance-scripts"), &corev1.ConfigMap{}))).To(BeTrue())
	})

	It("creates neither for a cluster without compliance settings", func() {
		const name = "cc-none"
		create(name, false)
		reconcileOnce(name)
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key(name+"-compliance-check"), &batchv1.CronJob{}))).To(BeTrue())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key(name+"-network-probe"), &batchv1.CronJob{}))).To(BeTrue())
		Expect(get(name).Status.Compliance.Controls).To(BeEmpty())
		Expect(meta.FindStatusCondition(get(name).Status.Conditions, "ControlsMet")).To(BeNil())
	})

	It("reports every control Unknown, and ControlsMet Unknown, before either job has run", func() {
		const name = "cc-fresh"
		create(name, true)
		reconcileOnce(name)
		c := get(name)
		Expect(c.Status.Compliance.Standard).To(Equal("hipaa"))
		Expect(c.Status.Compliance.LastChecked).To(BeNil())
		Expect(c.Status.Compliance.Controls).To(HaveLen(10))
		cond := meta.FindStatusCondition(c.Status.Conditions, "ControlsMet")
		Expect(cond.Status).NotTo(Equal(metav1.ConditionTrue), "nothing has been shown yet")
		Expect(control(c, dbv1alpha1.ControlAuditLogging).Status).To(Equal(dbv1alpha1.ControlUnknown))
		Expect(control(c, dbv1alpha1.ControlAuditLogging).Evidence).To(ContainSubstring("waiting for the first compliance check"))
	})

	It("works out the controls from what the two jobs reported, and says which are not met", func() {
		const name = "cc-report"
		create(name, true)
		reconcileOnce(name)
		at := time.Now().Add(-2 * time.Minute).Truncate(time.Second)
		finished(name, componentComplianceCheck, healthyMessage(), at)
		finished(name, componentNetworkProbe, `{"result":"timeout"}`, at)
		reconcileOnce(name)

		c := get(name)
		for _, id := range []string{
			dbv1alpha1.ControlAuditLogging, dbv1alpha1.ControlConnectionLogging, dbv1alpha1.ControlEncryptionInTransit,
			dbv1alpha1.ControlAuthentication, dbv1alpha1.ControlDataIntegrity, dbv1alpha1.ControlAuditLogRetention,
		} {
			Expect(control(c, id).Status).To(Equal(dbv1alpha1.ControlMet), "%s: %s", id, control(c, id).Evidence)
		}
		Expect(control(c, dbv1alpha1.ControlNetworkIsolation).Status).To(Equal(dbv1alpha1.ControlMet),
			"the policy the reconcile just wrote is the one it expects, and a stranger was refused")
		Expect(control(c, dbv1alpha1.ControlBackupRecovery).Status).To(Equal(dbv1alpha1.ControlNotMet), "this cluster takes no backups")
		Expect(control(c, dbv1alpha1.ControlAvailability).Status).To(Equal(dbv1alpha1.ControlNotMet), "there is no primary in envtest")
		Expect(control(c, dbv1alpha1.ControlAuditLogDelivery).Status).To(Equal(dbv1alpha1.ControlUnknown), "there are no database pods to ship from")
		Expect(c.Status.Compliance.LastChecked.Time).To(BeTemporally("~", at, time.Second))
		Expect(control(c, dbv1alpha1.ControlAuditLogging).ObservedAt.Time).To(BeTemporally("~", at, time.Second))

		cond := meta.FindStatusCondition(c.Status.Conditions, "ControlsMet")
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Message).To(And(ContainSubstring("backup-recovery"), ContainSubstring("availability"),
			ContainSubstring("1 not yet known: audit-log-delivery")))
	})

	It("publishes how many controls are in each state, and drops the series with the compliance settings", func() {
		const name = "cc-metric"
		create(name, true)
		reconcileOnce(name)
		finished(name, componentComplianceCheck, healthyMessage(), time.Now().Add(-time.Minute))
		finished(name, componentNetworkProbe, `{"result":"refused"}`, time.Now().Add(-time.Minute))
		reconcileOnce(name)
		gauge := func(status string) float64 {
			return testutil.ToFloat64(complianceControls.WithLabelValues(namespace, name, status))
		}
		Expect(gauge("Met")).To(BeEquivalentTo(7))
		Expect(gauge("NotMet")).To(BeEquivalentTo(2))
		Expect(gauge("Unknown")).To(BeEquivalentTo(1))

		c := get(name)
		c.Spec.Compliance = nil
		Expect(k8sClient.Update(ctx, c)).To(Succeed())
		reconcileOnce(name)
		Expect(complianceControls.DeletePartialMatch(map[string]string{labelNamespace: namespace, labelName: name})).To(BeZero(),
			"nothing left to delete: the series went with the settings")
		after := get(name)
		Expect(after.Status.Compliance.Controls).To(BeEmpty())
		Expect(meta.FindStatusCondition(after.Status.Conditions, "ControlsMet")).To(BeNil())
	})

	It("stops believing a report that is too old", func() {
		const name = "cc-stale"
		create(name, true)
		reconcileOnce(name)
		old := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
		finished(name, componentComplianceCheck, healthyMessage(), old)
		finished(name, componentNetworkProbe, `{"result":"timeout"}`, old)
		reconcileOnce(name)
		c := get(name)
		Expect(control(c, dbv1alpha1.ControlAuditLogging).Status).To(Equal(dbv1alpha1.ControlUnknown))
		Expect(control(c, dbv1alpha1.ControlAuditLogging).Evidence).To(ContainSubstring("has not reported since"))
		Expect(control(c, dbv1alpha1.ControlNetworkIsolation).Status).To(Equal(dbv1alpha1.ControlUnknown))
	})

	It("says a control is not met when a stranger got through the policy", func() {
		const name = "cc-open"
		create(name, true)
		reconcileOnce(name)
		finished(name, componentComplianceCheck, healthyMessage(), time.Now().Add(-time.Minute))
		finished(name, componentNetworkProbe, `{"result":"open"}`, time.Now().Add(-time.Minute))
		reconcileOnce(name)
		c := get(name)
		Expect(control(c, dbv1alpha1.ControlNetworkIsolation).Status).To(Equal(dbv1alpha1.ControlNotMet))
	})

	It("notices a policy that has been changed by hand, and the next reconcile puts it back", func() {
		const name = "cc-tampered"
		create(name, true)
		reconcileOnce(name)
		rec := &PostgresClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Operator: testOperator}

		state, err := rec.policyState(ctx, get(name))
		Expect(err).NotTo(HaveOccurred())
		Expect(state).To(Equal(policyState{Found: true, Matches: true}))

		policy := &networkingv1.NetworkPolicy{}
		Expect(k8sClient.Get(ctx, key(name), policy)).To(Succeed())
		policy.Spec.Ingress = policy.Spec.Ingress[:1]
		Expect(k8sClient.Update(ctx, policy)).To(Succeed())
		state, err = rec.policyState(ctx, get(name))
		Expect(err).NotTo(HaveOccurred())
		Expect(state).To(Equal(policyState{Found: true, Matches: false}), "a rule has been taken out")

		reconcileOnce(name)
		state, err = rec.policyState(ctx, get(name))
		Expect(err).NotTo(HaveOccurred())
		Expect(state.Matches).To(BeTrue())

		Expect(k8sClient.Delete(ctx, policy)).To(Succeed())
		state, err = rec.policyState(ctx, get(name))
		Expect(err).NotTo(HaveOccurred())
		Expect(state).To(Equal(policyState{}), "and when it is gone altogether")
	})

	It("tolerates a report it cannot read, as Unknown rather than as an error", func() {
		const name = "cc-garbage"
		create(name, true)
		reconcileOnce(name)
		finished(name, componentComplianceCheck, "this is not json", time.Now().Add(-time.Minute))
		reconcileOnce(name)
		Expect(control(get(name), dbv1alpha1.ControlAuditLogging).Evidence).To(ContainSubstring("could not be read"))
	})
})

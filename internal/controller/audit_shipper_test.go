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
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

func envByName(env []corev1.EnvVar) map[string]corev1.EnvVar {
	out := map[string]corev1.EnvVar{}
	for _, e := range env {
		out[e.Name] = e
	}
	return out
}

var _ = Describe("The audit shipper sidecar", func() {
	hipaa := func() *dbv1alpha1.PostgresCluster {
		c := withCompliance(plainCluster(), dbv1alpha1.StandardHIPAA)
		c.Namespace = "prod"
		c.Name = "orders"
		c.Spec.Compliance.AuditLogs.Destination.Endpoint = "http://minio:9000"
		c.Spec.Compliance.AuditLogs.Destination.ForcePathStyle = true
		return c
	}
	container := func(c *dbv1alpha1.PostgresCluster) corev1.Container {
		return auditShipperContainer(c, "registry.example/spilo:1")
	}

	It("runs the cluster's own image", func() {
		Expect(container(hipaa()).Image).To(Equal("registry.example/spilo:1"))
	})

	It("is told where to ship, under a path of its own that does not nest with the backups", func() {
		env := envByName(container(hipaa()).Env)
		Expect(env["AUDIT_BUCKET"].Value).To(Equal(auditBucket))
		Expect(env["AUDIT_PREFIX"].Value).To(Equal("audit/prod/orders"))
		Expect(env["AUDIT_ENDPOINT"].Value).To(Equal("http://minio:9000"))
		Expect(env["AUDIT_FORCE_PATH_STYLE"].Value).To(Equal("true"))
		Expect(env["AUDIT_REGION"].Value).To(Equal(testRegion))
		Expect(env["LOG_DIR"].Value).To(Equal("/home/postgres/pgdata/pgroot/pg_log"))
	})

	It("uses the destination's own path when it sets one", func() {
		c := hipaa()
		c.Spec.Compliance.AuditLogs.Destination.Path = "/team/audit/"
		Expect(envByName(container(c).Env)["AUDIT_PREFIX"].Value).To(Equal("team/audit"))
	})

	It("names each pod's uploads by the pod, so replicas never write to the same place", func() {
		pod := envByName(container(hipaa()).Env)["POD_NAME"]
		Expect(pod.ValueFrom.FieldRef.FieldPath).To(Equal("metadata.name"))
	})

	It("takes its credentials from the Secret the spec names, never as plain values", func() {
		env := envByName(container(hipaa()).Env)
		for _, key := range []string{secretKeyAccessKey, secretKeySecretKey} {
			Expect(env[key].Value).To(BeEmpty())
			Expect(env[key].ValueFrom.SecretKeyRef.Name).To(Equal(auditSecret))
			Expect(env[key].ValueFrom.SecretKeyRef.Key).To(Equal(key))
		}
	})

	It("mounts the data volume read-only, since it only reads the log", func() {
		mounts := map[string]corev1.VolumeMount{}
		for _, m := range container(hipaa()).VolumeMounts {
			mounts[m.Name] = m
		}
		Expect(mounts[dataVolumeName].ReadOnly).To(BeTrue())
		Expect(mounts[dataVolumeName].MountPath).To(Equal(dataMountPath))
		Expect(mounts[scriptVolumeName].ReadOnly).To(BeTrue())
	})

	It("runs with the strictest settings the database container does", func() {
		sc := container(hipaa()).SecurityContext
		Expect(*sc.AllowPrivilegeEscalation).To(BeFalse())
		Expect(sc.Capabilities.Drop).To(ConsistOf(corev1.Capability("ALL")))
	})

	It("has a memory limit sized for one chunk, and asks for little", func() {
		res := container(hipaa()).Resources
		Expect(res.Limits.Memory().String()).To(Equal("256Mi"))
		Expect(res.Requests.Cpu().MilliValue()).To(BeNumerically("<=", 50))
	})

	It("can never stop: a failing or missing shipper must not make the database pod not Ready", func() {
		script := container(hipaa()).Command[2]
		Expect(container(hipaa()).Command[:2]).To(Equal([]string{"bash", "-c"}))
		Expect(script).To(ContainSubstring("while true"))
		Expect(script).To(ContainSubstring("|| true"), "a failed run is followed by the next, not by an exit")
		Expect(script).To(ContainSubstring("trap 'exit 0' TERM INT"), "but it still stops promptly with the pod")
		Expect(script).To(ContainSubstring("python3 /scripts/audit_shipper.py"))
		Expect(container(hipaa()).LivenessProbe).To(BeNil(), "nothing here may restart or unready the pod")
		Expect(container(hipaa()).ReadinessProbe).To(BeNil())
	})

	Describe("in the pod template", func() {
		build := func(c *dbv1alpha1.PostgresCluster) corev1.PodSpec {
			sts, err := desiredStatefulSet(c, "img")
			ExpectWithOffset(1, err).NotTo(HaveOccurred())
			return sts.Spec.Template.Spec
		}
		names := func(containers []corev1.Container) []string {
			out := make([]string, 0, len(containers))
			for _, c := range containers {
				out = append(out, c.Name)
			}
			return out
		}

		It("is added next to the database container for a cluster with compliance settings", func() {
			pod := build(hipaa())
			Expect(names(pod.Containers)).To(Equal([]string{postgresContainerName, auditContainerName}))
			var script *corev1.Volume
			for i := range pod.Volumes {
				if pod.Volumes[i].Name == scriptVolumeName {
					script = &pod.Volumes[i]
				}
			}
			Expect(script).NotTo(BeNil())
			Expect(script.ConfigMap.Name).To(Equal("orders-audit-scripts"))
		})

		It("is absent without compliance settings, and nothing about the pod changes", func() {
			plain := plainCluster()
			pod := build(plain)
			Expect(names(pod.Containers)).To(Equal([]string{postgresContainerName}))
			for _, v := range pod.Volumes {
				Expect(v.Name).NotTo(Equal(scriptVolumeName))
			}
		})

		It("is absent when there is no destination, which the webhook refuses and the reconciler reports", func() {
			c := hipaa()
			c.Spec.Compliance.AuditLogs.Destination = nil
			Expect(names(build(c).Containers)).To(Equal([]string{postgresContainerName}))
		})

		It("leaves the database container exactly as it was, bar the library list", func() {
			with := build(hipaa()).Containers[0]
			without := build(plainWithName("orders", "prod")).Containers[0]
			Expect(with.VolumeMounts).To(Equal(without.VolumeMounts))
			Expect(with.Resources).To(Equal(without.Resources))
			Expect(with.SecurityContext).To(Equal(without.SecurityContext))
			Expect(with.Ports).To(Equal(without.Ports))
		})

		It("is not swapped for the next standard, so changing standards still replaces no pod", func() {
			a := withCompliance(plainCluster(), dbv1alpha1.StandardHIPAA)
			b := withCompliance(plainCluster(), dbv1alpha1.StandardPCI)
			Expect(build(a)).To(Equal(build(b)))
		})
	})
})

func plainWithName(name, namespace string) *dbv1alpha1.PostgresCluster {
	c := plainCluster()
	c.Name, c.Namespace = name, namespace
	return c
}

var _ = Describe("The audit shipper through the reconciler", func() {
	const namespace = "default"
	ctx := context.Background()
	key := func(name string) types.NamespacedName { return types.NamespacedName{Name: name, Namespace: namespace} }
	reconcileOnce := func(name string) reconcile.Result {
		rec := &PostgresClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Patroni: &fakePatroni{live: map[string]any{}}}
		res, err := rec.Reconcile(ctx, reconcile.Request{NamespacedName: key(name)})
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		return res
	}
	newCluster := func(name, secret string, mutate ...func(*dbv1alpha1.PostgresCluster)) {
		c := withCompliance(&dbv1alpha1.PostgresCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: dbv1alpha1.PostgresClusterSpec{
				Version: "17", Instances: 3,
				Storage:   dbv1alpha1.StorageSpec{Size: resource.MustParse("1Gi")},
				Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}},
			},
		}, dbv1alpha1.StandardHIPAA)
		c.Spec.Compliance.AuditLogs.Destination.CredentialsSecret = secret
		for _, m := range mutate {
			m(c)
		}
		ExpectWithOffset(1, k8sClient.Create(ctx, c)).To(Succeed())
		DeferCleanup(func() { ExpectWithOffset(1, client.IgnoreNotFound(k8sClient.Delete(ctx, c))).To(Succeed()) })
	}
	createSecret := func(name string, data map[string]string) {
		s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}, StringData: data}
		ExpectWithOffset(1, k8sClient.Create(ctx, s)).To(Succeed())
		DeferCleanup(func() { ExpectWithOffset(1, client.IgnoreNotFound(k8sClient.Delete(ctx, s))).To(Succeed()) })
	}
	good := map[string]string{secretKeyAccessKey: "id", secretKeySecretKey: testSecretValue}
	progressing := func(name string) *metav1.Condition {
		c := &dbv1alpha1.PostgresCluster{}
		ExpectWithOffset(1, k8sClient.Get(ctx, key(name), c)).To(Succeed())
		return meta.FindStatusCondition(c.Status.Conditions, "Progressing")
	}
	noStatefulSet := func(name string) {
		ExpectWithOffset(1, apierrors.IsNotFound(k8sClient.Get(ctx, key(name), &appsv1.StatefulSet{}))).To(BeTrue())
	}

	It("waits, changing nothing, while the audit credentials Secret does not exist", func() {
		const name = "ship-nosecret"
		newCluster(name, name+"-audit")
		res := reconcileOnce(name)
		Expect(res.RequeueAfter).To(Equal(backupSecretRecheckDelay))
		noStatefulSet(name)
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key(name+"-audit-scripts"), &corev1.ConfigMap{}))).To(BeTrue())
		cond := progressing(name)
		Expect(cond.Reason).To(Equal("BackupSecretProblem"))
		Expect(cond.Message).To(ContainSubstring("audit log credentials Secret"))
		Expect(cond.Message).To(ContainSubstring(name + "-audit"))
	})

	It("rejects an audit credentials Secret that lacks a key", func() {
		const name = "ship-halfsecret"
		createSecret(name+"-audit", map[string]string{secretKeyAccessKey: "id"})
		newCluster(name, name+"-audit")
		reconcileOnce(name)
		noStatefulSet(name)
		Expect(progressing(name).Message).To(ContainSubstring(secretKeySecretKey))
	})

	It("says what is wrong when the destination is missing, instead of building a pod without a shipper", func() {
		const name = "ship-nodest"
		newCluster(name, "unused", func(c *dbv1alpha1.PostgresCluster) { c.Spec.Compliance.AuditLogs.Destination = nil })
		reconcileOnce(name)
		noStatefulSet(name)
		Expect(progressing(name).Message).To(ContainSubstring("destination is required"))
	})

	It("carries on once the Secret appears, with the shipper in the pods and its script in a ConfigMap", func() {
		const name = "ship-ok"
		newCluster(name, name+"-audit")
		reconcileOnce(name)
		createSecret(name+"-audit", good)
		reconcileOnce(name)

		sts := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, key(name), sts)).To(Succeed())
		Expect(sts.Spec.Template.Spec.Containers).To(HaveLen(2))
		Expect(sts.Spec.Template.Spec.Containers[1].Name).To(Equal(auditContainerName))

		cm := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, key(name+"-audit-scripts"), cm)).To(Succeed())
		Expect(cm.Data).To(HaveKeyWithValue(auditScriptFile, auditShipperScript))
		Expect(cm.OwnerReferences).NotTo(BeEmpty(), "it goes with the cluster")
	})

	It("creates the script before the pods that mount it, in the same reconcile", func() {
		const name = "ship-order"
		createSecret(name+"-audit", good)
		newCluster(name, name+"-audit")
		reconcileOnce(name)
		Expect(k8sClient.Get(ctx, key(name+"-audit-scripts"), &corev1.ConfigMap{})).To(Succeed())
		Expect(k8sClient.Get(ctx, key(name), &appsv1.StatefulSet{})).To(Succeed())
	})

	It("removes the shipper and its script when the compliance settings go", func() {
		const name = "ship-remove"
		createSecret(name+"-audit", good)
		newCluster(name, name+"-audit")
		reconcileOnce(name)
		Expect(k8sClient.Get(ctx, key(name+"-audit-scripts"), &corev1.ConfigMap{})).To(Succeed())

		c := &dbv1alpha1.PostgresCluster{}
		Expect(k8sClient.Get(ctx, key(name), c)).To(Succeed())
		c.Spec.Compliance = nil
		Expect(k8sClient.Update(ctx, c)).To(Succeed())
		reconcileOnce(name)

		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key(name+"-audit-scripts"), &corev1.ConfigMap{}))).To(BeTrue())
		sts := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, key(name), sts)).To(Succeed())
		Expect(sts.Spec.Template.Spec.Containers).To(HaveLen(1))
	})

	It("restores the script if it is deleted by hand", func() {
		const name = "ship-repair"
		createSecret(name+"-audit", good)
		newCluster(name, name+"-audit")
		reconcileOnce(name)
		cm := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, key(name+"-audit-scripts"), cm)).To(Succeed())
		Expect(k8sClient.Delete(ctx, cm)).To(Succeed())
		reconcileOnce(name)
		Expect(k8sClient.Get(ctx, key(name+"-audit-scripts"), &corev1.ConfigMap{})).To(Succeed())
	})

	It("does not create a script for a cluster without compliance settings", func() {
		const name = "ship-never"
		newCluster(name, "x", func(c *dbv1alpha1.PostgresCluster) { c.Spec.Compliance = nil })
		reconcileOnce(name)
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key(name+"-audit-scripts"), &corev1.ConfigMap{}))).To(BeTrue())
	})
})

// The script itself, run for real against a stand-in for boto3 that keeps its
// "bucket" in a directory, so what was uploaded and how can be inspected.
var _ = Describe("The audit shipper script", func() {
	const (
		fakeBoto3 = `import base64, hashlib, json, os, urllib.parse


def client(service, **kwargs):
    with open(os.path.join(os.environ["FAKE_S3_DIR"], "client.json"), "w") as handle:
        json.dump({"service": service, "endpoint_url": kwargs.get("endpoint_url"),
                   "region_name": kwargs.get("region_name"), "config": kwargs["config"].kwargs}, handle)
    return Client(os.path.join(os.environ["FAKE_S3_DIR"], "store"))


class Client:
    def __init__(self, root):
        self.root = root
        os.makedirs(root, exist_ok=True)

    def _path(self, key):
        return os.path.join(self.root, urllib.parse.quote(key, safe=""))

    def list_objects_v2(self, Bucket, Prefix="", ContinuationToken=None):
        page = int(os.environ.get("FAKE_S3_PAGE", "1000"))
        keys = sorted(urllib.parse.unquote(n) for n in os.listdir(self.root) if not n.endswith(".meta"))
        keys = [k for k in keys if k.startswith(Prefix)]
        start = int(ContinuationToken or 0)
        out = {"Contents": [{"Key": k} for k in keys[start:start + page]]}
        if start + page < len(keys):
            out["IsTruncated"] = True
            out["NextContinuationToken"] = str(start + page)
        return out

    def put_object(self, Bucket, Key, Body, ContentMD5=None, ContentType=None, Metadata=None):
        fail = os.environ.get("FAKE_S3_FAIL_PUT")
        if fail and fail in Key:
            raise RuntimeError("simulated upload failure")
        if ContentMD5 != base64.b64encode(hashlib.md5(Body).digest()).decode():
            raise RuntimeError("BadDigest")
        with open(self._path(Key), "wb") as handle:
            handle.write(Body)
        with open(self._path(Key) + ".meta", "w") as handle:
            json.dump({"Metadata": Metadata, "ContentType": ContentType}, handle)

    def head_object(self, Bucket, Key):
        size = os.path.getsize(self._path(Key))
        return {"ContentLength": size - 1 if os.environ.get("FAKE_S3_HEAD_SHORT") else size}

    def delete_object(self, Bucket, Key):
        for path in (self._path(Key), self._path(Key) + ".meta"):
            if os.path.exists(path):
                os.remove(path)
`
		fakeConfig = `class Config:
    def __init__(self, **kwargs):
        self.kwargs = kwargs
`
		prefix = "audit/prod/orders/orders-0"
		stamp  = "2026-10-18 22:15:30.123 UTC"
	)

	var dir, logs string

	BeforeEach(func() {
		if _, err := exec.LookPath("python3"); err != nil {
			Skip("python3 is not available")
		}
		dir = GinkgoT().TempDir()
		logs = filepath.Join(dir, "logs")
		for path, body := range map[string]string{
			"fake/boto3/__init__.py":    fakeBoto3,
			"fake/botocore/__init__.py": "",
			"fake/botocore/config.py":   fakeConfig,
		} {
			full := filepath.Join(dir, path)
			ExpectWithOffset(1, os.MkdirAll(filepath.Dir(full), 0o755)).To(Succeed())  //nolint:gosec // a test directory
			ExpectWithOffset(1, os.WriteFile(full, []byte(body), 0o644)).To(Succeed()) //nolint:gosec // test files
		}
		for _, d := range []string{logs, filepath.Join(dir, "store")} {
			ExpectWithOffset(1, os.MkdirAll(d, 0o755)).To(Succeed()) //nolint:gosec // a test directory
		}
	})

	logFile := func(name, content string) string {
		path := filepath.Join(logs, name)
		ExpectWithOffset(1, os.WriteFile(path, []byte(content), 0o644)).To(Succeed()) //nolint:gosec // test files
		return path
	}
	appendTo := func(path, content string) {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644) //nolint:gosec // test files
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		_, err = f.WriteString(content)
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		ExpectWithOffset(1, f.Close()).To(Succeed())
	}
	run := func(extra ...string) (string, int) {
		cmd := exec.Command("python3", "scripts/audit_shipper.py")
		cmd.Env = append(os.Environ(), "PYTHONPATH="+filepath.Join(dir, "fake"), "FAKE_S3_DIR="+dir, "LOG_DIR="+logs,
			"AUDIT_BUCKET=audit-logs", "AUDIT_PREFIX=audit/prod/orders/", "POD_NAME=orders-0")
		cmd.Env = append(cmd.Env, extra...)
		out, err := cmd.CombinedOutput()
		code := 0
		if exitErr, ok := err.(*exec.ExitError); ok {
			code = exitErr.ExitCode()
		} else {
			ExpectWithOffset(1, err).NotTo(HaveOccurred(), string(out))
		}
		return string(out), code
	}
	keys := func() []string {
		entries, err := os.ReadDir(filepath.Join(dir, "store"))
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		var out []string
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".meta") {
				k, err := url.PathUnescape(e.Name())
				ExpectWithOffset(1, err).NotTo(HaveOccurred())
				out = append(out, k)
			}
		}
		slices.Sort(out)
		return out
	}
	object := func(key string) []byte {
		raw, err := os.ReadFile(filepath.Join(dir, "store", url.PathEscape(key)))
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		plain, err := io.ReadAll(zr)
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		return plain
	}
	// shipped joins a series' chunks in order, as someone reading the audit trail would.
	shipped := func(series string) string {
		var out strings.Builder
		for _, k := range keys() {
			if strings.HasPrefix(k, series) {
				out.Write(object(k))
			}
		}
		return out.String()
	}
	// seed puts a chunk in the bucket as an earlier run, or another sidecar, would have.
	seed := func(key, content string) {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		_, _ = zw.Write([]byte(content))
		Expect(zw.Close()).To(Succeed())
		ExpectWithOffset(1, os.WriteFile(filepath.Join(dir, "store", url.PathEscape(key)), buf.Bytes(), 0o644)).To(Succeed()) //nolint:gosec // test files
	}
	line := func(n int) string { return fmt.Sprintf("%s,audit line number %05d\n", stamp, n) }
	series := prefix + "/postgresql-7.csv/20261018T221530123/"

	It("ships a log file as one chunk named by pod, file, generation and byte range", func() {
		content := line(1) + line(2) + line(3)
		logFile("postgresql-7.csv", content)
		out, code := run()
		Expect(code).To(BeZero(), out)
		Expect(keys()).To(Equal([]string{fmt.Sprintf("%s%012d-%012d.csv.gz", series, 0, len(content))}))
		Expect(shipped(series)).To(Equal(content))
	})

	It("sends an MD5 for the bucket to check and records the SHA-256 of what it sent", func() {
		content := line(1) + line(2)
		logFile("postgresql-7.csv", content)
		_, code := run()
		Expect(code).To(BeZero())
		key := keys()[0]
		sum := sha256.Sum256([]byte(content))
		raw, err := os.ReadFile(filepath.Join(dir, "store", url.PathEscape(key)+".meta"))
		Expect(err).NotTo(HaveOccurred())
		var meta struct {
			Metadata    map[string]string
			ContentType string
		}
		Expect(json.Unmarshal(raw, &meta)).To(Succeed())
		Expect(meta.Metadata["sha256"]).To(Equal(hex.EncodeToString(sum[:])))
		Expect(meta.Metadata["source-file"]).To(Equal("postgresql-7.csv"))
		Expect(meta.ContentType).To(Equal("application/gzip"))
		// The stand-in refuses a body that does not match its Content-MD5, so a
		// shipper that sent the wrong header would have uploaded nothing.
		Expect(keys()).To(HaveLen(1))
	})

	It("ships only what is new, as a chunk that starts where the last one ended", func() {
		path := logFile("postgresql-7.csv", line(1)+line(2))
		_, code := run()
		Expect(code).To(BeZero())
		first := keys()
		appendTo(path, line(3)+line(4))
		_, code = run()
		Expect(code).To(BeZero())

		all := keys()
		Expect(all).To(HaveLen(2))
		Expect(all[0]).To(Equal(first[0]), "the first chunk is untouched")
		Expect(all[1]).To(MatchRegexp(fmt.Sprintf(`%012d-%012d\.csv\.gz$`, len(line(1)+line(2)), len(line(1)+line(2)+line(3)+line(4)))))
		Expect(shipped(series)).To(Equal(line(1) + line(2) + line(3) + line(4)))
	})

	It("uploads nothing when nothing is new, however many times it runs", func() {
		logFile("postgresql-7.csv", line(1))
		for range 3 {
			_, code := run()
			Expect(code).To(BeZero())
		}
		Expect(keys()).To(HaveLen(1))
	})

	It("leaves a line that is still being written until it is whole", func() {
		path := logFile("postgresql-7.csv", line(1)+stamp+",half a li")
		_, code := run()
		Expect(code).To(BeZero())
		Expect(shipped(series)).To(Equal(line(1)))

		appendTo(path, "ne\n")
		_, code = run()
		Expect(code).To(BeZero())
		Expect(shipped(series)).To(Equal(line(1) + stamp + ",half a line\n"))
	})

	It("starts a new series at zero when the file is truncated and written again, and leaves the old one alone", func() {
		path := logFile("postgresql-7.csv", line(1)+line(2)+line(3))
		_, code := run()
		Expect(code).To(BeZero())
		old := keys()

		next := strings.Replace(line(9), stamp, "2026-10-25 00:00:01.500 UTC", 1)
		Expect(os.WriteFile(path, []byte(next+next), 0o644)).To(Succeed()) //nolint:gosec // test files
		_, code = run()
		Expect(code).To(BeZero())

		all := keys()
		Expect(all).To(HaveLen(2))
		Expect(all).To(ContainElement(old[0]), "the previous week's chunk is not overwritten")
		fresh := prefix + "/postgresql-7.csv/20261025T000001500/"
		Expect(shipped(fresh)).To(Equal(next + next))
		Expect(all).To(ContainElement(HavePrefix(fresh + "000000000000-")))
	})

	It("carries on from the bucket's contents alone, as a restarted sidecar must", func() {
		content := line(1) + line(2) + line(3)
		logFile("postgresql-7.csv", content)
		seed(fmt.Sprintf("%s%012d-%012d.csv.gz", series, 0, len(line(1)+line(2))), line(1)+line(2))
		_, code := run()
		Expect(code).To(BeZero())
		Expect(keys()).To(HaveLen(2))
		Expect(shipped(series)).To(Equal(content))
	})

	It("fills a gap in the series exactly, and touches nothing else", func() {
		content := line(1) + line(2) + line(3)
		logFile("postgresql-7.csv", content)
		n := len(line(1))
		seed(fmt.Sprintf("%s%012d-%012d.csv.gz", series, 0, n), line(1))
		seed(fmt.Sprintf("%s%012d-%012d.csv.gz", series, 2*n, 3*n), line(3))
		_, code := run()
		Expect(code).To(BeZero())
		Expect(keys()).To(HaveLen(3))
		Expect(keys()[1]).To(Equal(fmt.Sprintf("%s%012d-%012d.csv.gz", series, n, 2*n)))
		Expect(shipped(series)).To(Equal(content))
	})

	It("reads every page of the listing to find where it stopped", func() {
		var content strings.Builder
		n := len(line(1))
		for i := range 5 {
			content.WriteString(line(i))
			seed(fmt.Sprintf("%s%012d-%012d.csv.gz", series, i*n, (i+1)*n), line(i))
		}
		content.WriteString(line(5))
		logFile("postgresql-7.csv", content.String())
		_, code := run("FAKE_S3_PAGE=2")
		Expect(code).To(BeZero())
		Expect(keys()).To(HaveLen(6), "only the sixth line was missing")
		Expect(shipped(series)).To(Equal(content.String()))
	})

	It("splits a large file into chunks that end on a line, and a run carries on where the last stopped", func() {
		var content strings.Builder
		for i := range 40 {
			content.WriteString(line(i))
		}
		logFile("postgresql-7.csv", content.String())
		size := len(line(0))
		_, code := run("MAX_CHUNK_BYTES="+fmt.Sprint(size*10+5), "MAX_BYTES_PER_RUN="+fmt.Sprint(size*20))
		Expect(code).To(BeZero())
		Expect(len(keys())).To(BeNumerically(">=", 2))
		Expect(shipped(series)).To(HaveLen(size*20), "a run stops at its limit")
		for _, k := range keys() {
			Expect(string(object(k))).To(HaveSuffix("\n"), "no chunk ends mid-line")
		}

		_, code = run("MAX_CHUNK_BYTES="+fmt.Sprint(size*10+5), "MAX_BYTES_PER_RUN="+fmt.Sprint(size*20))
		Expect(code).To(BeZero())
		_, code = run("MAX_CHUNK_BYTES=" + fmt.Sprint(size*10+5))
		Expect(code).To(BeZero())
		Expect(shipped(series)).To(Equal(content.String()))
	})

	It("ships a line longer than a chunk as it is, rather than never", func() {
		long := stamp + "," + strings.Repeat("x", 300) + "\n"
		logFile("postgresql-7.csv", long)
		limits := []string{"MAX_CHUNK_BYTES=100", "MAX_BYTES_PER_RUN=100"}
		_, code := run(limits...)
		Expect(code).To(BeZero())
		Expect(shipped(series)).To(Equal(long[:100]), "one chunk's worth, the rest follows")
		for range 3 {
			_, code = run(limits...)
			Expect(code).To(BeZero())
		}
		Expect(shipped(series)).To(Equal(long))
	})

	It("does not move on when an upload fails, and ships everything once the bucket is back", func() {
		content := line(1) + line(2)
		logFile("postgresql-7.csv", content)
		out, code := run("FAKE_S3_FAIL_PUT=postgresql")
		Expect(code).To(Equal(1), out)
		Expect(out).To(ContainSubstring("could not ship postgresql-7.csv"))
		Expect(keys()).To(BeEmpty())

		_, code = run()
		Expect(code).To(BeZero())
		Expect(shipped(series)).To(Equal(content))
	})

	It("removes a chunk whose size in the bucket is not what was sent, so it is sent again", func() {
		logFile("postgresql-7.csv", line(1))
		out, code := run("FAKE_S3_HEAD_SHORT=1")
		Expect(code).To(Equal(1), out)
		Expect(out).To(ContainSubstring("expected"))
		Expect(keys()).To(BeEmpty(), "left there, the next run would take it for shipped")
	})

	It("ships every file, and a file that fails does not stop the others", func() {
		one := logFile("postgresql-1.csv", line(1))
		two := logFile("postgresql-2.csv", line(2))
		_, _ = one, two
		out, code := run("FAKE_S3_FAIL_PUT=postgresql-1")
		Expect(code).To(Equal(1), out)
		Expect(keys()).To(HaveLen(1))
		Expect(keys()[0]).To(ContainSubstring("/postgresql-2.csv/"))
	})

	It("ships only the CSV log, not other files in the directory", func() {
		logFile("postgresql-7.csv", line(1))
		logFile("postgresql-7.log", "plain log, not audit\n")
		logFile("notes.txt", "not a log\n")
		_, code := run()
		Expect(code).To(BeZero())
		Expect(keys()).To(HaveLen(1))
		Expect(keys()[0]).To(ContainSubstring("postgresql-7.csv"))
	})

	It("skips an empty file", func() {
		logFile("postgresql-7.csv", "")
		_, code := run()
		Expect(code).To(BeZero())
		Expect(keys()).To(BeEmpty())
	})

	It("names a file whose first line is not a timestamp by a hash of it, the same every time", func() {
		logFile("postgresql-7.csv", "no timestamp here\nsecond line\n")
		_, code := run()
		Expect(code).To(BeZero())
		_, code = run()
		Expect(code).To(BeZero())
		Expect(keys()).To(HaveLen(1))
		Expect(keys()[0]).To(MatchRegexp(`/postgresql-7\.csv/h[0-9a-f]{16}/`))
	})

	It("addresses a store with its own endpoint by path, and signs for a region when none is given", func() {
		logFile("postgresql-7.csv", line(1))
		_, code := run("AUDIT_ENDPOINT=http://minio:9000", "AUDIT_FORCE_PATH_STYLE=true")
		Expect(code).To(BeZero())
		raw, err := os.ReadFile(filepath.Join(dir, "client.json"))
		Expect(err).NotTo(HaveOccurred())
		var got struct {
			EndpointURL string `json:"endpoint_url"`
			RegionName  string `json:"region_name"`
			Config      struct {
				S3 struct {
					AddressingStyle string `json:"addressing_style"`
				} `json:"s3"`
			} `json:"config"`
		}
		Expect(json.Unmarshal(raw, &got)).To(Succeed())
		Expect(got.EndpointURL).To(Equal("http://minio:9000"))
		Expect(got.RegionName).To(Equal("us-east-1"))
		Expect(got.Config.S3.AddressingStyle).To(Equal("path"))
	})

	It("uses Amazon's own addressing and the given region when there is no endpoint", func() {
		logFile("postgresql-7.csv", line(1))
		_, code := run("AUDIT_REGION=eu-west-1")
		Expect(code).To(BeZero())
		raw, err := os.ReadFile(filepath.Join(dir, "client.json"))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(raw)).To(ContainSubstring(`"region_name": "eu-west-1"`))
		Expect(string(raw)).To(ContainSubstring(`"endpoint_url": null`))
		Expect(string(raw)).To(ContainSubstring(`"addressing_style": "auto"`))
	})

	It("says so, and does not run, when it was not told where to ship", func() {
		out, code := run("AUDIT_BUCKET=")
		Expect(code).To(Equal(2))
		Expect(out).To(ContainSubstring("AUDIT_BUCKET and POD_NAME must be set"))
	})

	It("reports an unreadable log directory instead of crashing", func() {
		out, code := run("LOG_DIR=" + filepath.Join(dir, "missing"))
		Expect(code).To(Equal(1))
		Expect(out).To(ContainSubstring("cannot read"))
	})
})

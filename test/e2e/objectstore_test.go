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
	"fmt"
	"time"

	. "github.com/onsi/gomega"
)

// The object store the backup scenarios write to is SeaweedFS, pinned to one
// release. MinIO would be the obvious choice, but its official images are no
// longer published to the registries that can be pulled anonymously.
const (
	objectStoreNamespace = "e2e-s3"
	objectStoreImage     = "chrislusf/seaweedfs:4.48"
	backupBucket         = "pgbackups"
	// The store runs on a throwaway cluster and holds nothing of value, so
	// its credentials are fixed.
	objectStoreAccessKey = "e2eaccesskey"
	objectStoreSecretKey = "e2esecretkey123456"
)

var objectStoreEndpoint = fmt.Sprintf("http://s3.%s.svc.cluster.local:8333", objectStoreNamespace)

// objectStoreManifest is an S3 endpoint with one identity, in a namespace of
// its own so the strict Pod Security level on the database namespace does not
// apply to it.
func objectStoreManifest() string {
	return fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata: {name: s3-config, namespace: %[1]s}
data:
  s3.json: |
    {"identities":[{"name":"e2e","credentials":[{"accessKey":"%[3]s","secretKey":"%[4]s"}],
      "actions":["Admin","Read","Write","List","Tagging"]}]}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: s3, namespace: %[1]s}
spec:
  replicas: 1
  selector: {matchLabels: {app: s3}}
  template:
    metadata: {labels: {app: s3}}
    spec:
      containers:
      - name: seaweed
        image: %[2]s
        args: [server, -dir=/data, -s3, -s3.port=8333, -s3.config=/etc/s3/s3.json, -master.volumeSizeLimitMB=128]
        ports: [{containerPort: 8333}]
        readinessProbe: {tcpSocket: {port: 8333}, periodSeconds: 3}
        volumeMounts: [{name: cfg, mountPath: /etc/s3}]
      volumes: [{name: cfg, configMap: {name: s3-config}}]
---
apiVersion: v1
kind: Service
metadata: {name: s3, namespace: %[1]s}
spec:
  selector: {app: s3}
  ports: [{port: 8333, targetPort: 8333}]
---
apiVersion: batch/v1
kind: Job
metadata: {name: mkbucket, namespace: %[1]s}
spec:
  backoffLimit: 30
  template:
    spec:
      restartPolicy: OnFailure
      containers:
      - name: curl
        image: curlimages/curl:8.10.1
        command: [/bin/sh, -c]
        args: ["curl -sf --aws-sigv4 aws:amz:us-east-1:s3 --user %[3]s:%[4]s -X PUT http://s3:8333/%[5]s"]
`, objectStoreNamespace, objectStoreImage, objectStoreAccessKey, objectStoreSecretKey, backupBucket)
}

// startObjectStore deploys the store and creates the bucket, returning once
// the bucket exists.
func startObjectStore() {
	_, err := kubectlOut("create", "namespace", objectStoreNamespace)
	Expect(err).NotTo(HaveOccurred())
	_, err = kubectlIn(objectStoreManifest(), "apply", "-f", "-")
	Expect(err).NotTo(HaveOccurred())
	Eventually(func() (string, error) {
		return kubectlOut("-n", objectStoreNamespace, "get", "job", "mkbucket", "-o", "jsonpath={.status.succeeded}")
	}, 5*time.Minute, pollEvery).Should(Equal("1"), "the bucket could not be created")
}

func stopObjectStore() {
	_, _ = kubectlOut("delete", "namespace", objectStoreNamespace, "--wait=false")
}

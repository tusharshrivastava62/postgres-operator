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
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

// Objects are the resources found in a saved capture.
type Objects struct {
	Clusters []dbv1alpha1.PostgresCluster
	Requests []dbv1alpha1.PostgresEmergencyAccess
}

// ParseObjects reads the output of kubectl get -o json or -o yaml: one resource,
// or a List of them. Resources of other kinds are ignored.
func ParseObjects(data []byte) (Objects, error) {
	raw, err := yaml.YAMLToJSON(data)
	if err != nil {
		return Objects{}, fmt.Errorf("the input is neither JSON nor YAML: %w", err)
	}
	var head struct {
		Kind  string            `json:"kind"`
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return Objects{}, fmt.Errorf("the input is not a Kubernetes resource: %w", err)
	}

	var objs Objects
	items := head.Items
	if head.Kind != "List" {
		items = []json.RawMessage{raw}
	}
	for _, item := range items {
		var kind struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(item, &kind); err != nil {
			return Objects{}, fmt.Errorf("an item in the input is not a Kubernetes resource: %w", err)
		}
		switch kind.Kind {
		case "PostgresCluster":
			var c dbv1alpha1.PostgresCluster
			if err := json.Unmarshal(item, &c); err != nil {
				return Objects{}, fmt.Errorf("reading a PostgresCluster: %w", err)
			}
			objs.Clusters = append(objs.Clusters, c)
		case "PostgresEmergencyAccess":
			var q dbv1alpha1.PostgresEmergencyAccess
			if err := json.Unmarshal(item, &q); err != nil {
				return Objects{}, fmt.Errorf("reading a PostgresEmergencyAccess: %w", err)
			}
			objs.Requests = append(objs.Requests, q)
		}
	}
	return objs, nil
}

// Select picks the cluster to report on, with the requests made for it. The
// namespace and name may be empty if the capture holds only one cluster.
func (o Objects) Select(namespace, name, source string) (Input, error) {
	var found []dbv1alpha1.PostgresCluster
	for _, c := range o.Clusters {
		if (namespace == "" || c.Namespace == namespace) && (name == "" || c.Name == name) {
			found = append(found, c)
		}
	}
	switch len(found) {
	case 0:
		return Input{}, errors.New("the input holds no PostgresCluster" + describe(namespace, name))
	case 1:
		return Input{Cluster: found[0], Requests: o.Requests, Source: source}, nil
	}
	names := make([]string, len(found))
	for i, c := range found {
		names[i] = c.Namespace + "/" + c.Name
	}
	return Input{}, fmt.Errorf("the input holds %d clusters (%s): say which with --namespace and --name",
		len(found), strings.Join(names, ", "))
}

func describe(namespace, name string) string {
	switch {
	case namespace != "" && name != "":
		return " " + namespace + "/" + name
	case name != "":
		return " named " + name
	case namespace != "":
		return " in namespace " + namespace
	}
	return ""
}

// LiveOptions say which cluster to read, and how to reach the Kubernetes API.
type LiveOptions struct {
	Kubeconfig string
	Context    string
	// As is a user to impersonate, to see what a read-only user would see.
	As        string
	Namespace string
	Name      string
}

// LoadLive reads the cluster and its emergency access requests from the
// Kubernetes API. It only reads: get on the cluster, list on the requests.
func LoadLive(ctx context.Context, opts LiveOptions) (Input, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = opts.Kubeconfig
	overrides := &clientcmd.ConfigOverrides{CurrentContext: opts.Context}
	if opts.Namespace != "" {
		overrides.Context.Namespace = opts.Namespace
	}
	loader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides)
	cfg, err := loader.ClientConfig()
	if err != nil {
		return Input{}, fmt.Errorf("reading the kubeconfig: %w", err)
	}
	namespace, _, err := loader.Namespace()
	if err != nil {
		return Input{}, fmt.Errorf("reading the kubeconfig: %w", err)
	}
	if opts.As != "" {
		cfg.Impersonate.UserName = opts.As
	}

	scheme := runtime.NewScheme()
	if err := dbv1alpha1.AddToScheme(scheme); err != nil {
		return Input{}, err
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return Input{}, fmt.Errorf("connecting to the cluster: %w", err)
	}

	in := Input{Source: "live cluster"}
	key := client.ObjectKey{Namespace: namespace, Name: opts.Name}
	if err := c.Get(ctx, key, &in.Cluster); err != nil {
		return Input{}, readError(err, "get the PostgresCluster "+key.String(), "get", "postgresclusters")
	}
	var requests dbv1alpha1.PostgresEmergencyAccessList
	if err := c.List(ctx, &requests, client.InNamespace(namespace)); err != nil {
		return Input{}, readError(err, "list the PostgresEmergencyAccess requests in "+namespace, "list", "postgresemergencyaccesses")
	}
	in.Requests = requests.Items
	return in, nil
}

// readError explains a refusal in terms of the permission that is missing.
func readError(err error, did, verb, resource string) error {
	if apierrors.IsForbidden(err) {
		return fmt.Errorf("not allowed to %s: the report needs %s on %s.db.youroperator.io in this namespace: %w",
			did, verb, resource, err)
	}
	return fmt.Errorf("could not %s: %w", did, err)
}

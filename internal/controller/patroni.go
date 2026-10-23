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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

// The keys of Patroni's dynamic configuration that this operator manages.
const (
	dcsTTL                  = "ttl"
	dcsLoopWait             = "loop_wait"
	dcsRetryTimeout         = "retry_timeout"
	dcsMaximumLagOnFailover = "maximum_lag_on_failover"
	dcsFailsafeMode         = "failsafe_mode"
	dcsSynchronousMode      = "synchronous_mode"
)

// The section of Patroni's configuration that holds PostgreSQL's own settings,
// and the settings in it.
const (
	dcsPostgresql = "postgresql"
	dcsParameters = "parameters"
)

// annotationLeader is where Patroni records which member holds the leader
// lock, on the Endpoints object named after the cluster. It is the only
// trustworthy answer to "who is the primary": the role label on a pod can be
// stale for a primary that has hung.
const annotationLeader = "leader"

// switchoverTimeout bounds one switchover request.
const switchoverTimeout = 60 * time.Second

// leaderRequeueDelay is how soon to look again when there is no leader to
// talk to yet, as during cluster creation or a failover.
const leaderRequeueDelay = 10 * time.Second

// PatroniAPI is the part of a member's REST API the operator uses.
type PatroniAPI interface {
	// GetConfig returns the cluster-wide dynamic configuration.
	GetConfig(ctx context.Context, address string) (map[string]any, error)
	// PatchConfig merges the given keys into it. Patroni validates the
	// merged result as a whole and rejects an inconsistent change.
	PatchConfig(ctx context.Context, address string, patch map[string]any) error
	// Switchover asks the leader at address to hand leadership to candidate,
	// or to any eligible replica when candidate is empty. It returns once
	// the handover has completed.
	Switchover(ctx context.Context, address, leader, candidate string) error
}

// httpPatroni talks to Patroni's REST API over plain HTTP, which is how the
// Spilo image serves it.
type httpPatroni struct {
	client *http.Client
}

func newHTTPPatroni() *httpPatroni {
	// No client-wide timeout: each request carries its own deadline.
	return &httpPatroni{client: &http.Client{}}
}

func patroniURL(address string) string {
	return "http://" + net.JoinHostPort(address, strconv.Itoa(int(patroniPort))) + "/config"
}

// requestTimeout bounds a read or a configuration change.
const requestTimeout = 10 * time.Second

func (p *httpPatroni) GetConfig(ctx context.Context, address string) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, patroniURL(address), nil)
	if err != nil {
		return nil, err
	}
	body, err := p.do(req)
	if err != nil {
		return nil, err
	}
	config := map[string]any{}
	if err := json.Unmarshal(body, &config); err != nil {
		return nil, fmt.Errorf("decoding patroni config: %w", err)
	}
	return config, nil
}

func (p *httpPatroni) PatchConfig(ctx context.Context, address string, patch map[string]any) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	payload, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, patroniURL(address), bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	_, err = p.do(req)
	return err
}

func (p *httpPatroni) Switchover(ctx context.Context, address, leader, candidate string) error {
	body := map[string]string{"leader": leader}
	if candidate != "" {
		body["candidate"] = candidate
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	// A switchover waits for the old leader to demote and the new one to
	// take over, which takes a few seconds; allow far longer than a read.
	ctx, cancel := context.WithTimeout(ctx, switchoverTimeout)
	defer cancel()
	url := "http://" + net.JoinHostPort(address, strconv.Itoa(int(patroniPort))) + "/switchover"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	_, err = p.do(req)
	return err
}

func (p *httpPatroni) do(req *http.Request) ([]byte, error) {
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("patroni %s %s returned %d: %s", req.Method, req.URL.Path, resp.StatusCode, bytes.TrimSpace(body))
	}
	return body, nil
}

// desiredDynamicConfig is the slice of Patroni's dynamic configuration this
// operator manages. Anything else in it (replication slots, other PostgreSQL
// parameters) is left exactly as it is. The audit settings under
// postgresql.parameters are the one exception; see complianceParameterPatch.
//
// failoverTimeout becomes Patroni's ttl, and loop_wait and retry_timeout are
// each a third of it (rounded down). Patroni requires
// loop_wait + 2*retry_timeout <= ttl; with both at floor(ttl/3) that always
// holds, and at the CRD's minimum ttl of 20 the values (6 and 6) are still
// above Patroni's own minimums.
func desiredDynamicConfig(cluster *dbv1alpha1.PostgresCluster) map[string]any {
	ha := cluster.Spec.HA
	ttl := ha.FailoverTimeout
	if ttl == 0 {
		ttl = defaultFailoverTimeout
	}
	maxLag := defaultMaximumLagOnFailover
	if ha.MaximumLagOnFailover != nil {
		maxLag = *ha.MaximumLagOnFailover
	}
	return map[string]any{
		dcsTTL:                  int64(ttl),
		dcsLoopWait:             int64(ttl / 3),
		dcsRetryTimeout:         int64(ttl / 3),
		dcsMaximumLagOnFailover: maxLag,
		// With the Kubernetes API unreachable, a primary that can still reach
		// its replicas keeps serving instead of demoting itself. Measured on
		// Kind: no failed writes during a 50s API-server outage, against a
		// 41s write outage without it. It needs pod-to-pod access to the
		// Patroni port.
		dcsFailsafeMode:    true,
		dcsSynchronousMode: ha.SynchronousMode,
	}
}

// configDiff returns the entries of desired that the live configuration does
// not already have. JSON numbers decode as float64, so numeric values are
// compared as numbers, not by Go type.
func configDiff(desired, live map[string]any) map[string]any {
	diff := map[string]any{}
	for key, want := range desired {
		got, ok := live[key]
		if !ok || !sameValue(want, got) {
			diff[key] = want
		}
	}
	return diff
}

func sameValue(want, got any) bool {
	switch w := want.(type) {
	case int64:
		g, ok := got.(float64)
		return ok && float64(w) == g
	case bool:
		g, ok := got.(bool)
		return ok && w == g
	default:
		return fmt.Sprint(want) == fmt.Sprint(got)
	}
}

// leaderAddress names the pod holding the leader lock and returns its IP. The
// address is empty, without an error, while there is no leader to talk to: as
// during creation or a failover, or when the leader's pod has no address yet.
func (r *PostgresClusterReconciler) leaderAddress(ctx context.Context, cluster *dbv1alpha1.PostgresCluster) (name, address string, err error) {
	// Core Endpoints are deprecated in Kubernetes, but they are where Patroni
	// keeps the leader record, so they are what has to be read.
	endpoints := &corev1.Endpoints{} //nolint:staticcheck // Patroni's Kubernetes DCS stores the leader on Endpoints
	err = r.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: cluster.Name}, endpoints)
	if apierrors.IsNotFound(err) {
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	name = endpoints.Annotations[annotationLeader]
	if name == "" {
		return "", "", nil
	}

	pod := &corev1.Pod{}
	err = r.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: name}, pod)
	if apierrors.IsNotFound(err) {
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	return name, pod.Status.PodIP, nil
}

// syncPatroniConfig makes the running cluster's dynamic configuration match
// spec.ha. It reports whether that was possible.
//
// The settings cannot simply live in the pod spec. Patroni only reads them
// from the environment when it first initialises a cluster and from the DCS
// afterwards, so editing a pod-spec value would roll every pod, one after
// another and without a controlled switchover, and still change nothing. Here
// the operator asks the current leader, which holds the authoritative copy,
// and sends only what differs. It is idempotent, so a lost or reset
// configuration is also put back.
//
// A cluster with no leader yet, or one whose leader pod has no address, is
// not an error: it just means try again shortly.
func (r *PostgresClusterReconciler) syncPatroniConfig(ctx context.Context, cluster *dbv1alpha1.PostgresCluster) (bool, error) {
	log := logf.FromContext(ctx)

	leader, address, err := r.leaderAddress(ctx, cluster)
	if err != nil {
		return false, err
	}
	if address == "" {
		return false, nil
	}

	api := r.Patroni
	if api == nil {
		api = defaultPatroni
	}
	live, err := api.GetConfig(ctx, address)
	if err != nil {
		// The leader named in the DCS may be the old one, still shutting
		// down, so this is expected during a failover.
		log.Info("could not read Patroni configuration from the leader", "leader", leader, "error", err.Error())
		return false, nil
	}
	diff := configDiff(desiredDynamicConfig(cluster), live)
	if patch := complianceParameterPatch(cluster, live); patch != nil {
		diff[dcsPostgresql] = patch[dcsPostgresql]
	}
	if len(diff) == 0 {
		return true, nil
	}
	if err := api.PatchConfig(ctx, address, diff); err != nil {
		return false, fmt.Errorf("updating Patroni configuration on %s: %w", leader, err)
	}
	log.Info("updated Patroni configuration", "leader", leader, "changed", diff)
	return true, nil
}

var defaultPatroni PatroniAPI = newHTTPPatroni()

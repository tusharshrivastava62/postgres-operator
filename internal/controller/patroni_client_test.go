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
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testAPIPassword = "s3cret-api"
	testOldPassword = "pw-old"
	testNewPassword = "pw-new"
)

// fakeAPI is a Patroni that asks for a password on anything that changes
// something, as one configured to does, and records what it was asked.
type fakeAPI struct {
	mu       sync.Mutex
	password string // empty: no password is asked for
	status   int    // what to answer a request that is let in with; 0 is 200
	seen     []seenRequest
}

type seenRequest struct {
	method, path, user, password, contentType, body string
	hadAuth                                         bool
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	user, password, ok := r.BasicAuth()
	f.seen = append(f.seen, seenRequest{r.Method, r.URL.Path, user, password, r.Header.Get("Content-Type"), string(body), ok})
	if f.password != "" && r.Method != http.MethodGet && password != f.password {
		http.Error(w, "not authenticated", http.StatusUnauthorized)
		return
	}
	if f.status != 0 {
		http.Error(w, "refused", f.status)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ttl": 30})
}

func (f *fakeAPI) requests() []seenRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]seenRequest(nil), f.seen...)
}

// serveAPI starts the fake, over TLS if it is given a certificate, and returns
// where it is.
func serveAPI(t *testing.T, api *fakeAPI, cert *testCert) PatroniTarget {
	t.Helper()
	server := httptest.NewUnstartedServer(api)
	if cert != nil {
		server.TLS = &tls.Config{
			Certificates: []tls.Certificate{{Certificate: [][]byte{cert.cert.Raw}, PrivateKey: cert.key}},
			MinVersion:   tls.VersionTLS12,
		}
		server.StartTLS()
	} else {
		server.Start()
	}
	t.Cleanup(server.Close)
	host, port, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	n, _ := strconv.Atoi(port)
	return PatroniTarget{Address: host, Port: int32(n)} //nolint:gosec // a TCP port
}

func TestTheAPIClientSendsNoCredentialsWhenThereAreNone(t *testing.T) {
	api := &fakeAPI{}
	target := serveAPI(t, api, nil)
	if _, err := newHTTPPatroni().GetConfig(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	seen := api.requests()
	if len(seen) != 1 || seen[0].hadAuth || seen[0].method != http.MethodGet || seen[0].path != "/config" {
		t.Errorf("requests %+v", seen)
	}
}

func TestTheAPIClientSendsTheCredentialsItHas(t *testing.T) {
	api := &fakeAPI{password: testAPIPassword}
	target := serveAPI(t, api, nil)
	target.Username, target.Passwords = patroniAPIUser, []string{testAPIPassword}
	if err := newHTTPPatroni().PatchConfig(context.Background(), target, map[string]any{"ttl": 40}); err != nil {
		t.Fatal(err)
	}
	seen := api.requests()
	if len(seen) != 1 {
		t.Fatalf("requests %+v", seen)
	}
	got := seen[0]
	if got.method != http.MethodPatch || got.path != "/config" || got.user != patroniAPIUser || got.password != testAPIPassword ||
		got.contentType != "application/json" || got.body != `{"ttl":40}` {
		t.Errorf("request %+v", got)
	}
}

func TestSwitchoverSaysWhoToHandOverTo(t *testing.T) {
	api := &fakeAPI{}
	target := serveAPI(t, api, nil)
	client := newHTTPPatroni()
	if err := client.Switchover(context.Background(), target, "orders-0", "orders-1"); err != nil {
		t.Fatal(err)
	}
	if err := client.Switchover(context.Background(), target, "orders-0", ""); err != nil {
		t.Fatal(err)
	}
	seen := api.requests()
	if len(seen) != 2 || seen[0].method != http.MethodPost || seen[0].path != "/switchover" {
		t.Fatalf("requests %+v", seen)
	}
	if seen[0].body != `{"candidate":"orders-1","leader":"orders-0"}` || seen[1].body != `{"leader":"orders-0"}` {
		t.Errorf("bodies %q and %q", seen[0].body, seen[1].body)
	}
}

// While the pods are being replaced with a new password, some still ask for the
// old one.
func TestTheAPIClientTriesThePreviousPasswordWhenTheCurrentOneIsRefused(t *testing.T) {
	api := &fakeAPI{password: testOldPassword}
	target := serveAPI(t, api, nil)
	target.Username, target.Passwords = patroniAPIUser, []string{testNewPassword, testOldPassword}
	if err := newHTTPPatroni().Switchover(context.Background(), target, "orders-0", "orders-1"); err != nil {
		t.Fatal(err)
	}
	seen := api.requests()
	tried := make([]string, 0, len(seen))
	for _, r := range seen {
		tried = append(tried, r.password)
	}
	if strings.Join(tried, ",") != testNewPassword+","+testOldPassword {
		t.Errorf("passwords tried: %v", tried)
	}
}

func TestTheAPIClientGivesUpWhenNoPasswordIsAccepted(t *testing.T) {
	api := &fakeAPI{password: "something-else"}
	target := serveAPI(t, api, nil)
	target.Username, target.Passwords = patroniAPIUser, []string{testNewPassword, testOldPassword}
	err := newHTTPPatroni().PatchConfig(context.Background(), target, map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("got %v, want a 401", err)
	}
	if got := len(api.requests()); got != 2 {
		t.Errorf("%d requests, want one for each password", got)
	}
}

func TestTheAPIClientDoesNotTryAnotherPasswordForAnyOtherRefusal(t *testing.T) {
	api := &fakeAPI{status: http.StatusConflict}
	target := serveAPI(t, api, nil)
	target.Username, target.Passwords = patroniAPIUser, []string{testNewPassword, testOldPassword}
	err := newHTTPPatroni().Switchover(context.Background(), target, "orders-0", "orders-1")
	if err == nil || !strings.Contains(err.Error(), "409") {
		t.Fatalf("got %v", err)
	}
	if got := len(api.requests()); got != 1 {
		t.Errorf("%d requests, want one", got)
	}
}

func TestTheAPIClientChecksTheCertificateOfAnAPIServedOverTLS(t *testing.T) {
	ca := newCurrentCA(t)
	roots := x509.NewCertPool()
	roots.AddCert(ca.cert)
	const podName = "orders-0.orders-headless.shop.svc.cluster.local"
	leaf := issue(t, ca, "orders", false, []string{podName, "127.0.0.1"}, time.Now().Add(-time.Minute), time.Now().Add(time.Hour), serverAuth)

	t.Run("accepts it for the name it was told, and sends the credentials over it", func(t *testing.T) {
		api := &fakeAPI{password: testAPIPassword}
		target := serveAPI(t, api, leaf)
		target.TLS = &SQLTLS{Roots: roots, ServerName: podName}
		target.Username, target.Passwords = patroniAPIUser, []string{testAPIPassword}
		if err := newHTTPPatroni().PatchConfig(context.Background(), target, map[string]any{}); err != nil {
			t.Fatal(err)
		}
		if got := api.requests(); len(got) != 1 || got[0].password != testAPIPassword {
			t.Errorf("requests %+v", got)
		}
	})
	t.Run("refuses it for another name, and sends nothing", func(t *testing.T) {
		api := &fakeAPI{}
		target := serveAPI(t, api, leaf)
		target.TLS = &SQLTLS{Roots: roots, ServerName: "orders-1.orders-headless.shop.svc.cluster.local"}
		target.Username, target.Passwords = patroniAPIUser, []string{testAPIPassword}
		if _, err := newHTTPPatroni().GetConfig(context.Background(), target); err == nil {
			t.Fatal("a certificate for another name was accepted")
		}
		if got := api.requests(); len(got) != 0 {
			t.Errorf("a request went to a server that was not verified: %+v", got)
		}
	})
	t.Run("refuses a certificate from another CA", func(t *testing.T) {
		stranger := x509.NewCertPool()
		stranger.AddCert(newCurrentCA(t).cert)
		api := &fakeAPI{}
		target := serveAPI(t, api, leaf)
		target.TLS = &SQLTLS{Roots: stranger, ServerName: podName}
		if _, err := newHTTPPatroni().GetConfig(context.Background(), target); err == nil {
			t.Fatal("a certificate from another CA was accepted")
		}
	})
	t.Run("does not speak plain HTTP to an API that answers over TLS", func(t *testing.T) {
		api := &fakeAPI{}
		target := serveAPI(t, api, leaf)
		if _, err := newHTTPPatroni().GetConfig(context.Background(), target); err == nil {
			t.Fatal("plain HTTP got an answer from a TLS server")
		}
	})
	t.Run("builds https URLs for a target with TLS and http for one without", func(t *testing.T) {
		if got := (PatroniTarget{Address: "10.0.0.1", TLS: &SQLTLS{}}).url("/config"); got != "https://10.0.0.1:8008/config" {
			t.Errorf("got %s", got)
		}
		if got := (PatroniTarget{Address: "10.0.0.1"}).url("/config"); got != "http://10.0.0.1:8008/config" {
			t.Errorf("got %s", got)
		}
	})
}

func TestReloadAsksTheMemberToRereadItsConfigurationWithTheCredentials(t *testing.T) {
	api := &fakeAPI{password: testAPIPassword, status: 0}
	target := serveAPI(t, api, nil)
	target.Username, target.Passwords = patroniAPIUser, []string{testAPIPassword}
	if err := newHTTPPatroni().Reload(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	seen := api.requests()
	if len(seen) != 1 || seen[0].method != http.MethodPost || seen[0].path != "/reload" || seen[0].password != testAPIPassword {
		t.Errorf("requests %+v", seen)
	}
	// Patroni answers 202 Accepted to a reload, which is success.
	target.Passwords = []string{"wrong"}
	if err := newHTTPPatroni().Reload(context.Background(), target); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("a reload with the wrong password: %v", err)
	}
}

func TestAnAcceptedAnswerIsSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusAccepted) }))
	t.Cleanup(server.Close)
	host, port, _ := net.SplitHostPort(server.Listener.Addr().String())
	n, _ := strconv.Atoi(port)
	if err := newHTTPPatroni().Reload(context.Background(), PatroniTarget{Address: host, Port: int32(n)}); err != nil { //nolint:gosec // a TCP port
		t.Fatalf("202 is how Patroni says a reload was accepted: %v", err)
	}
}

func TestTheServedCertificateIsReadWithoutSendingAnything(t *testing.T) {
	ca := newCurrentCA(t)
	leaf := issue(t, ca, "orders", false, []string{"orders.shop.svc"}, time.Now().Add(-time.Minute), time.Now().Add(time.Hour), serverAuth)
	api := &fakeAPI{}
	target := serveAPI(t, api, leaf)
	// The target names no roots: what is presented is read, whoever issued it.
	got, err := newHTTPPatroni().ServedCertificate(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(leaf.cert) {
		t.Error("not the certificate the server presents")
	}
	if requests := api.requests(); len(requests) != 0 {
		t.Errorf("a request was sent, with whatever it carried: %+v", requests)
	}
	if _, err := newHTTPPatroni().ServedCertificate(context.Background(), serveAPI(t, &fakeAPI{}, nil)); err == nil {
		t.Error("an API that does not use TLS has no certificate to read")
	}
}

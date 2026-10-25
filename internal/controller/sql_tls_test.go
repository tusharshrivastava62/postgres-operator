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
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// serveTLSOnce plays the start of a PostgreSQL server: it agrees to TLS when
// asked, completes the handshake with the certificate, and hangs up. That is
// enough to see whether a client accepted the certificate.
func serveTLSOnce(t *testing.T, cert *testCert) (host string, port int32) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	pair := tls.Certificate{Certificate: [][]byte{cert.cert.Raw}, PrivateKey: cert.key}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				request := make([]byte, 8)
				if _, err := io.ReadFull(conn, request); err != nil || binary.BigEndian.Uint32(request[4:]) != 80877103 {
					return
				}
				if _, err := conn.Write([]byte("S")); err != nil {
					return
				}
				secure := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12})
				_ = secure.Handshake()
			}()
		}
	}()
	addr := listener.Addr().(*net.TCPAddr)
	return addr.IP.String(), int32(addr.Port) //nolint:gosec // a TCP port
}

func dialWith(t *testing.T, cert *testCert, check *SQLTLS) error {
	t.Helper()
	host, port := serveTLSOnce(t, cert)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session, err := NewPGXDialer().Dial(ctx, SQLTarget{Host: host, Port: port, Database: monitorDatabase, User: testSuperuserName, Password: "x", TLS: check})
	if err == nil {
		session.Close(ctx)
	}
	return err
}

func isCertificateRefusal(err error) bool {
	var refused *tls.CertificateVerificationError
	return errors.As(err, &refused)
}

// The server in these tests hangs up after the handshake, so a connection that
// accepts the certificate fails later, for another reason. What matters is
// whether it is the certificate that is refused.
func TestTheOperatorsConnectionChecksTheCertificateItWasGiven(t *testing.T) {
	current := newCurrentCA(t)
	currentRoots := x509.NewCertPool()
	currentRoots.AddCert(current.cert)
	leaf := issue(t, current, "orders", false, []string{testServiceName}, time.Now().Add(-time.Minute), time.Now().Add(time.Hour), serverAuth)

	t.Run("accepts a certificate from its CA, for the name it was told", func(t *testing.T) {
		err := dialWith(t, leaf, &SQLTLS{Roots: currentRoots, ServerName: testServiceName})
		if isCertificateRefusal(err) {
			t.Fatalf("the certificate was refused: %v", err)
		}
	})
	t.Run("refuses it for another name, though it is connected to by address", func(t *testing.T) {
		err := dialWith(t, leaf, &SQLTLS{Roots: currentRoots, ServerName: "orders.elsewhere.svc"})
		if !isCertificateRefusal(err) {
			t.Fatalf("got %v, want the certificate refused", err)
		}
	})
	t.Run("refuses a certificate from another CA", func(t *testing.T) {
		stranger := x509.NewCertPool()
		stranger.AddCert(newCurrentCA(t).cert)
		err := dialWith(t, leaf, &SQLTLS{Roots: stranger, ServerName: testServiceName})
		if !isCertificateRefusal(err) {
			t.Fatalf("got %v, want the certificate refused", err)
		}
	})
	t.Run("refuses a self-signed certificate", func(t *testing.T) {
		self := issue(t, nil, "orders", true, []string{testServiceName}, time.Now().Add(-time.Minute), time.Now().Add(time.Hour), serverAuth)
		err := dialWith(t, self, &SQLTLS{Roots: currentRoots, ServerName: testServiceName})
		if !isCertificateRefusal(err) {
			t.Fatalf("got %v, want the certificate refused", err)
		}
	})
	t.Run("does not check it when the cluster was given none", func(t *testing.T) {
		self := issue(t, nil, "orders", true, []string{"unused.example.com"}, time.Now().Add(-time.Minute), time.Now().Add(time.Hour), serverAuth)
		if err := dialWith(t, self, nil); isCertificateRefusal(err) {
			t.Fatalf("a certificate the image made was refused: %v", err)
		}
	})
	t.Run("tells an expired certificate from one that is wrong", func(t *testing.T) {
		old := issue(t, current, "orders", false, []string{testServiceName}, time.Now().Add(-48*time.Hour), time.Now().Add(-time.Hour), serverAuth)
		err := dialWith(t, old, &SQLTLS{Roots: currentRoots, ServerName: testServiceName})
		var refused *tls.CertificateVerificationError
		var invalid x509.CertificateInvalidError
		if !errors.As(err, &refused) || !errors.As(refused.Err, &invalid) || invalid.Reason != x509.Expired {
			t.Fatalf("got %v, want an expired certificate", err)
		}
	})
}

func newCurrentCA(t *testing.T) *testCert {
	t.Helper()
	return issue(t, nil, "orders-ca", true, nil, time.Now().Add(-100*24*time.Hour), time.Now().Add(24*time.Hour), nil)
}

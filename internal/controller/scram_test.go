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
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
)

// The expected values were produced by an independent implementation (Python's
// hashlib.pbkdf2_hmac and hmac) from the same inputs, not by this code.
func TestScramVerifierMatchesAnIndependentImplementation(t *testing.T) {
	salt := make([]byte, 16)
	for i := range salt {
		salt[i] = byte(i)
	}
	got, err := scramVerifier("correct-horse-battery-staple-0123", salt, 4096)
	if err != nil {
		t.Fatal(err)
	}
	const want = "SCRAM-SHA-256$4096:AAECAwQFBgcICQoLDA0ODw==$YaHvKPUZHR9m7FVjxnbsZorn/1QqCMJmxNQGOrx8vdk=:UZSfxVdanWRqBZT+gsR2cQyAIYyKG08hIAuk8Y+/GKE="
	if got != want {
		t.Fatalf("verifier = %s, want %s", got, want)
	}
}

func TestScramVerifierShape(t *testing.T) {
	password := "pa55word-that-must-not-appear"
	verifier, err := newScramVerifier(password)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(verifier, password) {
		t.Fatal("the verifier contains the password")
	}
	var iterations int
	var salt, stored, server string
	if _, err := fmt.Sscanf(strings.NewReplacer("$", " ", ":", " ").Replace(verifier), "SCRAM-SHA-256 %d %s %s %s", &iterations, &salt, &stored, &server); err != nil {
		t.Fatalf("not a SCRAM-SHA-256 verifier: %s: %v", verifier, err)
	}
	if iterations != scramIterations {
		t.Errorf("iterations = %d, want %d", iterations, scramIterations)
	}
	for name, part := range map[string]string{"salt": salt, "stored key": stored, "server key": server} {
		if _, err := base64.StdEncoding.DecodeString(part); err != nil {
			t.Errorf("%s is not base64: %v", name, err)
		}
	}
}

func TestScramVerifierUsesAFreshSaltAndDependsOnThePassword(t *testing.T) {
	a, _ := newScramVerifier("same")
	b, _ := newScramVerifier("same")
	if a == b {
		t.Error("two verifiers of one password are identical: the salt is not random")
	}
	salt := []byte("0123456789abcdef")
	one, _ := scramVerifier("one", salt, 4096)
	two, _ := scramVerifier("two", salt, 4096)
	if one == two {
		t.Error("different passwords give the same verifier")
	}
	fewer, _ := scramVerifier("one", salt, 1000)
	if one == fewer {
		t.Error("the iteration count does not change the verifier")
	}
}

func TestSQLLiteralCannotBeEndedFromInside(t *testing.T) {
	for in, want := range map[string]string{
		"plain":        "'plain'",
		"it's":         "'it''s'",
		"'; drop x;--": "'''; drop x;--'",
		"":             "''",
	} {
		if got := sqlLiteral(in); got != want {
			t.Errorf("sqlLiteral(%q) = %s, want %s", in, got, want)
		}
	}
}

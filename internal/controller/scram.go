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
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

// scramIterations is PostgreSQL's own default for scram_iterations.
const scramIterations = 4096

// scramVerifier is the SCRAM-SHA-256 verifier PostgreSQL stores for a
// password: the iteration count, the salt, and two keys derived from the
// password, never the password. Handing it to ALTER ROLE ... PASSWORD makes
// the server store exactly this, and, unlike a clear-text password, keeps the
// password out of the server log, which records the statement that sets it.
//
// The password is used as it is. PostgreSQL normalises a password with
// SASLprep first, which changes nothing for the ASCII passwords this operator
// generates.
func scramVerifier(password string, salt []byte, iterations int) (string, error) {
	salted, err := pbkdf2.Key(sha256.New, password, salt, iterations, sha256.Size)
	if err != nil {
		return "", fmt.Errorf("deriving the password key: %w", err)
	}
	clientKey := hmacSHA256(salted, "Client Key")
	storedKey := sha256.Sum256(clientKey)
	serverKey := hmacSHA256(salted, "Server Key")
	return fmt.Sprintf("SCRAM-SHA-256$%d:%s$%s:%s", iterations,
		base64.StdEncoding.EncodeToString(salt),
		base64.StdEncoding.EncodeToString(storedKey[:]),
		base64.StdEncoding.EncodeToString(serverKey)), nil
}

func hmacSHA256(key []byte, message string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(message))
	return mac.Sum(nil)
}

// newScramVerifier is scramVerifier with a fresh random salt.
func newScramVerifier(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generating a salt: %w", err)
	}
	return scramVerifier(password, salt, scramIterations)
}

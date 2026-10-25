/*
Copyright 2026.

Licensed under the Business Source License 1.1 (the "License"). You may not
use this file except in compliance with the License, a copy of which is in the
LICENSE file at the root of this repository. Production use requires a
commercial license from the Licensor. On the Change Date (2030-10-17) the
work becomes available under the Change License (Apache-2.0).
*/

// pg-compliance prints the compliance status the postgres-operator wrote for a
// cluster as a report a person can read.
package main

import (
	"context"
	"os"

	"github.com/tusharshrivastava62/postgres-operator/internal/compliancereport"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	os.Exit(compliancereport.Run(context.Background(), os.Args[1:], compliancereport.DefaultEnv(version)))
}

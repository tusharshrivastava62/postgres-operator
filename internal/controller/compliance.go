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
	"fmt"
	"strings"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

const (
	// paramPreload is the setting that loads pgaudit. Spilo defines it in each
	// pod's own Patroni file, and a value there beats the one in the shared
	// configuration, so it cannot be changed through Patroni's REST API: a
	// live test stored the new list, saw no pending restart, and PostgreSQL
	// kept the old one. It has to travel in the pod template, which rolls the
	// pods through the same replicas-first, switchover-before-the-primary
	// rollout as any other template change.
	paramPreload = "shared_preload_libraries"

	// pgauditLibrary is the library that provides the audit log.
	pgauditLibrary = "pgaudit"

	// spiloPreloadLibraries is what Spilo 4.1-p2 loads when nothing sets the
	// list: its own default, and then the extensions it adds at start. It is
	// the same for PostgreSQL 16, 17 and 18. A list set in SPILO_CONFIGURATION
	// replaces all of it, so the list must be written out in full, and an
	// extension left out would stop loading. An end-to-end check compares the
	// live setting with this, so moving the Spilo pin cannot change it silently.
	spiloPreloadLibraries = "bg_mon,pg_stat_statements,pgextwlist,pg_auth_mon,set_user,timescaledb,pg_cron,pg_stat_kcache"

	// paramLogStatement is switched off while pgaudit is on, since pgaudit
	// records every statement it would; Spilo's own setting is spiloLogStatement.
	paramLogStatement = "log_statement"
	logStatementNone  = "none"
	// spiloLogStatement is what Spilo sets, and what the operator restores when
	// the compliance settings are removed.
	spiloLogStatement = "ddl"
)

// preloadLibraries is the value for shared_preload_libraries when the cluster
// has compliance settings, and "" when Spilo's own list should stand.
func preloadLibraries(cluster *dbv1alpha1.PostgresCluster) string {
	if cluster.Spec.Compliance == nil {
		return ""
	}
	return spiloPreloadLibraries + "," + pgauditLibrary
}

// pgauditParameters are the pgaudit settings the operator writes, and so the
// ones it removes again. Anything else under pgaudit. is someone else's.
func pgauditParameters() []string {
	preset, _ := dbv1alpha1.PresetFor(dbv1alpha1.StandardHIPAA)
	var names []string
	for name := range preset.Parameters(false) {
		if strings.HasPrefix(name, pgauditLibrary+".") {
			names = append(names, name)
		}
	}
	return names
}

// complianceParameterPatch returns the change to Patroni's dynamic
// configuration that brings the audit settings in line with the spec, or nil
// when it already matches. These settings reload; none needs a restart, and a
// value set before the library is loaded is simply held until it is.
//
// With compliance set it writes the preset's settings. With it removed it
// deletes the pgaudit settings the operator wrote, and puts log_statement back
// to Spilo's value, but only when a pgaudit setting is still there to show the
// operator was the one that changed it.
func complianceParameterPatch(cluster *dbv1alpha1.PostgresCluster, live map[string]any) map[string]any {
	liveParams := liveParameters(live)
	changes := map[string]any{}

	if compliance := cluster.Spec.Compliance; compliance != nil {
		preset, ok := dbv1alpha1.PresetFor(compliance.Standard)
		if !ok {
			return nil
		}
		for name, want := range preset.Parameters(compliance.AuditLogs.LogParameters) {
			if got, present := liveParams[name]; !present || fmt.Sprint(got) != want {
				changes[name] = want
			}
		}
	} else {
		leftover := false
		for _, name := range pgauditParameters() {
			if _, present := liveParams[name]; present {
				changes[name] = nil
				leftover = true
			}
		}
		if leftover && fmt.Sprint(liveParams[paramLogStatement]) == logStatementNone {
			changes[paramLogStatement] = spiloLogStatement
		}
	}

	if len(changes) == 0 {
		return nil
	}
	return map[string]any{dcsPostgresql: map[string]any{dcsParameters: changes}}
}

// liveParameters reads postgresql.parameters out of Patroni's configuration.
func liveParameters(live map[string]any) map[string]any {
	postgresql, _ := live[dcsPostgresql].(map[string]any)
	params, _ := postgresql[dcsParameters].(map[string]any)
	return params
}

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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const rewindRoleName = "pgoperator_rewind"

// replicationRoleScenario limits the replication role on the main cluster and
// checks what that is for: the role can replicate and nothing else, pg_rewind
// has a login of its own with only what it needs, and Patroni really uses it
// when a former primary that has written on its own has to be brought back.
func replicationRoleScenario(healthy func(Gomega), podUIDs func() string) {
	DeferCleanup(func() {
		_, _ = inCluster("patch", "postgrescluster", clusterName, "--type=merge", "-p", `{"spec":{"security":null}}`)
	})
	primaryNow := func() string {
		primary, err := clusterField(".status.primaryInstance")
		ExpectWithOffset(2, err).NotTo(HaveOccurred())
		return primary
	}
	rewindRole := func() string {
		state, err := clusterField(".status.accessControl.rewindRole.state")
		ExpectWithOffset(2, err).NotTo(HaveOccurred())
		return state
	}
	asReplicator := func(pod string) string {
		out, _ := runInPod(pod, `PGPASSWORD="$PGPASSWORD_STANDBY" psql -X -At "host=$(hostname -i) user=replicator dbname=postgres sslmode=require" -c "select 'in'" 2>&1 | head -1`)
		return strings.TrimSpace(out)
	}

	By("limiting the replication role: every pod is replaced to start with the new rules, and the login for pg_rewind is made")
	before := podUIDs()
	_, err := inCluster("patch", "postgrescluster", clusterName, "--type=merge", "-p", `{"spec":{"security":{"restrictReplicationRole":true}}}`)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	EventuallyWithOffset(1, func(g Gomega) {
		for uid := range strings.FieldsSeq(before) {
			g.Expect(podUIDs()).NotTo(ContainSubstring(uid), "every pod was replaced")
		}
		g.Expect(rewindRole()).To(Equal("Ready"))
		healthy(g)
	}, clusterUpTimeout+5*time.Minute, pollEvery).Should(Succeed())

	By("seeing the login can do nothing but read the files pg_rewind reads")
	primary := primaryNow()
	attributes, err := psql(primary, "", "select rolsuper, rolcreatedb, rolcreaterole, rolreplication, rolbypassrls, rolcanlogin, rolconnlimit "+
		"from pg_roles where rolname = '"+rewindRoleName+"'")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	ExpectWithOffset(1, attributes).To(Equal("f|f|f|f|f|t|3"))
	functions, err := psql(primary, "", "select count(*) from pg_proc p where p.pronamespace = 'pg_catalog'::regnamespace "+
		"and p.proname in ('pg_ls_dir', 'pg_stat_file', 'pg_read_binary_file') "+
		"and has_function_privilege('"+rewindRoleName+"', p.oid, 'execute')")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	ExpectWithOffset(1, functions).To(Equal("4"), "the four functions pg_rewind calls")
	tables, err := psql(primary, "", "select count(*) from information_schema.table_privileges where grantee = '"+rewindRoleName+"'")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	ExpectWithOffset(1, tables).To(Equal("0"))

	By("seeing the replication role refused everything but replication, while the replicas go on streaming")
	ExpectWithOffset(1, asReplicator(primary)).To(ContainSubstring("pg_hba.conf rejects connection"))
	rules, err := psql(primary, "", "select count(*) from pg_hba_file_rules where auth_method = 'pam'")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	ExpectWithOffset(1, rules).To(Equal("0"), "the image's PAM rules are not there")
	EventuallyWithOffset(1, healthy, 2*time.Minute, pollEvery).Should(Succeed())

	By("making a former primary write on its own, as one that lost its lock to a replica would")
	old := primaryNow()
	_, err = psql(old, "", "create table if not exists e2e_diverged(t text)")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	patroniPID, err := runInPod(old, `ps -eo pid,args | grep "[p]atroni /home/postgres/postgres.yml" | head -1 | cut -c1-7 | tr -d ' '`)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	patroniPID = strings.TrimSpace(patroniPID)
	ExpectWithOffset(1, patroniPID).NotTo(BeEmpty())
	time.Sleep(3 * time.Second) // the table reaches the replicas
	_, err = runInPod(old, `kill -STOP "$1"`, patroniPID)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	resumed := false
	resume := func() {
		if !resumed {
			_, _ = runInPod(old, `kill -CONT "$1" || true`, patroniPID)
			resumed = true
		}
	}
	DeferCleanup(resume)

	// With Patroni frozen it cannot renew the lock, and a replica takes over.
	// The old primary's Postgres is still a primary, and writes that no replica
	// will ever have. The write is made as soon as there is a new primary, which
	// is well before the kubelet gives up on the frozen Patroni and restarts it.
	EventuallyWithOffset(1, func() (string, error) {
		return inCluster("get", "pods", "-l", mainRole("primary"), "-o", "name")
	}, 2*time.Minute, 2*time.Second).Should(And(ContainSubstring("pod/"), Satisfy(func(names string) bool {
		return len(strings.Fields(names)) == 2
	})), "a replica took over while the old primary still believes it is one")
	EventuallyWithOffset(1, func() (string, error) {
		return psql(old, "", "insert into e2e_diverged select md5(g::text) from generate_series(1, 3000) g")
	}, 30*time.Second, time.Second).Should(ContainSubstring("INSERT 0 3000"))
	_, err = psql(old, "", "checkpoint")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	resume()

	By("seeing Patroni bring it back with pg_rewind, as the login made for that, and the writes thrown away")
	rewound := func() string {
		current, _ := inCluster("logs", old, "-c", postgresContainer)
		previous, _ := inCluster("logs", old, "-c", postgresContainer, "--previous")
		return current + previous
	}
	EventuallyWithOffset(1, rewound, 4*time.Minute, 5*time.Second).Should(
		MatchRegexp(`running pg_rewind from dbname=postgres user=`+rewindRoleName+` `), "not as the superuser")
	EventuallyWithOffset(1, rewound, 2*time.Minute, 5*time.Second).Should(ContainSubstring("pg_rewind exit code=0"))
	EventuallyWithOffset(1, healthy, 3*time.Minute, pollEvery).Should(Succeed())
	primary = primaryNow()
	ExpectWithOffset(1, primary).NotTo(Equal(old))
	rows, err := psql(primary, "", "select count(*) from e2e_diverged")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	ExpectWithOffset(1, rows).To(Equal("0"), "what the old primary wrote on its own did not survive")

	By("taking the limits away: every pod goes back to the image's rules, and the login loses its right to log in but is not dropped")
	before = podUIDs()
	_, err = inCluster("patch", "postgrescluster", clusterName, "--type=merge", "-p", `{"spec":{"security":null}}`)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	EventuallyWithOffset(1, func(g Gomega) {
		for uid := range strings.FieldsSeq(before) {
			g.Expect(podUIDs()).NotTo(ContainSubstring(uid), "every pod was replaced")
		}
		g.Expect(rewindRole()).To(Equal("Disabled"))
		healthy(g)
	}, clusterUpTimeout+5*time.Minute, pollEvery).Should(Succeed())
	primary = primaryNow()
	login, err := psql(primary, "", "select rolcanlogin from pg_roles where rolname = '"+rewindRoleName+"'")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	ExpectWithOffset(1, login).To(Equal("f"), "the role is still there, and cannot log in")
	ExpectWithOffset(1, asReplicator(primary)).To(Equal("in"), "the replication role can log in as it could before")
}

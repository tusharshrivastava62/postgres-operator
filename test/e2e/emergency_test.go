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
	"bytes"
	"fmt"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const requestedByAnnotation = "db.youroperator.io/requested-by"

// whoAmI is the user the API server knows kubectl as.
func whoAmI() string {
	out, err := kubectlOut("auth", "whoami", "-o", "jsonpath={.status.userInfo.username}")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	ExpectWithOffset(1, out).NotTo(BeEmpty())
	return out
}

// applyShowingWarnings applies a manifest and returns what kubectl printed on
// standard error, where it puts an admission webhook's warnings, with any error.
func applyShowingWarnings(manifest string) (string, error) {
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	_, err := cmd.Output()
	if err != nil {
		// The reason is on standard error, and is what a test looks for.
		return stderr.String(), fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stderr.String(), nil
}

func emergencyManifest(name, duration, reason string, annotations map[string]string) string {
	var notes strings.Builder
	if len(annotations) > 0 {
		notes.WriteString("  annotations:\n")
		for k, v := range annotations {
			fmt.Fprintf(&notes, "    %s: %q\n", k, v)
		}
	}
	return fmt.Sprintf(`apiVersion: db.youroperator.io/v1alpha1
kind: PostgresEmergencyAccess
metadata:
  name: %s
  namespace: %s
%sspec:
  clusterRef: {name: %s}
  duration: %s
  reason: %q
`, name, clusterNamespace, notes.String(), clusterName, duration, reason)
}

func requestField(name, path string) string {
	out, err := inCluster("get", "postgresemergencyaccess", name, "-o", "jsonpath={"+path+"}")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return out
}

func requestState(name string) func() (string, error) {
	return func() (string, error) {
		return inCluster("get", "postgresemergencyaccess", name, "-o", "jsonpath={.status.state}")
	}
}

// asSuperuserIn runs one statement in a database of the cluster as the
// superuser, from a pod, through the pod's own socket.
func asSuperuserIn(pod, database, sql string) string {
	out, err := runInPod(pod, fmt.Sprintf(`PGPASSWORD="$PGPASSWORD_SUPERUSER" psql -U postgres -d %s -tAX -c %q 2>&1 || true`, database, sql))
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return strings.TrimSpace(out)
}

// roleCount is how many roles of a name the database has.
func roleCount(pod, role string) string {
	return asSuperuserIn(pod, "postgres", fmt.Sprintf("select count(*) from pg_roles where rolname = '%s'", role))
}

// latestLog is the newest server log of a pod.
func latestLog(pod string) string {
	out, err := runInPod(pod, `cat "$(ls -t /home/postgres/pgdata/pgroot/pg_log/*.csv | head -1)"`)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return out
}

// loginAs runs a script of SQL as an emergency role, in one connection, so that
// an escalation lasts through it.
func loginAs(pod, secret, database, script string) string {
	user, err := secretValue(secret, "username")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	password, err := secretValue(secret, "password")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	out, err := runInPod(pod, fmt.Sprintf("PGPASSWORD='%s' PGSSLMODE=require psql -X -At -h %s -U %s -d %s 2>&1 <<'SQL' || true\n%s\nSQL\n",
		password, clusterName, user, database, script))
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return strings.TrimSpace(out)
}

// emergencyAccessScenario exercises emergency access on the main cluster, which
// has a compliance preset on, a database called orders and a table in it that
// the role of a request can not read until it escalates.
func emergencyAccessScenario(primary string) {
	const (
		request = "e2e-ea"
		secret  = request + "-access"
		reason  = "orders database is out of connections and the application is down"
	)
	me := whoAmI()
	cleanup := func(names ...string) {
		for _, n := range names {
			_, _ = inCluster("delete", "postgresemergencyaccess", n, "--wait=false", "--ignore-not-found")
		}
	}
	DeferCleanup(func() {
		cleanup(request, request+"-del", request+"-off", request+"-refused")
		_, _ = inCluster("patch", "postgrescluster", clusterName, "--type=merge", "-p", `{"spec":{"emergencyAccess":null}}`)
	})

	By("refusing a request while the cluster does not allow emergency access, and saying what to set")
	_, err := applyShowingWarnings(emergencyManifest(request+"-refused", "5m", reason, nil))
	ExpectWithOffset(1, err).To(HaveOccurred())
	ExpectWithOffset(1, err.Error()).To(ContainSubstring("spec.emergencyAccess.enabled"))

	By("allowing it on the cluster, for at most ten minutes")
	_, err = inCluster("patch", "postgrescluster", clusterName, "--type=merge", "-p",
		`{"spec":{"emergencyAccess":{"enabled":true,"maxDuration":"10m"}}}`)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())

	By("refusing a request for longer than that, and one with no reason")
	_, err = applyShowingWarnings(emergencyManifest(request+"-refused", "30m", reason, nil))
	ExpectWithOffset(1, err).To(HaveOccurred())
	ExpectWithOffset(1, err.Error()).To(ContainSubstring("spec.duration"))
	_, err = applyShowingWarnings(emergencyManifest(request+"-refused", "5m", "urgent", nil))
	ExpectWithOffset(1, err).To(HaveOccurred())
	ExpectWithOffset(1, err.Error()).To(ContainSubstring("reason"))

	By("warning the person who asks, before anything, that their statements and values are logged in clear text")
	warnings, err := applyShowingWarnings(emergencyManifest(request, "3m", reason,
		map[string]string{requestedByAnnotation: "somebody-else"}))
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	ExpectWithOffset(1, warnings).To(ContainSubstring("WARNING: While you are escalated"))
	ExpectWithOffset(1, warnings).To(ContainSubstring("clear text"))

	By("recording who asked from the API call, not from the manifest")
	ExpectWithOffset(1, requestField(request, `.metadata.annotations.db\.youroperator\.io/requested-by`)).To(Equal(me))
	EventuallyWithOffset(1, requestState(request), 3*time.Minute, pollEvery).Should(Equal("Active"))
	ExpectWithOffset(1, requestField(request, ".status.requestedBy")).To(Equal(me))
	role := requestField(request, ".status.role")
	ExpectWithOffset(1, role).To(MatchRegexp(`^bg_[0-9a-f]{8}$`))
	ExpectWithOffset(1, requestField(request, ".status.message")).To(ContainSubstring("WARNING:"))
	ExpectWithOffset(1, requestField(request, ".status.expiresAt")).NotTo(BeEmpty())
	EventuallyWithOffset(1, func() (string, error) { return conditionStatus("EmergencyAccessActive") }, time.Minute, pollEvery).Should(Equal("True"))
	username, err := secretValue(secret, "username")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	ExpectWithOffset(1, username).To(Equal(role))
	notice, err := secretValue(secret, "warning")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	ExpectWithOffset(1, notice).To(ContainSubstring("clear text"))

	By("a login that can do nothing until it escalates")
	out := loginAs(primary, secret, "orders", "select count(*) from orders_t;")
	ExpectWithOffset(1, out).To(ContainSubstring("permission denied for table orders_t"))

	By("a session that is open when the time runs out")
	user, err := secretValue(secret, "username")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	password, err := secretValue(secret, "password")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	_, err = runInPod(primary, fmt.Sprintf(
		`PGPASSWORD='%s' PGSSLMODE=require nohup psql -X -At -h %s -U %s -d orders -c "select pg_sleep(900)" >/tmp/ea-session.out 2>&1 &`,
		password, clusterName, user))
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	EventuallyWithOffset(1, func() string {
		return asSuperuserIn(primary, "postgres", fmt.Sprintf("select count(*) from pg_stat_activity where usename = '%s'", role))
	}, time.Minute, pollEvery).Should(Equal("1"))

	By("escalating in a database the role was granted nothing in beforehand, and working as the superuser")
	out = loginAs(primary, secret, "orders", `select set_user_u('postgres');
select current_user || '|' || session_user || '|' || (select count(*) from orders_t);
create table ea_made (i int);
select $1::text \bind 'e2e-bound-value' \g
select reset_user();`)
	ExpectWithOffset(1, out).To(MatchRegexp(`postgres\|` + role + `\|\d+`))
	ExpectWithOffset(1, asSuperuserIn(primary, "orders", "select tableowner from pg_tables where tablename = 'ea_made'")).To(Equal("postgres"))

	By("a connection to the postgres database starts unescalated: the escalation is per connection")
	out = loginAs(primary, secret, "postgres", `select current_user;`)
	ExpectWithOffset(1, strings.TrimSpace(out)).To(Equal(role))

	By("recording it in the server log: the way in, the statements, and the values bound to them in clear text")
	EventuallyWithOffset(1, func(g Gomega) {
		log := latestLog(primary)
		g.Expect(log).To(ContainSubstring("Role " + role + " transitioning to Superuser Role postgres"))
		g.Expect(log).To(ContainSubstring("Superuser Role postgres transitioning to Role " + role))
		g.Expect(log).To(MatchRegexp(`"` + role + `".*statement: create table ea_made`))
		g.Expect(log).To(ContainSubstring("e2e-bound-value"), "a bound value is in the log, which the warning says")
	}, time.Minute, pollEvery).Should(Succeed())

	By("pgaudit not recording the escalated session's reads, which is why the documentation says to search for the role")
	log := latestLog(primary)
	for line := range strings.SplitSeq(log, "\n") {
		if strings.Contains(line, `"`+role+`"`) && strings.Contains(line, "AUDIT: SESSION") && strings.Contains(line, "from orders_t") {
			Fail("pgaudit recorded an escalated read, so the documentation is out of date: " + line)
		}
	}
	_, err = inCluster("exec", primary, "-c", "audit-shipper", "--", "python3", "/scripts/audit_shipper.py")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	shipped := verifyShippedAuditLog(primary, "Role "+role+" transitioning to Superuser Role postgres", "statement: create table ea_made")
	for marker, found := range shipped.Markers {
		ExpectWithOffset(1, found).To(BeTrue(), "the audit bucket has %q", marker)
	}

	By("ending by itself when the time is up: the login, the sessions, the role and the Secret all go")
	EventuallyWithOffset(1, requestState(request), 6*time.Minute, pollEvery).Should(Equal("Expired"))
	ExpectWithOffset(1, requestField(request, ".status.endedAt")).NotTo(BeEmpty())
	ExpectWithOffset(1, requestField(request, ".status.message")).To(ContainSubstring("Expired"))
	ExpectWithOffset(1, roleCount(primary, role)).To(Equal("0"))
	ExpectWithOffset(1, asSuperuserIn(primary, "postgres", fmt.Sprintf("select count(*) from pg_stat_activity where usename = '%s'", role))).To(Equal("0"))
	_, err = inCluster("get", "secret", secret)
	ExpectWithOffset(1, err).To(HaveOccurred(), "the login is deleted")
	out, err = runInPod(primary, "cat /tmp/ea-session.out")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	ExpectWithOffset(1, out).To(ContainSubstring("terminating connection"), "the open session was ended")
	ExpectWithOffset(1, asSuperuserIn(primary, "orders", "select tableowner from pg_tables where tablename = 'ea_made'")).To(Equal("postgres"),
		"what was made while escalated stays")
	EventuallyWithOffset(1, func() (string, error) { return conditionStatus("EmergencyAccessActive") }, 2*time.Minute, pollEvery).Should(Equal("False"))
	events, err := inCluster("get", "events.events.k8s.io", "--field-selector", "regarding.name="+request, "-o", "jsonpath={.items[*].reason}")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	ExpectWithOffset(1, events).To(And(ContainSubstring("AccessGranted"), ContainSubstring("AccessEnded")))

	By("taking the access away when the request is deleted, before it goes")
	_, err = applyShowingWarnings(emergencyManifest(request+"-del", "5m", reason, nil))
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	EventuallyWithOffset(1, requestState(request+"-del"), 3*time.Minute, pollEvery).Should(Equal("Active"))
	deletedRole := requestField(request+"-del", ".status.role")
	ExpectWithOffset(1, roleCount(primary, deletedRole)).To(Equal("1"))
	_, err = inCluster("delete", "postgresemergencyaccess", request+"-del", "--timeout=3m")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	ExpectWithOffset(1, roleCount(primary, deletedRole)).To(Equal("0"))
	_, err = inCluster("get", "secret", request+"-del-access")
	ExpectWithOffset(1, err).To(HaveOccurred())

	By("taking the access away at once when the cluster stops allowing it")
	_, err = applyShowingWarnings(emergencyManifest(request+"-off", "5m", reason, nil))
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	EventuallyWithOffset(1, requestState(request+"-off"), 3*time.Minute, pollEvery).Should(Equal("Active"))
	offRole := requestField(request+"-off", ".status.role")
	_, err = inCluster("patch", "postgrescluster", clusterName, "--type=merge", "-p", `{"spec":{"emergencyAccess":{"enabled":false}}}`)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	EventuallyWithOffset(1, requestState(request+"-off"), 3*time.Minute, pollEvery).Should(Equal("Revoked"))
	ExpectWithOffset(1, roleCount(primary, offRole)).To(Equal("0"))

	By("leaving no emergency role behind")
	ExpectWithOffset(1, asSuperuserIn(primary, "postgres", "select count(*) from pg_roles where rolname like 'bg\\_%'")).To(Equal("0"))
}

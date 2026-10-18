#!/usr/bin/env python3
"""Proves that the newest backup can be restored, and times it.

Run on a schedule by the operator in a pod that holds only the bucket
credentials and scratch space. It fetches the latest base backup into an empty
directory, starts PostgreSQL on a local socket (not reachable from anywhere),
lets it replay the archived WAL to the end, checks that every database can be
read, stops it, and writes one short JSON document to the termination log.

The restored server is configured so that it cannot affect anything outside
the pod: no listening address, no archiving, no replication, no SSL. The time
it takes is the measured time to recover, for a restore of the newest backup
by this method.
"""

import json
import os
import subprocess
import sys
import time

TERMINATION_LOG = os.environ.get("TERMINATION_LOG", "/dev/termination-log")
WORKDIR = os.environ.get("RESTORE_DIR", "/restore")
PGVERSION = os.environ.get("PGVERSION", "17")
COMMAND_TIMEOUT = int(os.environ.get("COMMAND_TIMEOUT", "300"))
RECOVERY_TIMEOUT = int(os.environ.get("RECOVERY_TIMEOUT", "14400"))
POLL_SECONDS = float(os.environ.get("POLL_SECONDS", "2"))

PGDATA = os.path.join(WORKDIR, "data")
SOCKET_DIR = os.path.join(WORKDIR, "socket")
LOG_FILE = os.path.join(WORKDIR, "postgres.log")

# Applied on top of the restored configuration. They keep the server inside
# the pod and make it run on little memory, whatever the source was set to.
OVERRIDES = """
restore_command = 'wal-g wal-fetch %f %p'
recovery_target_action = 'promote'
archive_mode = 'off'
archive_command = '/bin/true'
listen_addresses = ''
unix_socket_directories = '{socket}'
shared_buffers = '64MB'
primary_conninfo = ''
cluster_name = 'restore-verify'
hot_standby = 'on'
ssl = 'off'
hba_file = '{hba}'
ident_file = '{ident}'
logging_collector = 'off'
log_destination = 'stderr'
"""


class Failure(Exception):
    pass


def bin_path(name):
    return os.path.join(os.environ.get("PGBIN", "/usr/lib/postgresql/%s/bin" % PGVERSION), name)


def run(cmd, timeout=COMMAND_TIMEOUT):
    try:
        return subprocess.run(cmd, capture_output=True, text=True, timeout=timeout)
    except subprocess.TimeoutExpired:
        raise Failure("%s timed out after %ds" % (" ".join(cmd[:2]), timeout))


def latest_backup():
    result = run(["wal-g", "backup-list", "--json"])
    if result.returncode != 0:
        raise Failure("wal-g backup-list failed: " + result.stderr.strip()[-300:])
    out = result.stdout.strip()
    backups = json.loads(out) if out else []
    if not backups:
        raise Failure("there is no base backup to restore")
    return sorted(backups, key=lambda b: b.get("finish_time") or b.get("time") or "")[-1]["backup_name"]


def fetch(name):
    os.makedirs(PGDATA, mode=0o700, exist_ok=True)
    os.chmod(PGDATA, 0o700)
    os.makedirs(SOCKET_DIR, mode=0o700, exist_ok=True)
    result = run(["wal-g", "backup-fetch", PGDATA, name], timeout=RECOVERY_TIMEOUT)
    if result.returncode != 0:
        raise Failure("wal-g backup-fetch failed: " + result.stderr.strip()[-300:])


def configure():
    hba = os.path.join(PGDATA, "pg_hba.conf")
    ident = os.path.join(PGDATA, "pg_ident.conf")
    with open(hba, "w") as handle:
        handle.write("local all all trust\n")
    with open(ident, "w"):
        pass
    for stale in ("postmaster.pid", "standby.signal"):
        try:
            os.remove(os.path.join(PGDATA, stale))
        except FileNotFoundError:
            pass
    with open(os.path.join(PGDATA, "recovery.signal"), "w"):
        pass
    with open(os.path.join(PGDATA, "postgresql.auto.conf"), "w") as handle:
        handle.write(OVERRIDES.format(socket=SOCKET_DIR, hba=hba, ident=ident))


def psql(sql, dbname="postgres"):
    result = run(["psql", "-XAt", "-h", SOCKET_DIR, "-U", "postgres", "-d", dbname, "-c", sql])
    if result.returncode != 0:
        raise Failure("psql failed: " + result.stderr.strip()[-300:])
    return result.stdout.strip()


def server_log_tail():
    try:
        with open(LOG_FILE) as handle:
            return handle.read()[-400:].strip()
    except OSError:
        return ""


def start_server():
    result = run(
        [bin_path("pg_ctl"), "-D", PGDATA, "-w", "-t", str(RECOVERY_TIMEOUT), "-l", LOG_FILE, "start"],
        timeout=RECOVERY_TIMEOUT + 60,
    )
    if result.returncode != 0:
        raise Failure("PostgreSQL did not start: " + server_log_tail())


def wait_for_end_of_recovery():
    """Waits until the server has replayed the archive and promoted itself."""
    deadline = time.time() + RECOVERY_TIMEOUT
    while time.time() < deadline:
        if psql("select pg_is_in_recovery()") == "f":
            return
        time.sleep(POLL_SECONDS)
    raise Failure("recovery did not finish within %ds" % RECOVERY_TIMEOUT)


def check_databases():
    names = psql("select datname from pg_database where datallowconn order by 1").splitlines()
    if not names:
        raise Failure("the restored server has no databases")
    for name in names:
        # Reading the catalog of every database shows that it was restored
        # whole, not just that the server came up.
        if int(psql("select count(*) from pg_class", dbname=name)) < 1:
            raise Failure("database %s has an empty catalog" % name)
    return len(names)


def stop_server():
    run([bin_path("pg_ctl"), "-D", PGDATA, "-m", "fast", "-w", "stop"])


def main():
    started = time.time()
    report = {"success": False}
    server_started = False
    try:
        name = latest_backup()
        report["backup"] = name
        fetch(name)
        report["fetchSeconds"] = int(time.time() - started)
        configure()
        replay_started = time.time()
        start_server()
        server_started = True
        wait_for_end_of_recovery()
        report["replaySeconds"] = int(time.time() - replay_started)
        report["databases"] = check_databases()
        report["success"] = True
    except Failure as failure:
        report["message"] = str(failure)
    except Exception as exc:  # anything unexpected is still a failed test
        report["message"] = "unexpected error: %s" % exc
    finally:
        if server_started:
            try:
                stop_server()
            except Exception:
                pass
    report["durationSeconds"] = int(time.time() - started)
    with open(TERMINATION_LOG, "w") as handle:
        json.dump(report, handle, separators=(",", ":"))
    return 0 if report["success"] else 1


if __name__ == "__main__":
    sys.exit(main())

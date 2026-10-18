#!/usr/bin/env python3
"""Reads a cluster's backups back and applies its retention policy.

Run on a schedule by the operator, in a pod that holds only the bucket and
database credentials. It lists the base backups (wal-g), reads the WAL
archiver's counters and write position (psql, through the read-write Service),
deletes backups past retention, and writes one short JSON document to the
termination log, which is how the operator hears the result.

Failures of one step are recorded in the report instead of ending the run, so
a bucket that is down still reports on the database and vice versa. Nothing
is deleted unless the backup listing succeeded.
"""

import datetime
import json
import os
import subprocess
import sys

TERMINATION_LOG = os.environ.get("TERMINATION_LOG", "/dev/termination-log")
RETAIN_FULL = int(os.environ.get("RETAIN_FULL", "7"))
RETAIN_DAYS = int(os.environ.get("RETAIN_DAYS", "14"))
COMMAND_TIMEOUT = int(os.environ.get("COMMAND_TIMEOUT", "120"))

ARCHIVER_QUERY = (
    "select archived_count, failed_count, coalesce(last_archived_wal, ''), "
    "coalesce(extract(epoch from last_archived_time)::bigint, 0), "
    "pg_current_wal_lsn()::text, "
    "(select setting::bigint from pg_settings where name = 'wal_segment_size') "
    "from pg_stat_archiver"
)


def run(cmd):
    try:
        return subprocess.run(cmd, capture_output=True, text=True, timeout=COMMAND_TIMEOUT)
    except subprocess.TimeoutExpired:
        # A store or database that does not answer is the common failure; say
        # so plainly instead of printing the whole command line.
        raise RuntimeError("%s timed out after %ds" % (" ".join(cmd[:2]), COMMAND_TIMEOUT))


def parse_time(value):
    """Parses wal-g's RFC 3339 timestamps, with or without fractional seconds."""
    value = value.strip().replace("Z", "+00:00")
    if "." in value:
        head, rest = value.split(".", 1)
        digits = ""
        for ch in rest:
            if ch.isdigit():
                digits += ch
            else:
                break
        value = head + "." + digits[:6].ljust(6, "0") + rest[len(digits):]
    parsed = datetime.datetime.fromisoformat(value)
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=datetime.timezone.utc)
    return parsed.astimezone(datetime.timezone.utc)


def iso(moment):
    return moment.strftime("%Y-%m-%dT%H:%M:%SZ")


def list_backups():
    result = run(["wal-g", "backup-list", "--detail", "--json"])
    if result.returncode != 0:
        raise RuntimeError("wal-g backup-list failed: " + result.stderr.strip()[-300:])
    out = result.stdout.strip()
    raw = json.loads(out) if out else []
    backups = []
    for item in raw or []:
        finished = item.get("finish_time") or item.get("time")
        backups.append(
            {
                "name": item["backup_name"],
                "time": parse_time(finished),
                "size": int(item.get("compressed_size") or 0),
            }
        )
    backups.sort(key=lambda b: b["time"], reverse=True)
    return backups


def backups_to_keep(backups, now):
    """Newest RETAIN_FULL backups, plus every backup newer than RETAIN_DAYS."""
    cutoff = now - datetime.timedelta(days=RETAIN_DAYS)
    keep = [b for i, b in enumerate(backups) if i < RETAIN_FULL or b["time"] >= cutoff]
    return keep


def apply_retention(backups, now):
    """Deletes what falls outside the policy and returns how many were deleted."""
    keep = backups_to_keep(backups, now)
    doomed = len(backups) - len(keep)
    if doomed <= 0:
        return 0
    # Everything before the oldest backup to keep goes, together with the WAL
    # that only those backups needed.
    oldest_kept = keep[-1]["name"]
    result = run(["wal-g", "delete", "before", "FIND_FULL", oldest_kept, "--confirm"])
    if result.returncode != 0:
        raise RuntimeError("wal-g delete failed: " + result.stderr.strip()[-300:])
    return doomed


def read_archiver():
    result = run(["psql", "-XAt", "-F", "|", "-c", ARCHIVER_QUERY])
    if result.returncode != 0:
        raise RuntimeError("psql failed: " + result.stderr.strip()[-300:])
    fields = result.stdout.strip().split("|")
    if len(fields) != 6:
        raise RuntimeError("unexpected psql output: " + result.stdout.strip()[:200])
    return {
        "archived": int(fields[0]),
        "failed": int(fields[1]),
        "lastArchivedWal": fields[2],
        "lastArchivedEpoch": int(fields[3]),
        "currentLsn": fields[4],
        "segmentBytes": int(fields[5]),
    }


def main():
    now = datetime.datetime.now(datetime.timezone.utc)
    report = {"errors": []}

    backups = None
    try:
        backups = list_backups()
        summary = {"count": len(backups)}
        if backups:
            newest, oldest = backups[0], backups[-1]
            summary["newest"] = {
                "name": newest["name"],
                "time": iso(newest["time"]),
                "sizeBytes": newest["size"],
            }
            summary["oldest"] = {"name": oldest["name"], "time": iso(oldest["time"])}
        report["backups"] = summary
    except Exception as exc:  # one failing step must not hide the others
        report["errors"].append(str(exc))

    try:
        report["archiver"] = read_archiver()
    except Exception as exc:
        report["errors"].append(str(exc))

    if backups is not None:
        try:
            deleted = apply_retention(backups, now)
            report["retention"] = {"deleted": deleted}
            if deleted and "backups" in report:
                report["backups"]["count"] = len(backups) - deleted
                kept = backups_to_keep(backups, now)
                report["backups"]["oldest"] = {"name": kept[-1]["name"], "time": iso(kept[-1]["time"])}
        except Exception as exc:
            report["errors"].append(str(exc))

    with open(TERMINATION_LOG, "w") as handle:
        json.dump(report, handle, separators=(",", ":"))
    return 0


if __name__ == "__main__":
    sys.exit(main())

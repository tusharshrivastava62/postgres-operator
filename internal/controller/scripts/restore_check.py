#!/usr/bin/env python3
"""Checks that a new cluster can be restored from the backups it names.

Run once by the operator, before any database pod exists, in a pod that holds
only the source's bucket credentials. It lists the source's base backups
(wal-g) and applies the rule Spilo's clone script applies when it picks the
backup to start from: the newest backup written before the target time, or
the newest backup when there is no target. It writes the verdict to the
termination log, which is how the operator hears it.

Without this, a target earlier than every backup, a wrong path or a wrong key
only shows up as a database pod that exits during bootstrap and restarts, with
the reason in a log nothing reads.

The verdict is one of: ok, unreadable, no-backups, target-before-oldest. The
script itself always exits 0 once it has written one.
"""

import datetime
import json
import os
import subprocess
import sys

TERMINATION_LOG = os.environ.get("TERMINATION_LOG", "/dev/termination-log")
COMMAND_TIMEOUT = int(os.environ.get("COMMAND_TIMEOUT", "120"))
TARGET_TIME = os.environ.get("TARGET_TIME", "")


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
    try:
        result = subprocess.run(
            ["wal-g", "backup-list", "--detail", "--json"], capture_output=True, text=True, timeout=COMMAND_TIMEOUT
        )
    except subprocess.TimeoutExpired:
        raise RuntimeError("wal-g timed out after %ds" % COMMAND_TIMEOUT)
    if result.returncode != 0:
        raise RuntimeError("wal-g backup-list failed: " + result.stderr.strip()[-300:])
    out = result.stdout.strip()
    backups = []
    for item in (json.loads(out) if out else []) or []:
        # "time" is when the backup's record was written to the bucket, the
        # "last_modified" Spilo's clone script compares with the target.
        stamp = item.get("time") or item.get("finish_time")
        backups.append({"name": item["backup_name"], "time": parse_time(stamp)})
    backups.sort(key=lambda b: b["time"])
    return backups


def check():
    try:
        backups = list_backups()
    except Exception as exc:
        return {"verdict": "unreadable", "error": str(exc)}
    if not backups:
        return {"verdict": "no-backups"}
    oldest = {"name": backups[0]["name"], "time": iso(backups[0]["time"])}
    if TARGET_TIME:
        target = parse_time(TARGET_TIME)
        usable = [b for b in backups if b["time"] < target]
        if not usable:
            return {"verdict": "target-before-oldest", "oldest": oldest}
        chosen = usable[-1]
    else:
        chosen = backups[-1]
    return {
        "verdict": "ok",
        "backup": {"name": chosen["name"], "time": iso(chosen["time"])},
        "oldest": oldest,
        "count": len(backups),
    }


def main():
    report = check()
    with open(TERMINATION_LOG, "w") as handle:
        json.dump(report, handle, separators=(",", ":"))
    return 0


if __name__ == "__main__":
    sys.exit(main())

#!/usr/bin/env python3
"""Reads the facts that decide a cluster's compliance controls from places the
operator cannot see: the running database, and the audit log bucket.

Run on a schedule by the operator, in a pod that holds the database
credentials and the audit destination's credentials. It reports what it finds,
not what it makes of it: whether each control is met is decided by the
operator, which also knows the spec. The report goes to the termination log,
which holds 4 KiB, so every list in it is capped.

  sql     the settings, connection rules and password hashes of the database
  bucket  how recently each pod shipped its audit log, and what the bucket's
          lifecycle rules and object lock say about keeping it

A part that could not be read says why instead of guessing, and the other part
is still reported.
"""

import datetime
import ipaddress
import json
import os
import subprocess
import sys

TERMINATION_LOG = os.environ.get("TERMINATION_LOG", "/dev/termination-log")
COMMAND_TIMEOUT = int(os.environ.get("COMMAND_TIMEOUT", "60"))
BUCKET = os.environ.get("AUDIT_BUCKET", "")
PREFIX = os.environ.get("AUDIT_PREFIX", "").strip("/")
ENDPOINT = os.environ.get("AUDIT_ENDPOINT", "") or None
REGION = os.environ.get("AUDIT_REGION", "") or "us-east-1"
PATH_STYLE = os.environ.get("AUDIT_FORCE_PATH_STYLE", "").lower() == "true"

# Caps, to keep the report inside the termination log.
MAX_PODS = 12
MAX_RULES = 12
MAX_LISTED = 4

SETTINGS = (
    "shared_preload_libraries",
    "pgaudit.log",
    "log_connections",
    "log_disconnections",
    "ssl",
    "password_encryption",
    "data_checksums",
)

# What the database can tell about itself, in one round trip.
QUERY = """
select json_build_object(
  'settings', (select coalesce(json_object_agg(name, setting), '{}'::json) from pg_settings where name in (%s)),
  'hba', (select coalesce(json_agg(json_build_object('type', type, 'auth', auth_method, 'db', database,
                                                      'user', user_name, 'addr', address, 'mask', netmask) order by line_number), '[]'::json)
          from pg_hba_file_rules where error is null),
  'hbaErrors', (select count(*) from pg_hba_file_rules where error is not null),
  'nonScramRoles', (select count(*) from pg_authid
                    where rolpassword is not null and rolpassword not like 'SCRAM-SHA-256$%%')
)
""" % ", ".join("'%s'" % name for name in SETTINGS)

# A connection that arrives without TLS can match these rule types.
PLAIN_TYPES = ("host", "hostnossl")
HOST_TYPES = ("host", "hostssl", "hostnossl", "hostgssenc", "hostnogssenc")
# Methods that let someone in without proving who they are, or send the
# password unprotected. md5 is not listed: it negotiates scram-sha-256 when
# the stored hash is one, and the roles' hashes are checked separately.
WEAK_METHODS = ("trust", "password", "ident")


def iso(moment):
    return moment.astimezone(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def describe(rule):
    """One rule, as a line of pg_hba.conf, short enough to read in a condition."""
    def flat(value):
        if isinstance(value, list):
            return ",".join(str(v) for v in value)
        return "" if value is None else str(value)

    return " ".join(part for part in (rule.get("type"), flat(rule.get("db")), flat(rule.get("user")),
                                      flat(rule.get("addr")), rule.get("auth")) if part)


def loopback_only(rule):
    """Whether a rule can only match connections made from inside the pod
    itself. Spilo has rules for 127.0.0.1 and ::1 for its own tools; a
    connection over loopback never crosses a network, so it is neither
    unencrypted in transit nor reachable by anyone else. The whole range the
    rule covers has to be loopback: an address of 127.0.0.1 with a mask that
    matches every address is not."""
    addr, mask = rule.get("addr"), rule.get("mask")
    if not addr:
        return False
    if addr == "localhost":
        return True
    try:
        address = ipaddress.ip_address(addr)
        prefix = address.max_prefixlen if not mask else bin(int(ipaddress.ip_address(mask))).count("1")
        return ipaddress.ip_network("%s/%d" % (addr, prefix), strict=False).is_loopback
    except ValueError:
        # A host name or a keyword such as samehost: not known to be loopback.
        return False


def read_sql():
    try:
        result = subprocess.run(
            ["psql", "-XAt", "-c", QUERY], capture_output=True, text=True, timeout=COMMAND_TIMEOUT
        )
    except subprocess.TimeoutExpired:
        return {"ok": False, "error": "psql timed out after %ds" % COMMAND_TIMEOUT}
    if result.returncode != 0:
        return {"ok": False, "error": "psql failed: " + result.stderr.strip()[-200:]}
    try:
        raw = json.loads(result.stdout.strip())
    except ValueError:
        return {"ok": False, "error": "unexpected psql output: " + result.stdout.strip()[:120]}

    rules = raw.get("hba") or []
    host_rules = [r for r in rules if r.get("type") in HOST_TYPES and not loopback_only(r)]
    return {
        "ok": True,
        "settings": raw.get("settings") or {},
        # Rules a connection without TLS can match, and what they do with it.
        "plainRules": [describe(r) for r in host_rules if r.get("type") in PLAIN_TYPES and r.get("auth") != "reject"][:MAX_LISTED],
        "plainRuleCount": len([r for r in host_rules if r.get("type") in PLAIN_TYPES and r.get("auth") != "reject"]),
        "weakRules": [describe(r) for r in host_rules if r.get("auth") in WEAK_METHODS][:MAX_LISTED],
        "weakRuleCount": len([r for r in host_rules if r.get("auth") in WEAK_METHODS]),
        "hbaErrors": int(raw.get("hbaErrors") or 0),
        "nonScramRoles": int(raw.get("nonScramRoles") or 0),
    }


def s3_client():
    import boto3
    from botocore.config import Config

    config = Config(s3={"addressing_style": "path" if PATH_STYLE else "auto"}, retries={"max_attempts": 2})
    return boto3.client("s3", endpoint_url=ENDPOINT, region_name=REGION, config=config)


def list_dirs(client, prefix):
    """The names one level below a prefix."""
    dirs, token = [], None
    while True:
        args = {"Bucket": BUCKET, "Prefix": prefix, "Delimiter": "/"}
        if token:
            args["ContinuationToken"] = token
        page = client.list_objects_v2(**args)
        dirs += [p["Prefix"] for p in page.get("CommonPrefixes", [])]
        if not page.get("IsTruncated"):
            return dirs
        token = page.get("NextContinuationToken")


def newest_in(client, prefix):
    """The most recent modification time under a prefix, and how many objects."""
    newest, count, token = None, 0, None
    while True:
        args = {"Bucket": BUCKET, "Prefix": prefix}
        if token:
            args["ContinuationToken"] = token
        page = client.list_objects_v2(**args)
        for item in page.get("Contents", []):
            count += 1
            if newest is None or item["LastModified"] > newest:
                newest = item["LastModified"]
        if not page.get("IsTruncated"):
            return newest, count
        token = page.get("NextContinuationToken")


def shipped_by_pod(client):
    """When each pod last shipped. A pod's log is one file per weekday, each
    a series of generations named by timestamp, so the newest chunks are in the
    last generation of some file: that is all that needs reading."""
    pods = {}
    for pod_dir in list_dirs(client, PREFIX + "/")[:MAX_PODS]:
        pod = pod_dir.rstrip("/").split("/")[-1]
        newest, chunks = None, 0
        for file_dir in list_dirs(client, pod_dir):
            generations = sorted(list_dirs(client, file_dir))
            if not generations:
                continue
            when, count = newest_in(client, generations[-1])
            chunks += count
            if when is not None and (newest is None or when > newest):
                newest = when
        pods[pod] = {"newest": iso(newest) if newest else None, "chunks": chunks}
    return pods


def lifecycle_rules(client):
    """The bucket's lifecycle rules, reduced to what retention depends on."""
    try:
        config = client.get_bucket_lifecycle_configuration(Bucket=BUCKET)
    except Exception as exc:
        code = getattr(exc, "response", {}).get("Error", {}).get("Code", "")
        if code == "NoSuchLifecycleConfiguration":
            return {"supported": True, "rules": []}
        return {"supported": False, "error": code or type(exc).__name__}
    rules = []
    for rule in config.get("Rules", []):
        filt = rule.get("Filter") or {}
        prefix = filt.get("Prefix")
        if prefix is None:
            prefix = (filt.get("And") or {}).get("Prefix", rule.get("Prefix", ""))
        rules.append({
            "id": (rule.get("ID") or "")[:40],
            "enabled": rule.get("Status") == "Enabled",
            "prefix": prefix or "",
            "days": (rule.get("Expiration") or {}).get("Days", 0),
        })
    return {"supported": True, "rules": rules[:MAX_RULES], "ruleCount": len(rules)}


def object_lock(client):
    """The bucket's default object lock retention, in days."""
    try:
        config = client.get_object_lock_configuration(Bucket=BUCKET)
    except Exception as exc:
        code = getattr(exc, "response", {}).get("Error", {}).get("Code", "")
        if code == "ObjectLockConfigurationNotFoundError":
            return {"supported": True, "days": 0}
        return {"supported": False, "error": code or type(exc).__name__}
    retention = ((config.get("ObjectLockConfiguration") or {}).get("Rule") or {}).get("DefaultRetention") or {}
    days = retention.get("Days") or (retention.get("Years") or 0) * 365
    return {"supported": True, "days": int(days), "mode": retention.get("Mode", "")}


def read_bucket():
    if not BUCKET:
        return {"ok": False, "error": "AUDIT_BUCKET is not set"}
    try:
        client = s3_client()
        pods = shipped_by_pod(client)
    except Exception as exc:
        return {"ok": False, "error": "%s: %s" % (type(exc).__name__, str(exc)[:160])}
    return {"ok": True, "pods": pods, "lifecycle": lifecycle_rules(client), "objectLock": object_lock(client)}


def main():
    report = {"sql": read_sql(), "bucket": read_bucket()}
    with open(TERMINATION_LOG, "w") as handle:
        json.dump(report, handle, separators=(",", ":"))
    return 0


if __name__ == "__main__":
    sys.exit(main())

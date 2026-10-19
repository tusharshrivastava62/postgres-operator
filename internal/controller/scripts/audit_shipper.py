#!/usr/bin/env python3
"""Ships a database pod's PostgreSQL log files, which hold the pgaudit lines,
to an S3 bucket.

The log is a CSV file on the data volume, one file per weekday, truncated when
its weekday comes round again. This runs every few minutes in a sidecar that
mounts the volume read-only. For each file it uploads the bytes not yet in the
bucket as one gzipped chunk, so the audit trail trails the database by minutes
instead of a day.

It keeps no state of its own. What has been shipped is read back from the
bucket on every run, so a restarted pod, a sidecar that crashed mid-upload, or
a bucket that was down all resume from exactly where the bucket stands, with
nothing uploaded twice and nothing skipped.

Objects are named
    <prefix>/<pod>/<log file>/<generation>/<start>-<end>.csv.gz
where start and end are byte offsets into the log file, zero-padded. A
generation starts when the file does: it is the timestamp of the file's first
line, so a file that is truncated and written again starts a new series at
offset zero and never overwrites the old one. Within a generation the chunks
are contiguous byte ranges, so joining them in order reproduces the file.

Each upload carries a Content-MD5, which S3 checks, and is then looked up to
confirm its size. The SHA-256 of the uncompressed bytes is kept in the object's
metadata.

Only a whole line is shipped, so a record being written is left for the next run.
"""

import base64
import gzip
import hashlib
import os
import re
import sys

LOG_DIR = os.environ.get("LOG_DIR", "/home/postgres/pgdata/pgroot/pg_log")
BUCKET = os.environ.get("AUDIT_BUCKET", "")
PREFIX = os.environ.get("AUDIT_PREFIX", "").strip("/")
POD = os.environ.get("POD_NAME", "")
ENDPOINT = os.environ.get("AUDIT_ENDPOINT", "") or None
REGION = os.environ.get("AUDIT_REGION", "") or "us-east-1"
PATH_STYLE = os.environ.get("AUDIT_FORCE_PATH_STYLE", "").lower() == "true"
# One upload carries at most this much of the log, before compression.
MAX_CHUNK = int(os.environ.get("MAX_CHUNK_BYTES", str(8 << 20)))
# One run ships at most this much from any one file, so a long backlog cannot
# make a run last for ever; the next run carries on.
MAX_PER_RUN = int(os.environ.get("MAX_BYTES_PER_RUN", str(64 << 20)))

CHUNK_KEY = re.compile(r"/(\d{12})-(\d{12})\.csv\.gz$")
FIRST_TIMESTAMP = re.compile(rb"^(\d{4})-(\d\d)-(\d\d) (\d\d):(\d\d):(\d\d)(?:\.(\d+))?")


def log(message):
    print(message, flush=True)


def s3_client():
    import boto3
    from botocore.config import Config

    config = Config(s3={"addressing_style": "path" if PATH_STYLE else "auto"}, retries={"max_attempts": 3})
    return boto3.client("s3", endpoint_url=ENDPOINT, region_name=REGION, config=config)


def generation_of(path):
    """Names the file's current life by its first line's timestamp."""
    with open(path, "rb") as handle:
        head = handle.read(256)
    if not head:
        return None
    match = FIRST_TIMESTAMP.match(head)
    if match:
        year, month, day, hour, minute, second, fraction = match.groups()
        return "%s%s%sT%s%s%s%s" % (year.decode(), month.decode(), day.decode(), hour.decode(), minute.decode(),
                                    second.decode(), (fraction or b"").decode())
    # A first line that is not a timestamp is still the same line next time.
    return "h" + hashlib.sha256(head).hexdigest()[:16]


def shipped_ranges(client, prefix):
    """Reads back which byte ranges of a series are in the bucket."""
    ranges = []
    token = None
    while True:
        args = {"Bucket": BUCKET, "Prefix": prefix}
        if token:
            args["ContinuationToken"] = token
        page = client.list_objects_v2(**args)
        for item in page.get("Contents", []):
            match = CHUNK_KEY.search(item["Key"])
            if match:
                ranges.append((int(match.group(1)), int(match.group(2))))
        if not page.get("IsTruncated"):
            break
        token = page.get("NextContinuationToken")
    return sorted(ranges)


def first_gap(ranges):
    """Returns the first offset not covered, and where the next chunk starts
    (None when nothing is shipped beyond it)."""
    covered = 0
    for start, end in ranges:
        if start > covered:
            return covered, start
        covered = max(covered, end)
    return covered, None


def upload(client, key, raw, name, start, end):
    packed = gzip.compress(raw, mtime=0)
    client.put_object(
        Bucket=BUCKET,
        Key=key,
        Body=packed,
        ContentMD5=base64.b64encode(hashlib.md5(packed).digest()).decode(),
        ContentType="application/gzip",
        Metadata={"sha256": hashlib.sha256(raw).hexdigest(), "source-file": name, "range": "%d-%d" % (start, end)},
    )
    stored = client.head_object(Bucket=BUCKET, Key=key)["ContentLength"]
    if stored != len(packed):
        # Left in place, the next run would take it for shipped.
        try:
            client.delete_object(Bucket=BUCKET, Key=key)
        except Exception as exc:
            log("could not remove %s: %s" % (key, exc))
        raise RuntimeError("%s is %d bytes in the bucket, expected %d" % (key, stored, len(packed)))


def ship_file(client, path):
    name = os.path.basename(path)
    generation = generation_of(path)
    if generation is None:
        return 0
    series = "/".join(part for part in (PREFIX, POD, name, generation) if part) + "/"
    ranges = shipped_ranges(client, series)
    sent = 0
    while sent < MAX_PER_RUN:
        offset, next_start = first_gap(ranges)
        size = os.path.getsize(path)
        limit = size if next_start is None else min(size, next_start)
        if offset >= limit:
            break
        with open(path, "rb") as handle:
            handle.seek(offset)
            data = handle.read(min(limit - offset, MAX_CHUNK))
        if next_start is None or offset + len(data) < next_start:
            # Whole lines only, unless the file is giving none: a line longer
            # than a chunk is shipped as it is rather than never.
            cut = data.rfind(b"\n") + 1
            if cut:
                data = data[:cut]
            elif len(data) < MAX_CHUNK:
                break
        end = offset + len(data)
        key = "%s%012d-%012d.csv.gz" % (series, offset, end)
        upload(client, key, data, name, offset, end)
        ranges = sorted(ranges + [(offset, end)])
        sent += len(data)
        log("shipped %s bytes %d-%d of %s" % (generation, offset, end, name))
    return sent


def main():
    if not BUCKET or not POD:
        log("AUDIT_BUCKET and POD_NAME must be set")
        return 2
    try:
        files = sorted(entry for entry in os.listdir(LOG_DIR) if entry.endswith(".csv"))
    except OSError as exc:
        log("cannot read %s: %s" % (LOG_DIR, exc))
        return 1
    client = s3_client()
    failed = False
    for name in files:
        try:
            ship_file(client, os.path.join(LOG_DIR, name))
        except Exception as exc:  # one failing file must not stop the others
            failed = True
            log("could not ship %s: %s" % (name, exc))
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())

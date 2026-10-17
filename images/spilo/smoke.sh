#!/usr/bin/env bash
# Checks that a freshly built Spilo image contains what the operator relies on.
#
#   smoke.sh IMAGE POSTGRES_MAJOR_VERSION
#
# It only inspects the image; actually running a cluster from it is the job of
# the end-to-end tests (E2E_POSTGRES_IMAGE, E2E_POSTGRES_VERSION).
set -euo pipefail

image=${1:?usage: smoke.sh IMAGE POSTGRES_MAJOR_VERSION}
major=${2:?usage: smoke.sh IMAGE POSTGRES_MAJOR_VERSION}

run() { docker run --rm --entrypoint "$@"; }

echo "postgres binary on PATH reports version ${major}"
version=$(run postgres "$image" --version)
echo "  ${version}"
[[ "$version" == "postgres (PostgreSQL) ${major}."* ]] || { echo "unexpected version: ${version}" >&2; exit 1; }

echo "patroni is installed"
run patroni "$image" --version

echo "wal-g is installed"
run wal-g "$image" --version

# The operator runs the pod as uid 101, gid 103 and gives the data volume to
# that group. That only works if the image's postgres user is the same one.
echo "the postgres user is uid 101, gid 103"
ids=$(run id "$image" postgres)
echo "  ${ids}"
[[ "$ids" == "uid=101(postgres) gid=103("* ]] || { echo "unexpected user: ${ids}" >&2; exit 1; }

echo "Spilo's launch script is present"
run test "$image" -x /launch.sh

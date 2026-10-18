# Database images

The operator runs PostgreSQL under [Patroni](https://github.com/patroni/patroni) using
[Spilo](https://github.com/zalando/spilo), Zalando's image that bundles the two with wal-g and
the scripts that start them. This directory pins which Spilo release is used and how the images
are built. Nothing in it is modified from upstream: the images are built from upstream's own
`postgres-appliance/Dockerfile` at a pinned commit.

## Why not use upstream's images

Upstream publishes images rarely and late. Its 4.0-p3 image for PostgreSQL 17 ships 17.5, many
minor releases behind the 17.x line (which fix security issues), and that release has no image
for 16 or 18 at all. Building from the same source gets the current PostgreSQL minor release
and operating system packages, on the architectures this project supports.

## What is built

| Image | PostgreSQL | Platforms |
|---|---|---|
| `ghcr.io/tusharshrivastava62/spilo-16` | 16 | linux/amd64, linux/arm64 |
| `ghcr.io/tusharshrivastava62/spilo-17` | 17 | linux/amd64, linux/arm64 |
| `ghcr.io/tusharshrivastava62/spilo-18` | 18 | linux/amd64, linux/arm64 |

Tags have the form `<upstream release>-r<revision>`, for example `4.1-p2-r1`, and are immutable:
the workflow refuses to publish over one. There is deliberately no `latest`; an image that can
change under a running database is not something to point a cluster at.

The older PostgreSQL majors that are also supported are installed alongside, as upstream does
for its own images, so Spilo's major-upgrade tooling has the binaries it expects. The operator
does not perform major-version upgrades.

## Updating

1. Change `SPILO_REF` and `SPILO_COMMIT` in [`spilo/upstream.env`](spilo/upstream.env) to the new
   upstream release and the commit its tag points to, and reset `IMAGE_REVISION` to `1`. To
   publish a rebuild of the same release instead (for operating system updates), only raise
   `IMAGE_REVISION`.
2. Open a pull request. The [workflow](../.github/workflows/spilo-image.yml) builds all six
   images (three versions, two architectures), checks them with [`spilo/smoke.sh`](spilo/smoke.sh)
   and scans them with Trivy, failing on critical vulnerabilities that have a fix.
3. After merging, run the workflow by hand with **publish** ticked. Only then are images pushed.
4. Update `defaultImages` in `internal/controller/images.go` to the new tag. Existing clusters are
   not affected: each keeps the image recorded in its `status.image` until `spec.image` is
   changed.
5. Check the list of libraries Spilo preloads. The operator writes it out in full, plus `pgaudit`,
   for clusters with `spec.compliance` (`spiloPreloadLibraries` in
   `internal/controller/compliance.go`), so a library Spilo adds or drops in the new release
   changes what a compliance cluster loads. The end-to-end test that turns compliance on asks the
   image for its own list and fails if the two differ.

The workflow also runs every Monday without publishing, so newly disclosed vulnerabilities show
up as a failed run instead of going unnoticed.

## Trying an image against a cluster

The end-to-end tests accept the image and version to use:

```bash
E2E_POSTGRES_VERSION=18 E2E_POSTGRES_IMAGE=<image> make test-e2e
```

## First publication

GitHub creates a new container package as private. After the first publish, set each of the
three packages to public under the repository owner's *Packages* settings, or pulling the
images will need credentials.

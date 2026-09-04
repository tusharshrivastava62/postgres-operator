# Releasing

A release is cut entirely by pushing a version tag - the tag itself is
the version, not `charts/postgres-operator/Chart.yaml`, which the release
workflow overrides at package time. There is nothing to bump before
tagging except the CRD, if it changed (see below).

1. Make sure `main` is green: check the [Tests, Lint, and E2E Tests
   workflows](https://github.com/tusharshrivastava62/postgres-operator/actions)
   on the commit you're about to tag.

2. If the CRD schema changed since the last release, regenerate the
   chart's copy of it - Helm's `crds/` directory is install-only and
   won't pick up a schema change from an upgrade:

   ```bash
   make manifests
   cp config/crd/bases/db.db.youroperator.io_postgresclusters.yaml \
     charts/postgres-operator/crds/postgrescluster-crd.yaml
   ```

   Then re-apply the two by-hand fixes described in
   `charts/postgres-operator/README.md` (static labels instead of
   templating) before committing.

3. Tag and push:

   ```bash
   git tag v0.2.0
   git push origin v0.2.0
   ```

That push triggers `.github/workflows/release.yml`, which:

- Builds the operator image for `linux/amd64` and `linux/arm64` and
  pushes it to `ghcr.io/tusharshrivastava62/postgres-operator` as both
  `:0.2.0` and `:latest`.
- Packages `charts/postgres-operator` with the chart/app version set to
  `0.2.0` (overriding whatever's in `Chart.yaml`) and pushes it as an OCI
  artifact to `ghcr.io/tusharshrivastava62/charts`.
- Creates a GitHub Release for the tag, with the packaged chart `.tgz`
  attached and release notes generated from merged PRs/commits since the
  last tag.

## Installing a released version

```bash
helm install postgres-operator \
  oci://ghcr.io/tusharshrivastava62/charts/postgres-operator \
  --version 0.2.0 \
  --namespace postgres-operator-system --create-namespace
```

(This defaults `controllerManager.manager.image.tag` to the chart's app
version, so it pulls the matching `ghcr.io/.../postgres-operator:0.2.0`
image automatically - no `--set image.tag=...` needed the way local/dev
installs require.)

## One-time repository setup

GHCR packages created by `GITHUB_TOKEN` default to private. After the
first release, set both the image and the chart package to Public in
the repository's **Packages** tab, or `helm install`/`docker pull` from
outside the repo's own Actions runs will get a 403/401.

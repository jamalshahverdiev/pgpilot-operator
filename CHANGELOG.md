# Changelog

## v1.0.2

Operator image `v1.0.2`, Helm chart `0.1.2`.

Security and installation fixes. Upgrading is recommended for every install.

### Security

- pgwatch's built-in web UI and REST API are now disabled (`--web-disable=all`)
  in every collector pod. They listened on `0.0.0.0:8080` without
  authentication, and the `/test-connect` endpoint could be used by any
  workload able to reach the pod to obtain the monitored database's
  credentials. Existing collector pods are rolled automatically when the
  operator is upgraded.

### Fixed

- Helm: `helm install` with default values no longer fails with
  `invalid ownership metadata` — the CRDs were shipped twice (in the chart's
  `crds/` directory and again through a template). They are now rendered only
  by the template, so `installCRDs=false` really skips them and `helm upgrade`
  keeps them current.
- Helm: the chart's `PgpilotMonitor` CRD was a stale copy without inline
  `username`/`password`, the credential validation rules and
  `status.configHash`. Inline-credential monitors were rejected on Helm
  installs and a `ConfigMapUpdated` event was emitted on every reconcile. The
  chart CRDs are now copied from the generated ones by `make manifests`.
- Helm: the default image tag now matches the published `vX.Y.Z` image tag
  (it pointed at `1.0.1`, kustomize at `v1.0.1`).
- A metric `interval` that is not a valid duration (`"60"`, `"1d"`, `""`) is
  now rejected by the API server. Such a value used to be stored and then
  stopped the operator from reading any PgpilotMonitor or
  PgpilotMetricLibrary, crash-looping it after a restart. Intervals must be at
  least `1s`.
- A Prometheus sink on port 8080 no longer collides with pgwatch's web server.

### Changed

- The CRDs installed by the chart carry `helm.sh/resource-policy: keep`:
  `helm uninstall` leaves them (and your monitors) in place.
- The documented `helm install` command pins the chart version.
- `make manifests` also refreshes `charts/pgpilot-operator/files/crds`; CI
  fails when generated files are stale.

### Upgrade notes

- Helm, existing releases. First check whether the release already owns the
  CRDs:

  ```bash
  kubectl get crd pgpilotmonitors.pgpilot.io \
    -o jsonpath='{.metadata.annotations.meta\.helm\.sh/release-name}'
  ```

  - It prints the release name (the release was installed with `--skip-crds`
    or `--take-ownership`): a plain `helm upgrade --set installCRDs=true` is
    enough. Do not set `installCRDs=false` on such a release before this
    upgrade has run — the CRDs only get `helm.sh/resource-policy: keep` from
    this version on, and without it Helm would delete them.
  - It prints nothing (the CRDs came from the chart's `crds/` directory, i.e.
    the release was installed with `installCRDs=false`): either let the
    release adopt them with
    `helm upgrade --set installCRDs=true --take-ownership` (Helm >= 3.17), or
    keep managing them yourself with `--set installCRDs=false` and
    `kubectl apply --server-side --force-conflicts -f charts/pgpilot-operator/files/crds/`.

  `helm upgrade` reuses the values of the previous release, so
  `installCRDs=true` has to be passed explicitly if it was `false` before;
  otherwise the stale CRDs stay in place without any error.
- Objects that already store an unparseable `interval` (`"60"`, `"1d"`) must
  have it corrected with `kubectl edit` or `kubectl patch`. Until then the API
  server rejects every other write to the object, including the removal of its
  finalizer, so deleting such a monitor only completes once the interval is
  fixed. Objects with a parseable interval below `1s` keep working; the next
  edit of their metrics has to raise it to at least `1s`.

## v1.0.1

Testbed enhancements — no operator code changes.

- Added third test database (`app_tertiary` on `:15434`) with analytics schema
  (tenants, events, reports), FK-missing joins, unused indexes and HOT-update
  churn to exercise a domain distinct from primary (users/orders) and secondary
  (payments/merchants)
- `hack/testdb/init/tertiary.sql`, `init/workload-tertiary.sh`,
  `docker-compose.yaml` service entries, and `setup.sh` heredocs for
  `PgpilotMetricLibrary/tertiary-metrics` + `PgpilotMonitor/tertiary-db` so a
  single `setup.sh` run brings up all three monitors
- Demonstrates zero-config extensibility: adding the third monitor required no
  changes to the operator, Grafana dashboard JSON, or Prometheus scrape config

## v1.0.0

Initial release of pgpilot-operator.

### Features

- `PgpilotMonitor` CRD (`pgpilot.io/v1`) — one CR per monitored PostgreSQL database, creates an isolated pgwatch v5 pod
- `PgpilotMetricLibrary` CRD — reusable sets of custom SQL metric definitions, same-namespace references
- Reconciliation: ConfigMap (sources.yaml + metrics.yaml), Deployment, Service, optional ServiceMonitor
- Metric merge: pgwatch built-in presets + libraries + inline custom metrics, later entries override by name
- Prometheus sink enabled by default; gRPC sink as pass-through option
- User-controlled `podMetadata` / `serviceMetadata` (labels + annotations) for vmagent / Prometheus annotation-based discovery
- Automatic ServiceMonitor generation when prometheus-operator CRD is detected
- Content-hash annotation (`pgpilot.io/config-hash`) for automatic rollouts on config change
- Status conditions: `Ready`, `ConfigGenerated`, `DatabaseReachable`
- Finalizer for clean deletion
- Event recorder: Normal/Warning events for key lifecycle transitions
- Hardened security: non-root, read-only root FS, drop ALL capabilities, least-privilege RBAC
- Helm chart with toggles for CRDs, leader election, RBAC, ServiceMonitor
- GitHub Actions CI: lint (Go + Helm), unit tests, envtest, e2e (kind)
- Release artifacts: Docker image (Docker Hub) + Helm OCI chart, published manually via `make docker-push` + `helm push`
- Documentation: quickstart, CRD reference, architecture, observability, troubleshooting

# Changelog

## Unreleased

### Validation

The API server now rejects objects the operator cannot run. These were accepted
before and failed later, usually without a clear message:

- `spec.sinks` without an enabled sink, or with the gRPC sink enabled but no
  `endpoint`. `sinks: {}` now really means "Prometheus on port 9187": the
  Prometheus sink is enabled whenever `sinks.prometheus` is left out. To run
  with gRPC only, set `sinks.prometheus.enabled: false`.
- `spec.metrics` selecting nothing.
- The same library listed twice, and two metrics with the same name in one
  list (the last one used to win silently).
- `sqls` keys that are not PostgreSQL major versions written as plain integers.
  `"*"`, `"14.2"` and `"v14"` were dropped silently, leaving the metric without
  SQL; use `"0"` for "every version". `"014"` and `"+14"` were read as 14 and
  now have to be written `"14"`. Keys above 999 (a `server_version_num` such as
  `170000`) never matched a server. Empty statements are rejected too.
- `masterOnly` and `standbyOnly` both set on one metric.
- A `host` that is not a host name or IP address (URL metacharacters, a port
  typed into the host) and a database name over 63 characters.
- `customTags` keys that cannot work: empty, starting with `__`, or `dbname`
  (pgwatch sets it to the monitor name). Other keys are still accepted; on the
  Prometheus sink a character outside `[a-zA-Z0-9_]` is exported as `_`.
- `podMetadata` / `serviceMetadata` labels and annotation keys Kubernetes
  itself would refuse on the Pod or Service. Keys under `pgpilot.io/` are
  reserved for the operator.

New limits, to keep validation cost bounded: 256 metrics per library and per
`custom` list, 32 `sqls` entries per metric, 32 libraries per monitor, 32
`customTags`, 32 labels and 64 annotations per metadata block. `preset` must be
lower-case letters, digits and `_` (at most 63 characters), Secret keys letters,
digits, `-`, `_` and `.`, and a gRPC `endpoint` must not contain whitespace.

Fixed alongside:

- An IPv6 `host` works, with or without brackets; it used to be glued to the
  port into a different address. Database names with `#`, `?`, `%` or spaces
  no longer cut the connection URL short. Connection strings for ordinary host
  and database names are unchanged, so no pod is rolled because of this.
- The operator adds and removes its finalizer without rewriting the spec. It
  used to send the whole object back, which made the API server store defaults
  for `spec.image` on every monitor and counted as a spec change.
- `status.ready` is always written, so `kubectl get pm` shows `false` instead
  of an empty READY column.
- `kubectl get pml` shows the number of metrics (`status.metricCount`); the
  column was always empty.
- `statementTimeoutSeconds` is deprecated. It was never applied: pgwatch has no
  per-metric timeout. The field is still accepted and ignored.

Upgrade notes:

- A monitor that omitted `sinks.prometheus` (`sinks: {}`, or only a `grpc`
  block) gets the Prometheus sink as soon as the CRD is updated: defaults also
  apply to stored objects when they are read, so its pod is rolled with
  `/metrics` on port 9187. Add `prometheus: {enabled: false}` to keep a
  gRPC-only monitor without it.
- A monitor that set both `masterOnly` and `standbyOnly` was collecting on
  primaries only; keep `masterOnly` to preserve that.
- Existing objects that break one of the new rules are not changed, but how
  much the API server tolerates them depends on the Kubernetes version:

  | Kubernetes | Editing other fields, labels | Deleting | Status written by the operator |
  |------------|------------------------------|----------|--------------------------------|
  | 1.33+      | works                        | works    | works                          |
  | 1.30–1.32  | works                        | works    | refused until the object is fixed: READY, conditions and `metricCount` stop updating and the operator logs `update status ... is invalid`. The pgwatch pod keeps running. |
  | 1.28, 1.29 | refused                      | hangs in `Terminating` | refused |

  In every case the way out is to correct the offending field, which is always
  accepted - also while the object is `Terminating`. A list that exceeds a new
  size limit has to be shortened before any entry of it can be edited.
- Before upgrading the CRDs on Kubernetes older than 1.33, look for objects
  that will be affected. The usual ones are the `"*"` key in `sqls` and a
  monitor without an enabled sink:

  ```bash
  kubectl get pgpilotmonitors,pgpilotmetriclibraries -A -o json | jq -r '
    .items[] | . as $o
    | (if $o.kind == "PgpilotMonitor" then ($o.spec.metrics.custom // []) else ($o.spec.metrics // []) end) as $metrics
    | select(
        ($metrics | any((.sqls // {}) | keys | any(test("^(0|[1-9][0-9]{0,2})$") | not)))
        or ($metrics | any(.masterOnly == true and .standbyOnly == true))
        or ($o.kind == "PgpilotMonitor"
            and $o.spec.sinks.prometheus.enabled == false
            and $o.spec.sinks.grpc.enabled != true)
      )
    | "\(.kind) \(.metadata.namespace)/\(.metadata.name)"'
  ```

### Changed

- Built with Go 1.27 (was 1.25, which no longer receives security fixes). The
  builder and base images are pinned by digest.
- Kubernetes libraries 0.37, controller-runtime 0.25, prometheus-operator API
  types 0.95; `golang.org/x/net`, `x/text`, gRPC and OpenTelemetry moved to
  versions without known vulnerabilities.
- `make deploy` and `make build-installer` no longer rewrite
  `config/manager/kustomization.yaml`; `make install`, `uninstall`, `deploy`
  and `undeploy` fail when kustomize fails; `make docker-buildx` fails when the
  build or push fails and builds for `linux/amd64` and `linux/arm64`.
- `make test-e2e` uses a kubeconfig of its own (`bin/e2e-kubeconfig`) and
  refuses to run against any other context, so the suite can no longer install
  or remove anything in the cluster your kubeconfig points at. The Kind cluster
  is deleted even when the suite fails.
- New `make helm-validate` (renders the chart with its toggles and validates
  the objects against the Kubernetes schemas) and `make govulncheck`.
- CI: actions pinned by commit, least-privilege token, timeouts, no duplicate
  runs for pull requests; the linter and all tools come from the versions
  pinned in the Makefile; `go.mod` must be tidy; weekly vulnerability scan.

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

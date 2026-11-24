# Changelog

## Unreleased

### pgwatch 6.2.0

Collectors now run pgwatch 6.2.0 instead of 5.1.0, and the metric definitions
the operator hands to them are those of the same release (78 metrics and 17
presets instead of 74 and 15).

- The default image tag is no longer part of the CRD. `spec.image.repository`
  and `spec.image.tag` have no schema default any more; the operator fills them
  in when it builds the pod and does not write them into the object. A monitor
  that leaves the tag unset follows the operator from now on, and one that sets
  it keeps it.
- Operator releases up to v1.0.2 wrote `image: {repository:
  cybertecpostgresql/pgwatch, tag: "5.1.0"}` into every monitor they
  reconciled, which would keep all existing monitors on 5.1.0. When it starts,
  the operator removes that tag again, once, and records an
  `ImageDefaultRestored` event on the monitor. It goes by the record the API
  server keeps of who wrote each field (`managedFields`) and removes the tag
  only where that record shows no writer but the old operator. See "Which
  monitors move to 6.2.0" below for what it leaves alone and how to keep 5.1.0
  on purpose. `--migrate-stored-image-tag=false`
  (`migrations.storedImageTag: false` in the chart) turns it off.
- `spec.metrics.preset` is checked against pgwatch's presets, also when it is
  used without custom metrics. A name that is not a preset, and pgwatch's
  presets for other kinds of sources (`patroni`, `pgbouncer`, `pgpool`), used
  to give a collector that ran, looked healthy and collected nothing. Such a
  monitor now gets `Ready=False` / `ConfigGenerated=False` with reason
  `InvalidPreset`, a message listing the usable presets, and a warning event.
- The operator's writes are recorded by the API server under the field manager
  `pgpilot-operator` instead of `manager`.
- The copy of pgwatch's metric definitions in the operator comes with pgwatch's
  licence (BSD 3-Clause), in the source tree and in the image (`/licenses`).
  `make update-pgwatch-registry PGWATCH_VERSION=x.y.z` fetches the definitions
  of another release; `make test-collector` runs the generated configuration
  against the real pgwatch image and a PostgreSQL container, and CI does so for
  PostgreSQL 14, 18 and 19.

Upgrade notes:

- **Every collector pod is replaced once**, with the new image, the new metric
  definitions, or both. The new image has to be pullable:
  `cybertecpostgresql/pgwatch:6.2.0`. Where it is not (air-gapped clusters,
  mirrors), the new pod stays in `ImagePullBackOff` and the old one keeps
  collecting until the image is available.
- **Upgrade the CRDs together with the operator.** The old CRD defaults
  `spec.image.tag` to 5.1.0 and puts it back as fast as it is removed. The
  operator notices (`the installed PgpilotMonitor CRD still defaults
  spec.image.tag` in its log), retries for a few minutes and then waits for
  its next start. The Helm chart and `dist/install.yaml` update both at once.
- **Which monitors move to 6.2.0**: those without `spec.image.tag`, and those
  whose tag 5.1.0 nobody but an earlier operator release wrote (or nobody at
  all: a default the old CRD filled in). The operator leaves a tag alone, and
  names the monitor in its log at start, when

  - another writer owns the field: a tag you set when creating the monitor, or
    set, changed or applied later with a different value, or with server-side
    apply (Flux, `kubectl apply --server-side`),
  - the manifest last applied with `kubectl apply` (which is also what Argo CD
    uses by default) contains a tag,
  - the monitor has its own `spec.image.repository`,
  - the object carries no ownership record (restored from a backup without
    `managedFields`).

  One case cannot be told apart: writing `tag: "5.1.0"` over the identical
  value the old operator had already stored - with `kubectl patch`, `kubectl
  edit`, `kubectl replace` or a Helm upgrade - changes nothing in the object,
  and the API server keeps no trace of it. Such a tag is taken for the old
  operator's and removed. **To keep 5.1.0 on a monitor on purpose, annotate it
  before upgrading:**

  ```bash
  kubectl annotate pgpilotmonitor <name> -n <namespace> pgpilot.io/keep-image-tag=true
  ```

  The same applies to monitors created by another controller whose binary is
  called `manager`. To see what is left on the old tag after the upgrade, and
  to move a monitor by hand:

  ```bash
  kubectl get pgpilotmonitors -A \
    -o custom-columns='NAMESPACE:.metadata.namespace,NAME:.metadata.name,REPOSITORY:.spec.image.repository,TAG:.spec.image.tag'
  kubectl patch pgpilotmonitor <name> -n <namespace> --type=json \
    -p '[{"op":"remove","path":"/spec/image/tag"}]'
  ```

  On Kubernetes 1.28 and 1.29 a monitor that also breaks one of the validation
  rules introduced in this release cannot be changed at all until its spec is
  corrected (see the table under "Validation"); that includes removing the
  tag, by the operator or by hand.

- **Monitors that stay on pgwatch 5.x** still receive the 6.2.0 metric
  definitions when they use a preset; the operator has one copy. pgwatch 5.1.0
  runs with them, but collects the 6.2.0 preset contents (see the next point)
  and logs `no SQL found for metric version` once per interval for
  `checkpointer` on PostgreSQL 14-16 and for the PostgreSQL 19 metrics. Staying
  on 5.x is meant as a short-term measure.
- **Series that change name or meaning.** Check dashboards and alert rules:

  | Before | With pgwatch 6.2.0 |
  |--------|--------------------|
  | `pgwatch_settings_<setting>` (presets `exhaustive`, `exhaustive_no_python`, `full`, `rds`, `aurora`, `azure`, `gce`, `aiven`, `debug`) | `pgwatch_settings_num_value{name="<setting>"}`, for every numeric setting. `pgwatch_settings_max_connections` becomes `pgwatch_settings_num_value{name="max_connections"}` |
  | `pgwatch_recommendations_duplicate_count` | `pgwatch_recommendations_affected_count`, now also present (value 1) for unused and invalid indexes |
  | `pgwatch_backends_longest_*_seconds` in whole seconds, rounded up | fractional seconds |
  | `pgwatch_backends_total`, `_active`, `_idle` include the collector's other sessions | the collector holds one session, so the counts are 2-3 lower per monitor; the `longest_*`, `avg_*` and `max_xmin_age_tx` series are absent while no other session exists in the database |
  | `NaN` and `Inf` values of float columns are exported | the sample is left out |
  | label and metric names with characters outside `[a-zA-Z0-9_]` (custom tag keys, `tag_` columns, column names of custom metrics) are exported verbatim to Prometheus 3.4 and newer | exported with `_`, as they always were to Prometheus 2.x. Prometheus 3.0-3.3 gets the verbatim names from both versions |

  The dashboard in `examples/grafana` uses none of the renamed series.
- **Slow metric queries are cancelled.** pgwatch 6 fetches all metrics of a
  monitor with one worker, those due at the same moment in one batch. The
  batch has 30 seconds (or the greatest common divisor of the monitor's
  intervals, if that is longer); what has not finished is cancelled and tried
  again alone, with the metric's own interval (at least 30 seconds) as limit.
  pgwatch 5 let a query run and delivered late. A custom metric slower than
  the second limit stops producing data, and every slow query delays the other
  metrics of the same monitor while it runs. See "Query time" in
  docs/crd-reference.md. `statementTimeoutSeconds` is still ignored.
- **Failing metrics are logged every time**, not once in ten minutes: a
  monitor on preset `standard`, `exhaustive` or `full` without pgwatch's
  PL/Python helpers, or any preset with `stat_statements` without the
  `pg_stat_statements` extension, logs an error per interval for those metrics.
  `exhaustive_no_python` avoids the first.
- **TLS connections** (`sslmode` other than `disable`) use SCRAM channel
  binding when the server offers it; nothing to configure.
- PostgreSQL 14 remains the oldest version with built-in metrics; PostgreSQL 19
  is supported, with three metrics of its own (`autovacuum_scores`, `recovery`,
  `stat_lock`). Two metrics of preset `debug` (`subscription_stats`,
  `table_stats_approx`) fail on PostgreSQL 19 with pgwatch 6.2.0's own SQL.

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

### Credentials

- The operator no longer lists or watches Secrets. Its role grants `get` on
  Secrets only, and Secrets are never cached: it used to keep every Secret of
  the cluster, with its data, in memory. ConfigMaps, Deployments, Services,
  Pods and ServiceMonitors are cached only when they carry the operator's
  `managed-by` label, so the operator's memory no longer grows with the size of
  the cluster.
- A rotated password reaches the collector. The Secret a monitor references is
  read again every five minutes (`--credentials-recheck-interval`, Helm value
  `credentials.recheckInterval`); when it has changed, the pod is replaced.
  Before, pgwatch kept the old password until someone restarted it, while the
  monitor reported `Ready`. Annotating the monitor makes the operator look at
  once.
- A Secret without the username or password key is reported
  (`SecretKeyMissing`, naming the key) instead of leaving the pod in
  `CreateContainerConfigError` behind a green condition. A Secret that cannot
  be read is reported as `SecretReadFailed` and retried with backoff rather
  than being called "not found". `credentialsSecret.name` has to be a valid
  object name.
- While credentials are unusable, `status.ready` is `false` together with the
  `Ready` condition; the READY column used to keep showing `true`.
- The `DatabaseReachable` condition is now `CredentialsAvailable`. It never
  checked that the database could be reached, only that credentials were
  configured; the old condition is removed from existing objects. Anything
  that reads `DatabaseReachable` (alerts, scripts) has to be changed.
- A monitor is no longer reconciled again by its own status updates.

Upgrade notes:

- Upgrade (and roll back) the operator image and its ClusterRole together, as
  `helm upgrade` / `helm rollback` do. An operator older than this release
  under the new role cannot list Secrets any more: it logs `secrets is
  forbidden` and stops reconciling every monitor while its pod stays Ready.
  Do not pin `image.tag` to an older version with the new chart.
- Every collector that takes its credentials from a Secret is restarted once
  when the operator is upgraded (its pod template gains the
  `pgpilot.io/credentials-version` annotation). Monitors with inline
  credentials are not restarted.
- From then on any change to a referenced Secret object restarts the
  collector, also a change to its labels or annotations only.

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

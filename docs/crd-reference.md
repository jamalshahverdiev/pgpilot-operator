# CRD Reference

## PgpilotMonitor

**API Version:** `pgpilot.io/v1`
**Short Name:** `pm`
**Scope:** Namespaced

One PgpilotMonitor creates one pgwatch pod that monitors a single PostgreSQL database.

Keep `metadata.name` to at most 55 characters and without dots: the generated
Deployment, Service and ConfigMap are named `pgpilot-<name>`, and a Service
name has to be a DNS label of at most 63 characters. A longer or dotted name
is accepted by the API server, but the Service cannot be created for it.

### spec.database

Credentials can be provided in two ways — exactly one is required:
**inline** (`username` + `password`, dev/test only, stored in etcd in clear text)
or **Secret-based** (`credentialsSecret`, recommended for production).

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `host` | string | yes | | PostgreSQL host: DNS name, IPv4 or IPv6 address (brackets optional). The port goes into `port`, not here; max 253 chars |
| `port` | int32 | no | `5432` | PostgreSQL port (1-65535) |
| `database` | string | yes | | Logical database name (max 63 chars) |
| `sslmode` | enum | no | `disable` | `disable`, `allow`, `prefer`, `require`, `verify-ca`, `verify-full` |
| `username` | string | no* | | Inline DB username (dev/test only) |
| `password` | string | no* | | Inline DB password (dev/test only) |
| `credentialsSecret` | object | no* | | Reference to a Secret (see below) |
| `customTags` | map[string]string | no | | Additional labels added to every measurement. Keys must not be empty, start with `__` or be `dbname` (pgwatch sets it to the monitor name). Prefer plain label names: on the Prometheus sink any character outside `[a-zA-Z0-9_]` is exported as `_`. Max 32 entries |

*Exactly one of (`username`+`password`) or `credentialsSecret` must be set.

### spec.database.credentialsSecret

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `name` | string | yes | | Secret name in the same namespace (a valid object name: lower-case letters, digits, `-`, `.`; max 253 chars) |
| `usernameKey` | string | no | `username` | Key inside the Secret for the database username (letters, digits, `-`, `_`, `.`) |
| `passwordKey` | string | no | `password` | Key inside the Secret for the database password (letters, digits, `-`, `_`, `.`). The key must exist; its value may be empty |

### spec.metrics

At least one of `preset`, `fromLibraries` or `custom` must be set.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `preset` | string | no* | | pgwatch built-in preset, see [presets](#presets). Starts with a lower-case letter, then lower-case letters, digits and `_`; max 63 chars |
| `fromLibraries` | []LibraryRef | no* | | References to PgpilotMetricLibrary CRs in the same namespace (`name`, max 253 chars). Each library at most once; max 32 |
| `custom` | []MetricDefinition | no* | | Inline custom metrics (prefer libraries for shared metrics). Names must be unique; max 256 |

### spec.sinks

At least one sink must be enabled; pgwatch does not start without one. When
`prometheus` is left out it is enabled with the defaults below, so `sinks: {}`
gives a Prometheus endpoint on port 9187. To use the gRPC sink alone, set
`prometheus.enabled: false` explicitly.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `prometheus.enabled` | bool | no | `true` | Enable Prometheus /metrics endpoint. With `false` the monitor has no Service and no ServiceMonitor |
| `prometheus.port` | int32 | no | `9187` | Port for /metrics (1-65535) |
| `grpc.enabled` | bool | no | `false` | Enable gRPC sink |
| `grpc.endpoint` | string | when enabled | | gRPC receiver host:port, no whitespace, max 512 chars |
| `grpc.tls.caSecretRef` | string | no | | Secret name with CA cert (key: `ca.crt`) |

#### Presets

The presets of pgwatch 6.2.0 that a monitor can use: `minimal`, `basic`,
`standard`, `exhaustive`, `exhaustive_no_python`, `full`, `debug`,
`recommendations`, and for managed services `rds`, `aurora`, `azure`, `gce` and
`aiven`. [observability.md](observability.md#choosing-a-preset) describes them.

A name that is not a preset is not rejected when the object is created (the
list belongs to the pgwatch release, not to the API). The operator reports it
instead: the monitor gets `Ready=False` and `ConfigGenerated=False` with reason
`InvalidPreset`, a message that lists the usable names, and a warning event;
no collector is created for it, and a collector that already runs keeps its
last valid configuration. The same happens for the presets pgwatch ships for
other kinds of sources, which collect nothing from a PostgreSQL database:
`patroni` (for its Prometheus-scraping sources), `pgbouncer` and `pgpool`. A
name with other characters than the pattern above allows, such as
`postgres-exporter-basic`, is refused by the API server itself.

### spec.resources

Standard Kubernetes `ResourceRequirements` (`requests`, `limits`).

### spec.image

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `repository` | string | no | `cybertecpostgresql/pgwatch` | pgwatch image repository |
| `tag` | string | no | `6.2.0` | Image tag. Leave unset to run the pgwatch release the operator is built for |
| `pullPolicy` | enum | no | `IfNotPresent` | `Always`, `IfNotPresent`, `Never` |

The defaults of `repository` and `tag` are applied by the operator when it
builds the collector pod; they are not written into the object. A monitor that
leaves them unset therefore moves to the newer pgwatch release when the
operator is upgraded, and one that sets them stays where it is. (Monitors
created under operator v1.0.2 or older had `tag: "5.1.0"` written into them;
see [troubleshooting](troubleshooting.md#collector-still-runs-an-old-pgwatch-after-an-operator-upgrade).) Setting a tag
is only needed to hold a monitor back: the metric definitions the operator
hands to the collector are those of its default release (see
[architecture.md](architecture.md#metricsyaml)).

### spec.podMetadata / spec.serviceMetadata

| Field | Type | Description |
|-------|------|-------------|
| `labels` | map[string]string | Merged onto generated Pod or Service. Must be valid Kubernetes label keys and values; max 32. |
| `annotations` | map[string]string | Merged onto generated Pod or Service. Keys must be valid Kubernetes annotation keys; max 64. |

The labels and annotations the operator sets itself (`app.kubernetes.io/name`,
`app.kubernetes.io/instance`, `app.kubernetes.io/managed-by`,
`app.kubernetes.io/component`, `pgpilot.io/monitor`, `pgpilot.io/config-hash`,
`pgpilot.io/credentials-version`)
always win over an entry with the same key. Keys under `pgpilot.io/` are
reserved for the operator and are rejected. Other `app.kubernetes.io/*` keys,
such as `app.kubernetes.io/part-of`, are yours to set.

### status

| Field | Type | Description |
|-------|------|-------------|
| `ready` | bool | True when the pgwatch pod of the current spec is running and available, i.e. the rollout has completed. False while a changed spec is rolled out or cannot be, even though the pod of the previous spec keeps collecting. Always says what the `Ready` condition says, and is always present |
| `podName` | string | Name of a pgwatch pod that runs now: a ready one if there is one, otherwise the newest. Empty when none runs |
| `observedGeneration` | int64 | The `.metadata.generation` the status describes. Written on every reconcile, also when the spec could not be applied; the `Ready` condition tells which |
| `lastReconciled` | timestamp | When the operator last changed the status. Not a heartbeat: a reconcile that finds nothing new writes nothing |
| `configHash` | string | SHA-256 of the rendered `sources.yaml` + `metrics.yaml`. Changes whenever the underlying pgwatch Deployment is (or is about to be) rolled — useful for external tools tracking rollouts. |
| `conditions` | []Condition | `Ready` (its reason says what is in the way, see [troubleshooting](troubleshooting.md#monitor-stuck-in-readyfalse)), `ConfigGenerated` (`Unknown` when the reconcile did not get as far as rendering the configuration), `CredentialsAvailable` (credentials are configured and, for a Secret, both keys have a value; not a connectivity check), `ServiceMonitorApplied` (only on monitors that are to have a ServiceMonitor: the operator found the prometheus-operator API when it started and the Prometheus sink is on) |

A monitor's name has to fit the objects generated from it: at most 55
characters, lower-case letters, digits and `-`, ending with a letter or digit.
The Service is called `pgpilot-<name>`, and a Service name is a DNS label. A
monitor with another name is accepted by the API (the rule cannot be enforced
there without making existing objects undeletable) and reported with
`Ready=False`, reason `InvalidName`; nothing is generated for it.

### Printer columns

```
$ kubectl get pm
NAME    READY   AGE
my-db   true    5m

$ kubectl get pm -o wide
NAME    READY   POD                HOST              DB       AGE
my-db   true    pgpilot-my-db-...  pg.example.com   myapp    5m
```

---

## PgpilotMetricLibrary

**API Version:** `pgpilot.io/v1`
**Short Name:** `pml`
**Scope:** Namespaced

A reusable collection of pgwatch-compatible SQL metric definitions.

### spec.metrics[]

Between 1 and 256 entries; metric names must be unique within the library.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `name` | string | yes | | Metric name. Pattern: `^[a-z_][a-z0-9_]*$`, max 63 chars |
| `description` | string | no | | Human-readable hint |
| `interval` | duration | no | `60s` | Collection period as a Go duration (e.g. `30s`, `5m`); at least `1s`, used in whole seconds |
| `sqls` | map[string]string | yes | | Minimum PostgreSQL major version -> SQL (see below). 1 to 32 entries; statements must not be empty |
| `gauges` | []string | no | | Columns treated as gauges. `["*"]` = all columns |
| `statementTimeoutSeconds` | int32 | no | `0` | **Deprecated, ignored.** pgwatch has no configurable per-metric timeout (see [query time](#query-time)); set `statement_timeout` on the monitoring role instead |
| `masterOnly` | bool | no | `false` | Collect only on primary instances. Cannot be combined with `standbyOnly` |
| `standbyOnly` | bool | no | `false` | Collect only on standby instances. Cannot be combined with `masterOnly` |
| `isInstanceLevel` | bool | no | `false` | Instance-wide metric (not database-scoped) |

#### sqls keys

A key is the lowest PostgreSQL major version a statement is meant for, written
as a plain integer in quotes: `"14"`, `"17"`. For a given server pgwatch runs
the statement with the highest key that is not above the server's version, so
`"14"` also covers 15, 16 and so on until a higher key is defined. `"0"` covers
every version. A server older than the lowest key gets no data for the metric.

Keys are major versions (`"17"`), not `server_version_num` values (`170000`),
and at most three digits. There is no wildcard key: `"*"`, `"14.2"`, `"v14"` and
`"014"` are rejected.

```yaml
sqls:
  "0":  SELECT ...   # every version below 17
  "17": SELECT ...   # 17 and newer
```

#### Result columns

A column becomes a series `pgwatch_<metric>_<column>` when its type is
`integer`, `bigint`, `real`, `double precision` or `boolean` (exported as 0 or
1). Columns named `tag_<x>` become the label `<x>` instead. Every other type is
dropped without a log line - text, timestamps, and also `numeric`, `smallint`
and `oid`, which is what `avg()`, `round(x, 2)` and `sum()` of a `bigint`
return: cast such results, e.g. `round(avg(x), 2)::float8 AS avg_x`.

Return the measurement time as `(extract(epoch from now()) * 1e9)::int8 AS
epoch_ns`, as pgwatch's own metrics do; without the column pgwatch stamps the
row with its own clock when it fetches it. Either way the Prometheus sink
serves a metric for ten minutes after that timestamp and then drops it until
the next collection, so a metric with an `interval` above ten minutes has gaps
in Prometheus; query it with `last_over_time(...)` over a range longer than
the interval.

#### Query time

pgwatch 6 collects all metrics of a monitor in one loop over a single
connection: the metrics that are due at the same moment are sent to the server
together. Two consequences for custom metrics:

- The metrics of one batch share one time limit: 30 seconds, or the greatest
  common divisor of all the monitor's intervals if that is longer (30 or 60
  seconds with most presets). What has not finished by then is cancelled, on
  the server as well, and tried again alone; the limit is then the metric's
  own `interval`, at least 30 seconds. A query slower than the first limit
  therefore runs twice at every collection. One slower than the second never
  produces data; with pgwatch 5 it did, late.
- While a slow query runs, the other metrics of the same monitor wait,
  `pgwatch_instance_up` included. A long `interval` does not isolate a slow
  metric, it only makes the wait rarer.

Keep metric queries fast, give the monitoring role a `statement_timeout` (and a
`lock_timeout` if the queries can wait on locks), and put a query that is
heavy by nature into a PgpilotMonitor of its own: there it is the only metric
of its batch, the single limit is its own interval, and it can only delay
itself.

### status

| Field | Type | Description |
|-------|------|-------------|
| `observedGeneration` | int64 | Last reconciled `.metadata.generation` |
| `metricCount` | int32 | Number of metric definitions in the library |
| `conditions` | []Condition | `Valid` — `True` when every definition can be used. `False` with reason `InvalidMetric` when a definition stored before a validation rule existed breaks it (the message names the metric and what is wrong; monitors that use the library report `InvalidMetric` too), or with reason `Empty`. The SQL itself is not checked. On Kubernetes before 1.33 the status of a library that breaks a rule cannot be written, and keeps what an earlier release left there (`True`) |

### Printer columns

```
$ kubectl get pml
NAME               METRICS   AGE
business-metrics   2         5m
```

---

## MetricDefinition (shared type)

Used in both `PgpilotMonitor.spec.metrics.custom[]` and `PgpilotMetricLibrary.spec.metrics[]`. Fields are identical to the PgpilotMetricLibrary `spec.metrics[]` table above.

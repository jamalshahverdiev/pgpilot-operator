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
| `preset` | string | no* | | pgwatch built-in preset (`basic`, `standard`, `exhaustive`, `rds`, `aurora`, etc.). Starts with a lower-case letter, then lower-case letters, digits and `_`; max 63 chars |
| `fromLibraries` | []LibraryRef | no* | | References to PgpilotMetricLibrary CRs in the same namespace (`name`, max 253 chars). Each library at most once; max 32 |
| `custom` | []MetricDefinition | no* | | Inline custom metrics (prefer libraries for shared metrics). Names must be unique; max 256 |

### spec.sinks

At least one sink must be enabled; pgwatch does not start without one. When
`prometheus` is left out it is enabled with the defaults below, so `sinks: {}`
gives a Prometheus endpoint on port 9187. To use the gRPC sink alone, set
`prometheus.enabled: false` explicitly.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `prometheus.enabled` | bool | no | `true` | Enable Prometheus /metrics endpoint |
| `prometheus.port` | int32 | no | `9187` | Port for /metrics (1-65535) |
| `grpc.enabled` | bool | no | `false` | Enable gRPC sink |
| `grpc.endpoint` | string | when enabled | | gRPC receiver host:port, no whitespace, max 512 chars |
| `grpc.tls.caSecretRef` | string | no | | Secret name with CA cert (key: `ca.crt`) |

### spec.resources

Standard Kubernetes `ResourceRequirements` (`requests`, `limits`).

### spec.image

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `repository` | string | no | `cybertecpostgresql/pgwatch` | pgwatch image repository |
| `tag` | string | no | `5.1.0` | Image tag |
| `pullPolicy` | enum | no | `IfNotPresent` | `Always`, `IfNotPresent`, `Never` |

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
| `ready` | bool | True when the pgwatch pod is running and ready. Always present, so `false` is shown rather than an empty column |
| `podName` | string | Name of the current pgwatch pod |
| `observedGeneration` | int64 | Last reconciled `.metadata.generation` |
| `lastReconciled` | timestamp | Time of last successful reconcile |
| `configHash` | string | SHA-256 of the rendered `sources.yaml` + `metrics.yaml`. Changes whenever the underlying pgwatch Deployment is (or is about to be) rolled — useful for external tools tracking rollouts. |
| `conditions` | []Condition | `Ready`, `ConfigGenerated`, `CredentialsAvailable` (credentials are configured and, for a Secret, both keys have a value; not a connectivity check) |

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
| `statementTimeoutSeconds` | int32 | no | `0` | **Deprecated, ignored.** pgwatch has no per-metric timeout; set `statement_timeout` on the monitoring role instead |
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

### status

| Field | Type | Description |
|-------|------|-------------|
| `observedGeneration` | int64 | Last reconciled `.metadata.generation` |
| `metricCount` | int32 | Number of metric definitions in the library |
| `conditions` | []Condition | `Valid` — True when library contains metrics |

### Printer columns

```
$ kubectl get pml
NAME               METRICS   AGE
business-metrics   2         5m
```

---

## MetricDefinition (shared type)

Used in both `PgpilotMonitor.spec.metrics.custom[]` and `PgpilotMetricLibrary.spec.metrics[]`. Fields are identical to the PgpilotMetricLibrary `spec.metrics[]` table above.

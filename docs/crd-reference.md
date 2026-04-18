# CRD Reference

## PgpilotMonitor

**API Version:** `pgpilot.io/v1`
**Short Name:** `pm`
**Scope:** Namespaced

One PgpilotMonitor creates one pgwatch pod that monitors a single PostgreSQL database.

### spec.database

Credentials can be provided in two ways — exactly one is required:
**inline** (`username` + `password`, dev/test only, stored in etcd in clear text)
or **Secret-based** (`credentialsSecret`, recommended for production).

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `host` | string | yes | | PostgreSQL host (DNS or IP) |
| `port` | int32 | no | `5432` | PostgreSQL port (1-65535) |
| `database` | string | yes | | Logical database name |
| `sslmode` | enum | no | `disable` | `disable`, `allow`, `prefer`, `require`, `verify-ca`, `verify-full` |
| `username` | string | no* | | Inline DB username (dev/test only) |
| `password` | string | no* | | Inline DB password (dev/test only) |
| `credentialsSecret` | object | no* | | Reference to a Secret (see below) |
| `customTags` | map[string]string | no | | Additional labels added to every measurement |

*Exactly one of (`username`+`password`) or `credentialsSecret` must be set.

### spec.database.credentialsSecret

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `name` | string | yes | | Secret name in the same namespace |
| `usernameKey` | string | no | `username` | Key inside the Secret for the database username |
| `passwordKey` | string | no | `password` | Key inside the Secret for the database password |

### spec.metrics

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `preset` | string | no | | pgwatch built-in preset (`basic`, `standard`, `exhaustive`, `rds`, `aurora`, etc.) |
| `fromLibraries` | []LibraryRef | no | | References to PgpilotMetricLibrary CRs in the same namespace |
| `custom` | []MetricDefinition | no | | Inline custom metrics (prefer libraries for shared metrics) |

### spec.sinks

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `prometheus.enabled` | bool | no | `true` | Enable Prometheus /metrics endpoint |
| `prometheus.port` | int32 | no | `9187` | Port for /metrics (1-65535) |
| `grpc.enabled` | bool | no | `false` | Enable gRPC sink |
| `grpc.endpoint` | string | no | | gRPC receiver host:port |
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
| `labels` | map[string]string | Merged onto generated Pod or Service. System keys (`app.kubernetes.io/*`, `pgpilot.io/*`) cannot be overridden. |
| `annotations` | map[string]string | Merged onto generated Pod or Service. System annotations always win. |

### status

| Field | Type | Description |
|-------|------|-------------|
| `ready` | bool | True when the pgwatch pod is running and ready |
| `podName` | string | Name of the current pgwatch pod |
| `observedGeneration` | int64 | Last reconciled `.metadata.generation` |
| `lastReconciled` | timestamp | Time of last successful reconcile |
| `configHash` | string | SHA-256 of the rendered `sources.yaml` + `metrics.yaml`. Changes whenever the underlying pgwatch Deployment is (or is about to be) rolled — useful for external tools tracking rollouts. |
| `conditions` | []Condition | `Ready`, `ConfigGenerated`, `DatabaseReachable` |

### Printer columns

```
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

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `name` | string | yes | | Metric name. Pattern: `^[a-z_][a-z0-9_]*$`, max 63 chars |
| `description` | string | no | | Human-readable hint |
| `interval` | duration | no | `60s` | Collection period (e.g. `30s`, `5m`) |
| `sqls` | map[string]string | yes | | PG major version -> SQL. At least one entry required. |
| `gauges` | []string | no | | Columns treated as gauges. `["*"]` = all columns |
| `statementTimeoutSeconds` | int32 | no | `0` | Per-query timeout (0 = no timeout) |
| `masterOnly` | bool | no | `false` | Collect only on primary instances |
| `standbyOnly` | bool | no | `false` | Collect only on standby instances |
| `isInstanceLevel` | bool | no | `false` | Instance-wide metric (not database-scoped) |

### status

| Field | Type | Description |
|-------|------|-------------|
| `observedGeneration` | int64 | Last reconciled `.metadata.generation` |
| `conditions` | []Condition | `Valid` — True when library contains metrics |

---

## MetricDefinition (shared type)

Used in both `PgpilotMonitor.spec.metrics.custom[]` and `PgpilotMetricLibrary.spec.metrics[]`. Fields are identical to the PgpilotMetricLibrary `spec.metrics[]` table above.

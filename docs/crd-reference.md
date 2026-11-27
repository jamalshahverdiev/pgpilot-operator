# CRD Reference

## PgpilotMonitor

**API Version:** `pgpilot.io/v1`
**Short Name:** `pm`
**Scope:** Namespaced

One PgpilotMonitor creates one pgwatch pod that monitors a single PostgreSQL database.

Keep `metadata.name` to at most 55 characters and without dots: the generated
Deployment and Service are named `pgpilot-<name>`, and a Service
name has to be a DNS label of at most 63 characters. A longer or dotted name
is accepted by the API server, but the Service cannot be created for it.

### spec.database

Credentials can be provided in two ways — exactly one is required:
**inline** (`username` + `password`, dev/test only: they are part of the
PgpilotMonitor, readable by everyone who may read it and stored in etcd as
it is) or **Secret-based** (`credentialsSecret`, recommended for production).
Inline credentials reach the collector through a Secret the operator generates
for them, `pgpilot-<monitor>-credentials`; they are not in its pod template.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `host` | string | yes | | PostgreSQL host: DNS name, IPv4 or IPv6 address (brackets optional). The port goes into `port`, not here; max 253 chars |
| `port` | int32 | no | `5432` | PostgreSQL port (1-65535) |
| `database` | string | yes | | Logical database name (max 63 chars) |
| `sslmode` | enum | no | `disable` | `disable`, `allow`, `prefer`, `require`, `verify-ca`, `verify-full`. Whether the connection uses TLS, and how the server is checked: not at all up to `require`; against a certificate authority with `verify-ca`; against it and the host name with `verify-full`. The authority is the one of `tls.caSecretRef`, without it the public ones of the pgwatch image |
| `username` | string | no* | | Inline DB username (dev/test only) |
| `password` | string | no* | | Inline DB password (dev/test only) |
| `credentialsSecret` | object | no* | | Reference to a Secret (see below) |
| `tls` | object | no | | Certificates for the connection (see below) |
| `customTags` | map[string]string | no | | Additional labels added to every measurement. Keys must not be empty, start with `__` or be `dbname` (pgwatch sets it to the monitor name). The operator adds `namespace`, the namespace of the monitor; a `namespace` key set here is replaced by it. Prefer plain label names: on the Prometheus sink any character outside `[a-zA-Z0-9_]` is exported as `_`. Max 32 entries |

*Exactly one of (`username`+`password`) or `credentialsSecret` must be set.

### spec.database.credentialsSecret

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `name` | string | yes | | Secret name in the same namespace (a valid object name: lower-case letters, digits, `-`, `.`; max 253 chars) |
| `usernameKey` | string | no | `username` | Key inside the Secret for the database username (letters, digits, `-`, `_`, `.`) |
| `passwordKey` | string | no | `password` | Key inside the Secret for the database password (letters, digits, `-`, `_`, `.`). The key must exist; its value may be empty |

### spec.database.tls

The Secrets, in the monitor's namespace, that hold the certificates for the
connection to the database. The collector pod mounts them read-only. The
operator checks that each exists, has its keys and holds what pgwatch can
load, reports what is wrong as `SecretNotFound`, `SecretKeyMissing` or
`InvalidCertificate`, and replaces the pod when one of them changes: see
[Renewing a certificate](troubleshooting.md#renewing-a-certificate).

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `caSecretRef.name` | string | yes | | Secret with the certificate authority the server's certificate has to be signed by: one or more PEM certificates. Needs `sslmode` `require`, `verify-ca` or `verify-full`; with `require` it makes the collector check the server as `verify-ca` does. The API server refuses it with any other `sslmode`, which would leave the server unchecked whatever is named here |
| `caSecretRef.key` | string | no | `ca.crt` | Key inside that Secret (letters, digits, `-`, `_`, `.`) |
| `clientCertSecretRef.name` | string | yes | | Secret with a client certificate and its private key, for a server that asks for one (`pg_hba.conf` method `cert`, or `clientcert=verify-full`). A Secret of type `kubernetes.io/tls`, as cert-manager writes it, has the default keys. Refused with `sslmode: disable`: a connection without TLS presents no certificate |
| `clientCertSecretRef.certKey` | string | no | `tls.crt` | Key of the certificate, PEM, with its chain if it has one |
| `clientCertSecretRef.keyKey` | string | no | `tls.key` | Key of the private key: PEM, without a passphrase, and the first PEM block of the value. May be the same key as `certKey`, for a Secret that holds both in one value: the private key first, then the certificate |

`caSecretRef` and `clientCertSecretRef` are each optional, and `name` is
required in the one that is set. None of them can name
`pgpilot-<monitor>-credentials`, the Secret the operator generates for
inline credentials (`GeneratedSecretReferenced`).

What the server makes of a client certificate is its own configuration:

- with the `pg_hba.conf` method `cert` the certificate is the login. Its
  common name has to be the database user, or be mapped to it by the server
  (`map=`), and no password is asked for. The monitor still needs a
  `username`, that user, and a `password` key, whose value is not used;
- with `clientcert=verify-full` on another method (`scram-sha-256`, ...) the
  certificate is asked for in addition: the common name has to match, and
  the password has to be the right one as well.

```yaml
spec:
  database:
    host: users-db.team-users.svc.cluster.local
    database: users
    sslmode: verify-full
    credentialsSecret:
      name: users-db-monitor-creds
    tls:
      caSecretRef:
        name: users-db-ca
      clientCertSecretRef:
        name: users-db-monitor-cert
```

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
| `prometheus.enabled` | bool | no | `true` | Enable Prometheus /metrics endpoint. With `false` the monitor has no Service and no ServiceMonitor, and its pod has no port and no probes: nothing listens in it. Such a pod counts as available once it has run for a minute, which is longer than pgwatch waits for a receiver that does not answer |
| `prometheus.port` | int32 | no | `9187` | Port for /metrics (1-65535) |
| `grpc.enabled` | bool | no | `false` | Enable gRPC sink |
| `grpc.endpoint` | string | when enabled | | gRPC receiver host:port, no whitespace, max 512 chars |
| `grpc.tls.caSecretRef` | string | no | | Name of a Secret in the monitor's namespace with, under the key `ca.crt`, the certificate authority the receiver's certificate has to be signed by. Naming it is what turns TLS on: the collector then refuses a receiver that authority did not sign for, or whose certificate is not for the host in `endpoint`. Without it the connection is plaintext, and a receiver that only speaks TLS makes the collector exit at once. The Secret is checked and followed like those of `spec.database.tls`. The API server takes any value here, because the field was stored before it had a meaning; one that is not the name of a Secret is reported as `SecretNotFound`. An `sslrootca` parameter in `endpoint`, the way to name an authority before this field worked, gives way to the Secret |

#### Presets

The presets of pgwatch 6.2.0 that a monitor can use: `minimal`, `basic`,
`standard`, `exhaustive`, `exhaustive_no_python`, `full`, `debug`,
`recommendations`, and for managed services `rds`, `aurora`, `azure`, `gce` and
`aiven`. [observability.md](observability.md#choosing-a-preset) describes them.

The list is that of the pgwatch release the operator is built for, also for a
monitor whose `spec.image` names another release. A monitor that only names a
preset runs on the definitions inside its image; a preset that this list has
and an older image lacks collects nothing there.

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

Standard Kubernetes `ResourceRequirements` (`requests`, `limits`) of the
pgwatch container, handed on as they are. The operator sets nothing of its own
when the field is left empty: what a `LimitRange` of the namespace gives a
container without requests then applies, and without one the pod is
`BestEffort`, the first to be evicted when its node runs short of memory. In a
namespace with a `ResourceQuota` on CPU or memory a pod without requests and
limits cannot be created at all; the monitor then reports `PodCreateFailed`
with the API server's message. Setting at least requests is recommended:

```yaml
spec:
  resources:
    requests:
      cpu: 50m
      memory: 64Mi
    limits:
      memory: 256Mi
```

How much memory a collector needs grows with the number of tables, indexes and
statements the preset reports on; watch it before settling on a limit.

### spec.image

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `repository` | string | no | `cybertecpostgresql/pgwatch` | pgwatch image repository |
| `tag` | string | no | `6.2.0` | Image tag. Leave unset to run the pgwatch release the operator is built for |
| `pullPolicy` | enum | no | `IfNotPresent` | `Always`, `IfNotPresent`, `Never`. Set on the pgwatch container as it is; the default also holds for the tag `latest`, which Kubernetes would otherwise pull at every start |
| `pullSecrets` | []object | no | | Secrets in the monitor's namespace with credentials for the image's registry, each as `- name: <secret>` (type `kubernetes.io/dockerconfigjson`). At most 8, each once. The kubelet reads them; the operator does not, and does not check that they exist: a missing one shows as `ImagePullBackOff` when the image cannot be pulled without it |

The defaults of `repository` and `tag` are applied by the operator when it
builds the collector pod; they are not written into the object. A monitor that
leaves them unset therefore moves to the newer pgwatch release when the
operator is upgraded, and one that sets them stays where it is. (Monitors
created under operator v1.0.2 or older had `tag: "5.1.0"` written into them;
see [troubleshooting](troubleshooting.md#collector-still-runs-an-old-pgwatch-after-an-operator-upgrade).) Setting a tag
is only needed to hold a monitor back. A monitor that only names a preset then
runs on the metric definitions of that release; one that combines a preset
with metrics of its own is handed the definitions of the operator's default
release whatever its tag (see [architecture.md](architecture.md#metricsyaml)).

### spec.scheduling

Where the collector pod may run. The fields are those of a pod and are passed
on as they are. What only a pod is refused for, a toleration with an operator
that does not exist for example, is reported when the collector is rolled
out: `ApplyRejected` with the API server's words, or `PodCreateFailed` for
what is only found when the pod is made (a priority class that does not
exist). A pod that no node can take shows as `Unschedulable`.

The fields are those of the Kubernetes version the operator is built with.
One that the cluster's version does not have yet (`matchLabelKeys` in a pod
affinity term before 1.29, `valueFrom.fileKeyRef` in `spec.env` before 1.34)
is accepted in the monitor and refused with the Deployment: `ApplyRejected`,
"field not declared in schema".

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `nodeSelector` | map[string]string | no | | The pod runs only on nodes with all of these labels. At most 32 entries; values have to be label values |
| `tolerations` | []Toleration | no | | Taints the pod tolerates. At most 32 |
| `affinity` | Affinity | no | | Node affinity, pod affinity and pod anti-affinity |
| `topologySpreadConstraints` | []TopologySpreadConstraint | no | | Spread over zones or nodes. A collector is one pod, so a constraint is of use with a `labelSelector` that takes in the collectors of the other monitors of the namespace as well, such as `app.kubernetes.io/managed-by: pgpilot-operator`; a constraint does not count pods of other namespaces (a pod anti-affinity term with a `namespaceSelector` does). At most 8, and as in a pod one for each pair of `topologyKey` and `whenUnsatisfiable` |
| `priorityClassName` | string | no | | PriorityClass of the pod |

```yaml
spec:
  scheduling:
    nodeSelector:
      pool: monitoring
    tolerations:
      - key: dedicated
        operator: Equal
        value: monitoring
        effect: NoSchedule
```

### spec.serviceAccountName

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `serviceAccountName` | string | no | `default` | ServiceAccount of the collector pod, in the monitor's namespace. The collector never talks to the Kubernetes API, and the kubelet mounts no token of the account into its pod. The account matters for what is bound to it: image pull secrets, a security context constraint on OpenShift, and an identity of a cloud provider. One that does not exist shows as `PodCreateFailed` |

Whoever may write a PgpilotMonitor can run a container, of an image of their
choosing, as any ServiceAccount of the namespace. That the kubelet mounts no
token does not take the account's cloud identity away: EKS and Azure
workload identity add a token of their own to the pod of an account that is
set up for them, and GKE hands the identity out without one. Give the
permission to write monitors to those you would let create pods.

### spec.env

Environment variables of the pgwatch container, after `PGUSER` and
`PGPASSWORD`, which the operator sets. At most 64, each name once.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `name` | string | yes | | Letters, digits and `_`, not starting with a digit; max 128 chars |
| `value` | string | no | | The value, max 4096 chars. `$(NAME)` refers to a variable that comes before it, as in any pod |
| `valueFrom` | EnvVarSource | no | | Take the value from a Secret, a ConfigMap or a field of the pod instead. Not together with `value` |

What they are for, and what not:

- settings of the PostgreSQL driver that the connection string does not
  carry: `PGOPTIONS` (`-c statement_timeout=30s`), `PGCONNECT_TIMEOUT`,
  `PGTZ`. What the connection string says wins: `PGHOST`, `PGPORT`,
  `PGDATABASE` and `PGSSLMODE` change nothing. Neither does `PGAPPNAME`:
  pgwatch names its sessions itself;
- settings of pgwatch that have no field of their own, such as
  `PW_MIN_DB_SIZE_MB` or `PW_INSTANCE_LEVEL_CACHE_MAX_SECONDS`. An option the
  operator passes on the command line wins over its variable, and the
  options of the sinks the operator does not use (`PW_BATCHING_DELAY`,
  `PW_RETENTION`, ...) change nothing;
- a proxy for the gRPC sink (`HTTPS_PROXY`, `NO_PROXY`).

`PGUSER`, `PGPASSWORD`, `PW_SOURCES`, `PW_METRICS`, `PW_SINK` and
`PW_WEBDISABLE` are refused: they are the credentials, and where pgwatch
finds its configuration and sends what it collects, which is the operator's
to say.

A Secret or ConfigMap that `valueFrom` names is read by the pod once, when it
starts. The operator does not look at it: one that is missing keeps the pod
from starting (`CreateContainerConfigError`), and one that changes does not
replace the pod. A Secret named here is readable by whoever may write the
monitor, as the credentials Secret is.

```yaml
spec:
  env:
    - name: PGOPTIONS
      value: "-c statement_timeout=30s"
```

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
| `configHash` | string | SHA-256 of the rendered `sources.yaml` + `metrics.yaml`. Changes whenever the underlying pgwatch Deployment is (or is about to be) rolled — useful for external tools tracking rollouts. Its first ten characters end the name of the ConfigMap that holds this configuration, `pgpilot-<name>-config-<hash>` |
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
| `interval` | duration | no | `60s` | Collection period as a Go duration (e.g. `30s`, `5m`); at least `1s`, used in whole seconds. A metric that replaces one of the monitor's preset by name and names no interval keeps the preset's |
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

# Observability

pgpilot-operator creates pgwatch pods that expose Prometheus-compatible metrics. This guide covers how to scrape them.

## Prometheus annotation-based discovery

The simplest approach. Add annotations to the pgwatch pod via `spec.podMetadata`:

```yaml
apiVersion: pgpilot.io/v1
kind: PgpilotMonitor
metadata:
  name: my-db
  namespace: my-team
spec:
  # ...
  podMetadata:
    annotations:
      prometheus.io/scrape: "true"
      prometheus.io/port: "9187"
      prometheus.io/path: "/metrics"
```

Works out of the box with vmagent, Prometheus with `kubernetes_sd_configs`, and any scraper that uses annotation-based target discovery.

Every series of a monitored database carries two labels that say whose it is:
`dbname`, the name of the monitor, and `namespace`, the namespace it is in.
pgwatch sets both, so they are there however the pod is scraped. (Its own
`pgwatch_exporter_*` series carry neither. A metric of yours with a
`tag_namespace` column has that column's value in `namespace` instead.)

If your scrape configuration puts a `namespace` label on the target itself,
Prometheus keeps the scraped one as `exported_namespace`. Where it only
repeats the target's namespace it can be removed; this rule does that and
leaves a `tag_namespace` value alone:

```yaml
metric_relabel_configs:
  - source_labels: [exported_namespace]
    regex: my-team               # the namespace of the monitors
    target_label: exported_namespace
    replacement: ""
```

A rule covers one namespace (Prometheus cannot compare two labels); for
monitors in several namespaces under one scrape job, write one per namespace.
The generated ServiceMonitor carries the rule for its own.

## vmagent relabeling example

If you use VictoriaMetrics vmagent with a `VMAgent` CR or scrape config:

```yaml
scrape_configs:
  - job_name: pgpilot-monitors
    kubernetes_sd_configs:
      - role: pod
    relabel_configs:
      - source_labels: [__meta_kubernetes_pod_annotation_prometheus_io_scrape]
        action: keep
        regex: "true"
      - source_labels: [__meta_kubernetes_pod_annotation_prometheus_io_port]
        action: replace
        target_label: __address__
        regex: (.+)
        replacement: ${1}:$1
      - source_labels: [__meta_kubernetes_pod_label_pgpilot_io_monitor]
        target_label: monitor
```

## ServiceMonitor (prometheus-operator)

If prometheus-operator is installed, pgpilot-operator auto-detects it and creates a `ServiceMonitor` for each `PgpilotMonitor` whose Prometheus sink is enabled (the default).

No extra configuration needed. The ServiceMonitor:
- Selects the pgwatch Service by labels
- Scrapes the `metrics` port
- Uses `interval: 30s`, `scrapeTimeout: 10s`
- Keeps the timestamps pgwatch gives its samples, see
  [below](#sample-timestamps-and-metrics-on-long-intervals)
- Removes `exported_namespace` where it repeats the target's `namespace`
  label, which is where pgwatch's own namespace label ends up. A changed
  ServiceMonitor takes a moment to reach Prometheus: after an upgrade the
  first scrapes of a new pod can still carry the label

To verify:

```bash
kubectl get servicemonitors -n my-team
```

### Label-based Service discovery

If your Prometheus uses `serviceMonitorSelector`, ensure the Service labels match. You can add labels via `spec.serviceMetadata`:

```yaml
spec:
  serviceMetadata:
    labels:
      release: kube-prometheus-stack
```

## Service annotations for direct scrape

Some setups scrape Services directly (not pods). Add annotations via `spec.serviceMetadata`:

```yaml
spec:
  serviceMetadata:
    annotations:
      prometheus.io/scrape: "true"
      prometheus.io/port: "9187"
```

## Sample timestamps and metrics on long intervals

pgwatch does not run a metric's query when it is scraped. It runs it on the
metric's own interval, keeps the result, and serves it at every scrape for ten
minutes, each sample stamped with the time its query ran.

Prometheus takes those timestamps over unless told otherwise, and so does the
ServiceMonitor the operator generates. That is what makes `rate()` over a
counter exact: a counter collected once a minute and scraped twice is still
one sample per minute, at the moment it was read.

The price is paid by metrics on long intervals. Prometheus stores a sample it
has seen before only once, and stops returning a series five minutes after
its last sample. A metric collected every ten minutes is there for five
minutes and gone for five; an alert with `for: 10m` on it never fires. In the
presets these are, among others, `table_io_stats` (10 minutes), `index_stats`
(15 minutes), `sequence_health` (1 hour) and `table_bloat_approx_summary_sql`
(2 hours). Ask for the last value over a window that covers the interval:

```promql
last_over_time(pgwatch_table_bloat_approx_summary_sql_approx_bloat_percentage[2h30m])
```

`max_over_time` does the same for a value that only matters at its worst, and
is what the example dashboard uses. In an alert, put the `*_over_time` in the
expression and keep `for:` short.

The other way is `honor_timestamps: false` in a scrape configuration of your
own (`honorTimestamps: false` in a ServiceMonitor of your own): a sample then
gets the time of the scrape, and a metric is there for as long as pgwatch
serves it, ten minutes after each collection. It is not what the operator
sets, because of what it does to counters. Read every 30 seconds, a counter
collected every 60 is a staircase, and `rate()` over a window swings by about
the share one collection has in that window: around ±11 % for `[5m]` on a
metric collected every minute, and between 0.7 and 1.0 of the true rate for
`[15m]` on one collected every five. `irate()` is of no use at all then.
Metrics collected less often than every ten minutes have gaps either way.

## Custom Prometheus port

The default Prometheus port is `9187` (pgwatch default). To change it:

```yaml
spec:
  sinks:
    prometheus:
      enabled: true
      port: 8080
  podMetadata:
    annotations:
      prometheus.io/port: "8080"
```

## Example Grafana dashboard

A starter dashboard is provided at
[`examples/grafana/pgpilot-overview.json`](../examples/grafana/pgpilot-overview.json).
It has five row-groups driven by the labels the operator emits
(`namespace`, `dbname`, `env`, `instance`, `table_name`, `query`, `locktype`):

- **Overview** — DB size, idle-in-transaction, xmin age, deadlocks
- **Backends & Locks** — connection state over time, locks by type
- **Tables & Bloat** — seq_scan rate, dead tuples, bloat %, unused indexes
- **Query performance** — top 10 slowest queries
- **WAL & I/O** — WAL generation rate, checkpoint rate

For local development, the helper at
[`hack/monitoring/setup.sh`](../hack/monitoring/README.md) brings up
kube-prometheus-stack in a separate namespace and imports the dashboard
automatically. **That helper is dev-only — kube-prometheus-stack is not
a pgpilot-operator dependency**, it's just a convenient way to run through
the full operator → Prometheus → Grafana path during development.

## Choosing a preset

A preset is a named list of pgwatch metrics with their collection intervals.
These are the presets of pgwatch 6.2.0, the release the operator runs by
default:

| Preset | Metrics | Collects |
|--------|--------:|----------|
| `minimal` | 2 | `instance_up` and `kpi`, one query with the key figures of the instance |
| `basic` | 4 | `instance_up`, `db_size`, `db_stats`, `wal` |
| `standard` | 11 | basic + `table_stats`, `index_stats`, `stat_statements`, `sproc_stats`, `sequence_health`, `cpu_load`, `autovacuum_scores` |
| `exhaustive` | 32 | standard + `backends`, `locks`, `bgwriter`, `checkpointer`, `replication`, `replication_slots`, `archiver`, `wal_size`, `table_io_stats`, `table_bloat_approx_summary_sql`, `stat_activity`, `settings_num` and more |
| `exhaustive_no_python` | 31 | exhaustive without `cpu_load` |
| `full` | 48 | exhaustive + the `reco_*` recommendations, `kpi`, `stat_ssl`, `logical_subscriptions` and the `psutil_*` operating-system metrics |
| `recommendations` | 8 | only the `reco_*` checks (missing or unused indexes, superusers, ...); rows appear only when there is something to report |
| `rds`, `aurora`, `azure`, `gce`, `aiven` | 22-32 | exhaustive without what the managed service does not expose |
| `debug` | 73 | everything, every 30 seconds; for development only |

Things to know before picking one:

- `cpu_load` and the `psutil_*` metrics need PL/Python helper functions in the
  monitored database, which the operator does not create. Where they are
  missing the collector logs an error for these metrics on every attempt and
  collects the rest. `exhaustive_no_python` is `exhaustive` without them and is
  the better choice unless you install the helpers yourself.
- `stat_statements` and `stat_statements_calls` need the `pg_stat_statements`
  extension in the monitored database; `reco_add_index` (presets
  `recommendations`, `full`, `aiven`) needs `pg_qualstats`. Without the
  extension the metric fails and is logged like the helper-based ones.
- The presets for managed services do not contain `instance_up`, so
  `pgwatch_instance_up` is not exported with value `1` for them. Alert on a
  series they do have going missing, for example
  `absent(pgwatch_db_stats_numbackends{dbname="<monitor>"})`.
- `autovacuum_scores`, `recovery` and `stat_lock` only have SQL for PostgreSQL
  19 and newer; on older servers they are skipped.
- Since pgwatch 6 the presets collect `settings_num` instead of `settings`:
  one series `pgwatch_settings_num_value{name="<setting>"}` per numeric server
  setting, instead of one series per setting
  (`pgwatch_settings_max_connections`, ...).
- pgwatch has more presets, for other kinds of sources: `patroni` and
  `postgres-exporter-basic` for its Prometheus-scraping sources, `pgbouncer`
  and `pgpool` for those poolers. They collect nothing from a PostgreSQL
  database and cannot be used for a monitor (see
  [presets](crd-reference.md#presets)).

The exact contents are in the registry the operator embeds,
[`internal/builder/pgwatchdata/metrics.yaml`](../internal/builder/pgwatchdata/metrics.yaml).
Built-in SQL exists for PostgreSQL 14 and newer; an older server gets
`instance_up` and little else.

### How pgwatch 6 collects

One worker per monitor fetches every metric that is due, over a single
connection and in one batch. A monitor therefore normally holds one session in
the monitored database (pgwatch 5 held up to four), and `pgwatch_backends_*` no
longer counts the collector's own extra sessions. The flip side: a slow or
lock-blocked metric query holds back every other metric of the same monitor,
see [query time](crd-reference.md#query-time).

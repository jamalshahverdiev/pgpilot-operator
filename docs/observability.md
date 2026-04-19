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

If prometheus-operator is installed, pgpilot-operator auto-detects it and creates a `ServiceMonitor` for each `PgpilotMonitor`.

No extra configuration needed. The ServiceMonitor:
- Selects the pgwatch Service by labels
- Scrapes the `metrics` port
- Uses `interval: 30s`, `scrapeTimeout: 10s`

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
(`dbname`, `env`, `instance`, `table_name`, `query`, `locktype`):

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

## Available metrics

pgwatch exposes metrics based on the selected preset. Examples:

| Preset | Typical metrics |
|--------|----------------|
| `basic` | `db_size`, `db_stats`, `wal`, `backends` |
| `standard` | basic + `table_stats`, `index_stats`, `stat_statements` |
| `exhaustive` | standard + `locks`, `blocking_locks`, `replication`, `bloat`, `sequences` |
| `rds` | tuned for AWS RDS (no WAL-related metrics) |
| `aurora` | tuned for AWS Aurora |

For the full list, see [pgwatch built-in metrics](https://github.com/cybertec-postgresql/pgwatch/blob/master/internal/metrics/metrics.yaml).

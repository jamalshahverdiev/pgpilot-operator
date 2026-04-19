# Local monitoring stack

Dev-only helper that deploys kube-prometheus-stack (Prometheus + Grafana) into
a separate `monitoring` namespace on the test cluster and imports a starter
Grafana dashboard for pgwatch metrics produced by `PgpilotMonitor` pods.

> **⚠ Out of scope notice.** kube-prometheus-stack is **not** part of
> pgpilot-operator. The operator emits metrics via its Prometheus (pull) and
> gRPC (push) sinks and auto-generates a `ServiceMonitor` when
> prometheus-operator CRDs are present. How you actually scrape and store
> those metrics — Prometheus, VictoriaMetrics, Datadog, Grafana Cloud,
> whatever — is your decision. This folder exists only so we can validate
> the full path end-to-end during development and produce screenshots for
> `docs/observability.md`.

## Prerequisites

- `kubectl`, `helm`, `python3`
- A Kubernetes cluster with pgpilot-operator already installed and at least
  one `PgpilotMonitor` in `Ready=True`

## Usage

```bash
# Install prom-stack + import the dashboard. Leaves a Grafana port-forward
# on http://localhost:13000 (admin/admin).
hack/monitoring/setup.sh

# Tear everything down when done.
hack/monitoring/setup.sh teardown
```

## What it does

1. Adds the `prometheus-community` Helm repo (idempotent)
2. Creates namespace `monitoring` (idempotent)
3. Installs/upgrades `kube-prometheus-stack` with settings that let
   Prometheus see `ServiceMonitor`s in **any** namespace (the default chart
   uses a Helm-values-based selector that filters by `release` label)
4. Restarts pgpilot-operator so it rediscovers the freshly-installed
   `monitoring.coreos.com/v1` CRD and starts creating `ServiceMonitor`s
5. Port-forwards Grafana, imports `examples/grafana/pgpilot-overview.json`

## Dashboard

The imported dashboard has five row-groups, all driven by labels our operator
emits (`dbname`, `env`, `instance`, `table_name`, `query`, `locktype`):

- **Overview** — DB size, idle-in-transaction, xmin age, deadlocks
- **Backends & Locks** — connection state over time, locks by type
- **Tables & Bloat** — seq_scan rate, dead tuples, bloat %, unused indexes
- **Query performance** — top 10 slowest queries
- **WAL & I/O** — WAL generation rate, checkpoint rate

Run the stress scenarios (`hack/testdb/stress/*`) to see the panels light up.

## Slow metrics

pgwatch's `exhaustive` preset collects a few metrics on long intervals.
In a freshly-deployed dev environment you will see "No data" on the
corresponding panels until the first collection completes:

| Metric | Default interval | Affected panel |
|---|---|---|
| `table_bloat_approx_summary_sql` | **2 hours** | Approx bloat % |
| `index_stats` | 15 min | Unused indexes |
| `table_io_stats` | 10 min | — |
| `sequence_health` | 1 hour | — |
| `settings` | 2 hours | — |

The dashboard wraps these in `max_over_time(...[N])` so the last value
stays visible between collections. This is the correct Prometheus pattern
for "rarely updated gauge" metrics and applies equally in production —
not just dev.

If you want faster collection for dev iteration, the cleanest path is
to provide a whole custom metric (name, full SQL, short interval) via
`spec.metrics.custom[]` under a *different* name — e.g.
`bloat_dev_fast` with `interval: 2m`. Pasting the exact built-in SQL
back into the spec to override only the interval works but is verbose
and ties you to a specific pgwatch release; prefer `max_over_time` in
the dashboard.

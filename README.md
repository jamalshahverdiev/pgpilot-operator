# pgpilot-operator

Kubernetes operator for declarative PostgreSQL monitoring via [pgwatch](https://github.com/cybertec-postgresql/pgwatch).

**pgpilot-operator** wraps pgwatch v5 in a Kubernetes-native operator so you can monitor any PostgreSQL database — including managed services like AWS RDS, Aurora, Cloud SQL, and Supabase — using CRDs, GitOps workflows, and standard Kubernetes tooling.

## Key Features

- **One CRD, one pgwatch pod** — each `PgpilotMonitor` creates an isolated pgwatch collector. Namespace-scoped, multi-tenant safe.
- **Reusable metric libraries** — `PgpilotMetricLibrary` lets teams share custom SQL metrics across monitors.
- **Prometheus / vmagent ready** — Prometheus sink enabled by default. Custom labels and annotations on pods and services for annotation-based discovery.
- **Optional ServiceMonitor** — auto-generated when prometheus-operator CRD is detected.
- **GitOps-first** — pgwatch runs in read-only file mode. The operator is the single source of truth.
- **Hardened** — non-root, read-only root filesystem, drop all capabilities, least-privilege RBAC.

## Quickstart

### Prerequisites

- Kubernetes >= 1.28
- Helm >= 3.x
- A PostgreSQL database to monitor
- A Secret with connection credentials

### Install via Helm

```bash
helm install pgpilot-operator oci://registry-1.docker.io/jamalshahverdiev/pgpilot-operator \
  --namespace pgpilot-system --create-namespace
```

### Create a Secret

```bash
kubectl create secret generic my-db-creds \
  --namespace my-team \
  --from-literal=username=pgwatch \
  --from-literal=password=changeme
```

### Create a PgpilotMonitor

```yaml
apiVersion: pgpilot.io/v1
kind: PgpilotMonitor
metadata:
  name: my-db
  namespace: my-team
spec:
  database:
    host: my-db.example.com
    port: 5432
    database: myapp
    sslmode: require
    credentialsSecret:
      name: my-db-creds
  metrics:
    preset: exhaustive
  sinks:
    prometheus:
      enabled: true
  podMetadata:
    annotations:
      prometheus.io/scrape: "true"
      prometheus.io/port: "9187"
```

```bash
kubectl apply -f monitor.yaml
```

Within seconds, the operator creates a pgwatch pod that starts collecting metrics.

### Verify

```bash
kubectl get pgpilotmonitors -n my-team
kubectl get pods -n my-team -l pgpilot.io/monitor=my-db
```

## Documentation

- [Quickstart](docs/quickstart.md) — step-by-step with local PostgreSQL
- [CRD Reference](docs/crd-reference.md) — every field of both CRDs
- [Architecture](docs/architecture.md) — how the operator produces pgwatch pods
- [Observability](docs/observability.md) — Prometheus, vmagent, ServiceMonitor, Grafana dashboard
- [Troubleshooting](docs/troubleshooting.md)

## Example Grafana dashboard

A starter dashboard for the metrics pgwatch produces is available at
[`examples/grafana/pgpilot-overview.json`](examples/grafana/pgpilot-overview.json).
Import it into your own Grafana, or use
[`hack/monitoring/setup.sh`](hack/monitoring/README.md) to spin up a
local Prometheus + Grafana stack for development and see it in action.

## CRDs

| Kind | Short Name | Description |
|------|-----------|-------------|
| `PgpilotMonitor` | `pm` | Declares a PostgreSQL database to monitor. Creates a pgwatch pod. |
| `PgpilotMetricLibrary` | `pml` | Reusable set of custom SQL metric definitions. |

## License

Apache License 2.0. See [LICENSE](LICENSE) for details.

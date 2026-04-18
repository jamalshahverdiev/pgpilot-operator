# Quickstart

This guide walks you through monitoring a local PostgreSQL instance with pgpilot-operator.

## Prerequisites

- A running Kubernetes cluster (minikube, kind, or any >=1.28)
- `kubectl` configured
- Helm 3.x installed
- A PostgreSQL instance accessible from the cluster

## 1. Install the operator

```bash
helm install pgpilot-operator oci://registry-1.docker.io/jamalshahverdiev/pgpilot-operator \
  --namespace pgpilot-system --create-namespace
```

Verify the operator is running:

```bash
kubectl get pods -n pgpilot-system
```

## 2. Create a namespace for your team

```bash
kubectl create namespace demo
```

## 3. Create a credentials Secret

```bash
kubectl create secret generic demo-db-creds \
  --namespace demo \
  --from-literal=username=pgwatch \
  --from-literal=password=your-password
```

The operator reads this Secret to build the pgwatch connection string. It never copies the Secret contents elsewhere.

## 4. Create a PgpilotMonitor

Save the following as `monitor.yaml`:

```yaml
apiVersion: pgpilot.io/v1
kind: PgpilotMonitor
metadata:
  name: demo-db
  namespace: demo
spec:
  database:
    host: my-postgres.demo.svc.cluster.local
    port: 5432
    database: myapp
    sslmode: disable
    credentialsSecret:
      name: demo-db-creds
    customTags:
      env: demo

  metrics:
    preset: basic

  sinks:
    prometheus:
      enabled: true
      port: 9187

  podMetadata:
    annotations:
      prometheus.io/scrape: "true"
      prometheus.io/port: "9187"
      prometheus.io/path: "/metrics"
```

Apply it:

```bash
kubectl apply -f monitor.yaml
```

## 5. Verify

```bash
# Check the monitor status
kubectl get pm -n demo

# Check the pgwatch pod
kubectl get pods -n demo -l pgpilot.io/monitor=demo-db

# Check the Service
kubectl get svc -n demo pgpilot-demo-db

# View conditions
kubectl describe pgpilotmonitor demo-db -n demo
```

Expected conditions:

- `DatabaseReachable: True` — credentials Secret found
- `ConfigGenerated: True` — ConfigMap with pgwatch config created
- `Ready: True` — pgwatch pod is running (may take a few seconds)

## 6. Check metrics

Port-forward to the pgwatch pod and verify Prometheus metrics are exposed:

```bash
kubectl port-forward -n demo svc/pgpilot-demo-db 9187:9187
curl http://localhost:9187/metrics | head -20
```

You should see pgwatch metric lines like `pgwatch_db_stats_*`, `pgwatch_backends_*`, etc.

## 7. Add custom metrics via PgpilotMetricLibrary

Save as `library.yaml`:

```yaml
apiVersion: pgpilot.io/v1
kind: PgpilotMetricLibrary
metadata:
  name: business-metrics
  namespace: demo
spec:
  metrics:
    - name: active_sessions
      description: "Active database sessions"
      interval: 30s
      sqls:
        "13": |
          SELECT (extract(epoch from now())*1e9)::int8 AS epoch_ns,
                 count(*) AS active
          FROM pg_stat_activity
          WHERE state = 'active'
      gauges:
        - "*"
```

```bash
kubectl apply -f library.yaml
```

Reference it from your monitor by adding to `spec.metrics`:

```yaml
spec:
  metrics:
    preset: basic
    fromLibraries:
      - name: business-metrics
```

The operator will re-reconcile and update the pgwatch ConfigMap automatically.

## Cleanup

```bash
kubectl delete -f monitor.yaml
kubectl delete -f library.yaml
kubectl delete namespace demo
helm uninstall pgpilot-operator -n pgpilot-system
```

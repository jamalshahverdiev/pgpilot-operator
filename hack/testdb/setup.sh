#!/usr/bin/env bash
# Start test PostgreSQL instances and deploy PgpilotMonitor CRs.
#
# Usage:
#   hack/testdb/setup.sh          # start DBs + deploy monitors
#   hack/testdb/setup.sh teardown # stop DBs + delete monitors
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
NS="pgpilot-test"
HOST_IP="${HOST_IP:-$(ip -4 addr show enp0s8 2>/dev/null | grep -oP 'inet \K[^/]+' || hostname -I | awk '{print $1}')}"

GREEN='\033[0;32m'; YELLOW='\033[1;33m'; NC='\033[0m'
info() { echo -e "${YELLOW}[INFO]${NC} $*"; }
ok()   { echo -e "${GREEN}[OK]${NC} $*"; }

# ---------------------------------------------------------------------------
teardown() {
    info "Tearing down..."
    kubectl delete namespace "$NS" --ignore-not-found --wait=false 2>/dev/null || true
    docker compose -f "$SCRIPT_DIR/docker-compose.yaml" down -v 2>/dev/null || true
    ok "Cleanup complete"
    exit 0
}

[[ "${1:-}" == "teardown" ]] && teardown

# ---------------------------------------------------------------------------
# 1. Start PostgreSQL containers
# ---------------------------------------------------------------------------
info "Starting PostgreSQL containers..."
docker compose -f "$SCRIPT_DIR/docker-compose.yaml" up -d --wait
ok "PostgreSQL primary (:15432), secondary (:15433), tertiary (:15434) are ready"

# ---------------------------------------------------------------------------
# 2. Verify connectivity (retry — init scripts may still be running)
# ---------------------------------------------------------------------------
wait_for_psql() {
    local port="$1" db="$2" label="$3"
    for _ in $(seq 1 30); do
        if PGPASSWORD=pgwatch_secret psql -h 127.0.0.1 -p "$port" -U pgwatch -d "$db" \
            -c "SELECT 1;" -t -q >/dev/null 2>&1; then
            return 0
        fi
        sleep 2
    done
    echo "[ERROR] timeout waiting for ${label} at :${port}/${db}" >&2
    return 1
}

info "Verifying pgwatch user can connect..."
wait_for_psql 15432 app_primary "primary"
PGPASSWORD=pgwatch_secret psql -h 127.0.0.1 -p 15432 -U pgwatch -d app_primary -c "SELECT count(*) FROM users;" -t -q | tr -d ' '
ok "primary: pgwatch connected, users table has data"

wait_for_psql 15433 app_secondary "secondary"
PGPASSWORD=pgwatch_secret psql -h 127.0.0.1 -p 15433 -U pgwatch -d app_secondary -c "SELECT count(*) FROM payments;" -t -q | tr -d ' '
ok "secondary: pgwatch connected, payments table has data"

wait_for_psql 15434 app_tertiary "tertiary"
PGPASSWORD=pgwatch_secret psql -h 127.0.0.1 -p 15434 -U pgwatch -d app_tertiary -c "SELECT count(*) FROM events;" -t -q | tr -d ' '
ok "tertiary: pgwatch connected, events table has data"

# ---------------------------------------------------------------------------
# 3. Create Kubernetes resources
# ---------------------------------------------------------------------------
info "Host IP for Kubernetes access: ${HOST_IP}"

info "Creating namespace ${NS}..."
kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f -

info "Creating credentials Secret..."
kubectl create secret generic pgwatch-creds \
  --namespace "$NS" \
  --from-literal=username=pgwatch \
  --from-literal=password=pgwatch_secret \
  --dry-run=client -o yaml | kubectl apply -f -

info "Creating PgpilotMetricLibrary..."
cat <<EOF | kubectl apply -f -
apiVersion: pgpilot.io/v1
kind: PgpilotMetricLibrary
metadata:
  name: business-metrics
  namespace: ${NS}
spec:
  metrics:
    - name: active_users
      description: "Users seen within last 5 minutes"
      interval: 30s
      sqls:
        "13": |
          SELECT (extract(epoch from now())*1e9)::int8 AS epoch_ns,
                 count(*) AS active
          FROM users
          WHERE last_seen > now() - interval '5 min'
      gauges: ["*"]
    - name: stuck_orders
      description: "Orders stuck in pending for over 10 minutes"
      interval: 60s
      sqls:
        "13": |
          SELECT (extract(epoch from now())*1e9)::int8 AS epoch_ns,
                 count(*) AS stuck,
                 coalesce(max(extract(epoch from now() - created_at))::int, 0) AS max_age_sec
          FROM orders
          WHERE status = 'pending'
            AND created_at < now() - interval '10 min'
      gauges: ["*"]
EOF

info "Creating PgpilotMetricLibrary for tertiary (analytics)..."
cat <<EOF | kubectl apply -f -
apiVersion: pgpilot.io/v1
kind: PgpilotMetricLibrary
metadata:
  name: tertiary-metrics
  namespace: ${NS}
spec:
  metrics:
    - name: events_by_type
      description: "Event counts by type in the last 5 minutes"
      interval: 30s
      sqls:
        "13": |
          SELECT (extract(epoch from now())*1e9)::int8 AS epoch_ns,
                 event_type AS tag_event_type,
                 count(*) AS count
          FROM events
          WHERE occurred_at > now() - interval '5 min'
          GROUP BY event_type
      gauges: ["*"]
    - name: pending_reports
      description: "Reports stuck in pending/running over 5 minutes"
      interval: 60s
      sqls:
        "13": |
          SELECT (extract(epoch from now())*1e9)::int8 AS epoch_ns,
                 count(*) FILTER (WHERE status = 'pending') AS pending,
                 count(*) FILTER (WHERE status = 'running') AS running,
                 coalesce(max(extract(epoch from now() - computed_at))::int, 0) AS max_age_sec
          FROM reports
          WHERE status IN ('pending', 'running')
            AND computed_at < now() - interval '5 min'
      gauges: ["*"]
EOF

info "Creating PgpilotMonitor for primary DB..."
cat <<EOF | kubectl apply -f -
apiVersion: pgpilot.io/v1
kind: PgpilotMonitor
metadata:
  name: primary-db
  namespace: ${NS}
spec:
  database:
    host: "${HOST_IP}"
    port: 15432
    database: app_primary
    sslmode: disable
    credentialsSecret:
      name: pgwatch-creds
    customTags:
      env: test
      instance: primary
  metrics:
    # Production-like combination: pgwatch's exhaustive preset for all standard
    # monitoring + a library of shared business metrics + an inline custom one.
    # The operator merges our metrics with pgwatch's embedded built-in registry
    # so the preset resolves correctly alongside custom definitions.
    preset: exhaustive
    fromLibraries:
      - name: business-metrics
    custom:
      - name: primary_row_count
        description: "Total number of rows in users table"
        interval: 60s
        sqls:
          "13": |
            SELECT (extract(epoch from now())*1e9)::int8 AS epoch_ns,
                   count(*) AS rows
            FROM users
        gauges: ["*"]
  sinks:
    prometheus:
      enabled: true
      port: 9187
  image:
    repository: cybertecpostgresql/pgwatch
    tag: "5.1.0"
  podMetadata:
    labels:
      team: test
    annotations:
      prometheus.io/scrape: "true"
      prometheus.io/port: "9187"
      prometheus.io/path: "/metrics"
EOF

info "Creating PgpilotMonitor for secondary DB..."
cat <<EOF | kubectl apply -f -
apiVersion: pgpilot.io/v1
kind: PgpilotMonitor
metadata:
  name: secondary-db
  namespace: ${NS}
spec:
  database:
    host: "${HOST_IP}"
    port: 15433
    database: app_secondary
    sslmode: disable
    credentialsSecret:
      name: pgwatch-creds
    customTags:
      env: test
      instance: secondary
  metrics:
    preset: exhaustive
  sinks:
    prometheus:
      enabled: true
      port: 9187
  image:
    repository: cybertecpostgresql/pgwatch
    tag: "5.1.0"
  podMetadata:
    labels:
      team: test
    annotations:
      prometheus.io/scrape: "true"
      prometheus.io/port: "9187"
      prometheus.io/path: "/metrics"
EOF

info "Creating PgpilotMonitor for tertiary DB..."
cat <<EOF | kubectl apply -f -
apiVersion: pgpilot.io/v1
kind: PgpilotMonitor
metadata:
  name: tertiary-db
  namespace: ${NS}
spec:
  database:
    host: "${HOST_IP}"
    port: 15434
    database: app_tertiary
    sslmode: disable
    credentialsSecret:
      name: pgwatch-creds
    customTags:
      env: test
      instance: tertiary
  metrics:
    preset: exhaustive
    fromLibraries:
      - name: tertiary-metrics
    custom:
      - name: tertiary_tenant_count
        description: "Total active tenants (inline custom metric)"
        interval: 60s
        sqls:
          "13": |
            SELECT (extract(epoch from now())*1e9)::int8 AS epoch_ns,
                   count(*) AS tenants
            FROM tenants
        gauges: ["*"]
  sinks:
    prometheus:
      enabled: true
      port: 9187
  image:
    repository: cybertecpostgresql/pgwatch
    tag: "5.1.0"
  podMetadata:
    labels:
      team: test
    annotations:
      prometheus.io/scrape: "true"
      prometheus.io/port: "9187"
      prometheus.io/path: "/metrics"
EOF

# ---------------------------------------------------------------------------
# 4. Wait for monitors
# ---------------------------------------------------------------------------
info "Waiting for pgwatch pods to be ready..."
kubectl wait --for=condition=Available deployment/pgpilot-primary-db -n "$NS" --timeout=90s
kubectl wait --for=condition=Available deployment/pgpilot-secondary-db -n "$NS" --timeout=90s
kubectl wait --for=condition=Available deployment/pgpilot-tertiary-db -n "$NS" --timeout=90s

echo ""
ok "Test environment ready!"
echo ""
echo "  Namespace:     ${NS}"
echo "  Primary DB:    ${HOST_IP}:15432/app_primary"
echo "  Secondary DB:  ${HOST_IP}:15433/app_secondary"
echo "  Tertiary DB:   ${HOST_IP}:15434/app_tertiary"
echo ""
kubectl get pm -n "$NS"
echo ""
echo "Useful commands:"
echo "  kubectl logs -n ${NS} -l pgpilot.io/monitor=primary-db -f"
echo "  kubectl logs -n ${NS} -l pgpilot.io/monitor=secondary-db -f"
echo "  kubectl logs -n ${NS} -l pgpilot.io/monitor=tertiary-db -f"
echo "  kubectl port-forward -n ${NS} svc/pgpilot-primary-db 9187:9187"
echo "  curl http://localhost:9187/metrics | head"
echo ""
echo "  hack/testdb/setup.sh teardown   # to clean up"

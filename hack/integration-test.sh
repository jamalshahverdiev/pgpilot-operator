#!/usr/bin/env bash
# pgpilot-operator Live Integration Test
# Requires:
#  - kubectl configured against a cluster with the operator deployed
#  - CRDs installed
#
# Tests:
#  1) Create PgpilotMonitor → ConfigMap, Deployment, Service created
#  2) Status conditions: DatabaseReachable, ConfigGenerated
#  3) Config hash triggers rollout on spec change
#  4) PgpilotMetricLibrary reference works
#  5) Missing Secret → Ready=False, SecretNotFound
#  6) Cleanup
set -euo pipefail

GREEN='\033[0;32m'; RED='\033[0;31m'; YELLOW='\033[1;33m'; NC='\033[0m'
PASS=0; FAIL=0
ok()   { echo -e "${GREEN}[PASS]${NC} $*"; PASS=$((PASS+1)); }
fail() { echo -e "${RED}[FAIL]${NC} $*"; FAIL=$((FAIL+1)); }
info() { echo -e "${YELLOW}[INFO]${NC} $*"; }

TEST_NS="pgpilot-integration-test"

trap 'info "Cleaning up..."; kubectl delete namespace "$TEST_NS" --ignore-not-found --wait=false 2>/dev/null' EXIT

# ---------------------------------------------------------------------------
# 0. Setup namespace + Secret
# ---------------------------------------------------------------------------
info "Creating test namespace ${TEST_NS}..."
kubectl create namespace "$TEST_NS" --dry-run=client -o yaml | kubectl apply -f -

info "Creating credentials Secret..."
kubectl create secret generic test-db-creds \
  --namespace "$TEST_NS" \
  --from-literal=username=pgwatch \
  --from-literal=password=testpass \
  --dry-run=client -o yaml | kubectl apply -f -

# ---------------------------------------------------------------------------
# 1. Create PgpilotMonitor
# ---------------------------------------------------------------------------
info "Test 1: Create PgpilotMonitor"
cat <<EOF | kubectl apply -f -
apiVersion: pgpilot.io/v1
kind: PgpilotMonitor
metadata:
  name: test-db
  namespace: ${TEST_NS}
spec:
  database:
    host: pg.example.com
    port: 5432
    database: testdb
    sslmode: disable
    credentialsSecret:
      name: test-db-creds
    customTags:
      env: integration-test
  metrics:
    preset: basic
  sinks:
    prometheus:
      enabled: true
      port: 9187
  image:
    repository: cybertecpostgresql/pgwatch
    tag: "5.1.0"
    pullPolicy: IfNotPresent
  podMetadata:
    labels:
      test-run: "true"
    annotations:
      prometheus.io/scrape: "true"
EOF

# ---------------------------------------------------------------------------
# 2. Verify child resources created
# ---------------------------------------------------------------------------
info "Waiting for child resources (up to 30s)..."
for i in $(seq 1 15); do
  sleep 2
  CM=$(kubectl get configmap pgpilot-test-db-config -n "$TEST_NS" -o name 2>/dev/null || echo "")
  DEP=$(kubectl get deployment pgpilot-test-db -n "$TEST_NS" -o name 2>/dev/null || echo "")
  SVC=$(kubectl get service pgpilot-test-db -n "$TEST_NS" -o name 2>/dev/null || echo "")
  if [[ -n "$CM" && -n "$DEP" && -n "$SVC" ]]; then break; fi
  echo -n "."
done
echo ""

[[ -n "$CM" ]]  && ok "ConfigMap pgpilot-test-db-config created"  || fail "ConfigMap not found"
[[ -n "$DEP" ]] && ok "Deployment pgpilot-test-db created"        || fail "Deployment not found"
[[ -n "$SVC" ]] && ok "Service pgpilot-test-db created"           || fail "Service not found"

# ---------------------------------------------------------------------------
# 3. Verify ConfigMap content
# ---------------------------------------------------------------------------
info "Test 2: ConfigMap content"
SOURCES=$(kubectl get configmap pgpilot-test-db-config -n "$TEST_NS" -o jsonpath='{.data.sources\.yaml}' 2>/dev/null || echo "")
if echo "$SOURCES" | grep -q "pg.example.com:5432"; then
  ok "sources.yaml contains host:port"
else
  fail "sources.yaml missing host:port"
fi
if echo "$SOURCES" | grep -q "preset_metrics: basic"; then
  ok "sources.yaml contains preset_metrics"
else
  fail "sources.yaml missing preset_metrics"
fi
if echo "$SOURCES" | grep -q 'env: integration-test'; then
  ok "sources.yaml contains custom_tags"
else
  fail "sources.yaml missing custom_tags"
fi

# ---------------------------------------------------------------------------
# 4. Verify Deployment spec
# ---------------------------------------------------------------------------
info "Test 3: Deployment spec"
IMG=$(kubectl get deployment pgpilot-test-db -n "$TEST_NS" -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null || echo "")
if [[ "$IMG" == "cybertecpostgresql/pgwatch:5.1.0" ]]; then
  ok "Deployment uses default pgwatch image"
else
  fail "Unexpected image: $IMG"
fi

HASH=$(kubectl get deployment pgpilot-test-db -n "$TEST_NS" -o jsonpath='{.spec.template.metadata.annotations.pgpilot\.io/config-hash}' 2>/dev/null || echo "")
if [[ -n "$HASH" && ${#HASH} -ge 20 ]]; then
  ok "config-hash annotation present (${HASH:0:12}...)"
else
  fail "config-hash annotation missing or too short: $HASH"
fi

TEST_LABEL=$(kubectl get deployment pgpilot-test-db -n "$TEST_NS" -o jsonpath='{.spec.template.metadata.labels.test-run}' 2>/dev/null || echo "")
[[ "$TEST_LABEL" == "true" ]] && ok "podMetadata labels merged" || fail "podMetadata label missing"

SCRAPE_ANN=$(kubectl get deployment pgpilot-test-db -n "$TEST_NS" -o jsonpath='{.spec.template.metadata.annotations.prometheus\.io/scrape}' 2>/dev/null || echo "")
[[ "$SCRAPE_ANN" == "true" ]] && ok "podMetadata annotations merged" || fail "podMetadata annotation missing"

# ---------------------------------------------------------------------------
# 5. Verify Service
# ---------------------------------------------------------------------------
info "Test 4: Service"
SVC_PORT=$(kubectl get service pgpilot-test-db -n "$TEST_NS" -o jsonpath='{.spec.ports[0].port}' 2>/dev/null || echo "")
[[ "$SVC_PORT" == "9187" ]] && ok "Service port 9187" || fail "Service port: $SVC_PORT"

SVC_SELECTOR=$(kubectl get service pgpilot-test-db -n "$TEST_NS" -o jsonpath='{.spec.selector.app\.kubernetes\.io/instance}' 2>/dev/null || echo "")
[[ "$SVC_SELECTOR" == "test-db" ]] && ok "Service selector matches monitor" || fail "Service selector: $SVC_SELECTOR"

# ---------------------------------------------------------------------------
# 6. Verify status conditions
# ---------------------------------------------------------------------------
info "Test 5: Status conditions"
sleep 5

DB_REACHABLE=$(kubectl get pgpilotmonitor test-db -n "$TEST_NS" -o jsonpath='{.status.conditions[?(@.type=="DatabaseReachable")].status}' 2>/dev/null || echo "")
[[ "$DB_REACHABLE" == "True" ]] && ok "DatabaseReachable=True" || fail "DatabaseReachable: $DB_REACHABLE"

CONFIG_GEN=$(kubectl get pgpilotmonitor test-db -n "$TEST_NS" -o jsonpath='{.status.conditions[?(@.type=="ConfigGenerated")].status}' 2>/dev/null || echo "")
[[ "$CONFIG_GEN" == "True" ]] && ok "ConfigGenerated=True" || fail "ConfigGenerated: $CONFIG_GEN"

FINALIZER=$(kubectl get pgpilotmonitor test-db -n "$TEST_NS" -o jsonpath='{.metadata.finalizers[0]}' 2>/dev/null || echo "")
[[ "$FINALIZER" == "pgpilot.io/finalizer" ]] && ok "Finalizer set" || fail "Finalizer: $FINALIZER"

OBS_GEN=$(kubectl get pgpilotmonitor test-db -n "$TEST_NS" -o jsonpath='{.status.observedGeneration}' 2>/dev/null || echo "0")
[[ "$OBS_GEN" -ge 1 ]] && ok "observedGeneration >= 1" || fail "observedGeneration: $OBS_GEN"

OWNER=$(kubectl get configmap pgpilot-test-db-config -n "$TEST_NS" -o jsonpath='{.metadata.ownerReferences[0].kind}' 2>/dev/null || echo "")
[[ "$OWNER" == "PgpilotMonitor" ]] && ok "OwnerReference on ConfigMap" || fail "OwnerReference kind: $OWNER"

# ---------------------------------------------------------------------------
# 7. Config hash changes on spec update
# ---------------------------------------------------------------------------
info "Test 6: Config hash rollout on spec change"
OLD_HASH="$HASH"
kubectl patch pgpilotmonitor test-db -n "$TEST_NS" --type=merge \
  -p '{"spec":{"metrics":{"preset":"exhaustive"}}}'
sleep 5

NEW_HASH=$(kubectl get deployment pgpilot-test-db -n "$TEST_NS" -o jsonpath='{.spec.template.metadata.annotations.pgpilot\.io/config-hash}' 2>/dev/null || echo "")
if [[ -n "$NEW_HASH" && "$NEW_HASH" != "$OLD_HASH" ]]; then
  ok "config-hash changed after spec update (${OLD_HASH:0:8} -> ${NEW_HASH:0:8})"
else
  fail "config-hash did not change: old=$OLD_HASH new=$NEW_HASH"
fi

# ---------------------------------------------------------------------------
# 8. PgpilotMetricLibrary reference
# ---------------------------------------------------------------------------
info "Test 7: PgpilotMetricLibrary reference"
cat <<EOF | kubectl apply -f -
apiVersion: pgpilot.io/v1
kind: PgpilotMetricLibrary
metadata:
  name: test-lib
  namespace: ${TEST_NS}
spec:
  metrics:
    - name: custom_test_metric
      sqls:
        "13": "SELECT 1 AS val"
      gauges: ["*"]
EOF
sleep 2

kubectl patch pgpilotmonitor test-db -n "$TEST_NS" --type=merge \
  -p '{"spec":{"metrics":{"fromLibraries":[{"name":"test-lib"}]}}}'
sleep 5

METRICS_YAML=$(kubectl get configmap pgpilot-test-db-config -n "$TEST_NS" -o jsonpath='{.data.metrics\.yaml}' 2>/dev/null || echo "")
if echo "$METRICS_YAML" | grep -q "custom_test_metric"; then
  ok "Library metrics merged into ConfigMap"
else
  fail "Library metric not found in ConfigMap metrics.yaml"
fi

LIB_VALID=$(kubectl get pgpilotmetriclibrary test-lib -n "$TEST_NS" -o jsonpath='{.status.conditions[?(@.type=="Valid")].status}' 2>/dev/null || echo "")
[[ "$LIB_VALID" == "True" ]] && ok "PgpilotMetricLibrary Valid=True" || fail "Library Valid: $LIB_VALID"

# ---------------------------------------------------------------------------
# 9. Missing Secret → Ready=False
# ---------------------------------------------------------------------------
info "Test 8: Missing Secret produces Ready=False"
cat <<EOF | kubectl apply -f -
apiVersion: pgpilot.io/v1
kind: PgpilotMonitor
metadata:
  name: bad-secret-db
  namespace: ${TEST_NS}
spec:
  database:
    host: pg.example.com
    database: testdb
    credentialsSecret:
      name: nonexistent-secret
  metrics:
    preset: basic
  sinks:
    prometheus:
      enabled: true
EOF
sleep 10

READY_STATUS=$(kubectl get pgpilotmonitor bad-secret-db -n "$TEST_NS" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || echo "")
READY_REASON=$(kubectl get pgpilotmonitor bad-secret-db -n "$TEST_NS" -o jsonpath='{.status.conditions[?(@.type=="Ready")].reason}' 2>/dev/null || echo "")
[[ "$READY_STATUS" == "False" ]] && ok "Ready=False for missing secret" || fail "Ready status: $READY_STATUS"
[[ "$READY_REASON" == "SecretNotFound" ]] && ok "Reason=SecretNotFound" || fail "Ready reason: $READY_REASON"

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------
echo ""
echo "============================================"
echo -e "Results: ${GREEN}${PASS} passed${NC}, ${RED}${FAIL} failed${NC}"
echo "============================================"

if [[ $FAIL -gt 0 ]]; then exit 1; fi

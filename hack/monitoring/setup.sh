#!/usr/bin/env bash
# Deploy kube-prometheus-stack into the `monitoring` namespace and import
# the pgpilot starter dashboard into Grafana.
#
# THIS IS A DEV-ONLY HELPER. kube-prometheus-stack is NOT part of
# pgpilot-operator. The operator exposes metrics via Prometheus/gRPC
# sinks and auto-generates a ServiceMonitor when prometheus-operator's
# CRDs are present. How the user scrapes those metrics is their choice.
#
# Usage:
#   hack/monitoring/setup.sh           # install + import dashboard
#   hack/monitoring/setup.sh teardown  # helm uninstall + delete namespace

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
NS="monitoring"
RELEASE="kube-prometheus-stack"

GREEN='\033[0;32m'; YELLOW='\033[1;33m'; NC='\033[0m'
info() { echo -e "${YELLOW}[INFO]${NC} $*"; }
ok()   { echo -e "${GREEN}[OK]${NC} $*"; }

teardown() {
    info "uninstalling ${RELEASE} from ${NS}..."
    helm uninstall "$RELEASE" -n "$NS" 2>/dev/null || true
    kubectl delete namespace "$NS" --ignore-not-found --wait=false 2>/dev/null || true
    ok "teardown complete"
    exit 0
}

[[ "${1:-}" == "teardown" ]] && teardown

info "adding/updating helm repo prometheus-community..."
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts 2>/dev/null || true
helm repo update prometheus-community >/dev/null

info "creating namespace ${NS}..."
kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f -

info "installing ${RELEASE}... (can take several minutes on first run)"
helm upgrade --install "$RELEASE" prometheus-community/kube-prometheus-stack \
    --namespace "$NS" \
    --set prometheus.prometheusSpec.serviceMonitorSelectorNilUsesHelmValues=false \
    --set 'prometheus.prometheusSpec.serviceMonitorNamespaceSelector=' \
    --set 'prometheus.prometheusSpec.serviceMonitorSelector=' \
    --set grafana.adminPassword=admin \
    --set alertmanager.enabled=false \
    --set prometheus.prometheusSpec.retention=2d \
    --wait --timeout 10m

ok "${RELEASE} installed"

info "restarting pgpilot-operator so it picks up the ServiceMonitor CRD (if it was installed before prom-stack)..."
kubectl rollout restart deployment/pgpilot-operator-controller-manager -n pgpilot-operator-system 2>/dev/null || true

info "starting temporary port-forward to Grafana for dashboard import..."
kubectl port-forward -n "$NS" "svc/${RELEASE}-grafana" 13000:80 >/dev/null 2>&1 &
PF_PID=$!
trap 'kill $PF_PID 2>/dev/null || true' EXIT
sleep 5

info "importing starter dashboard..."
python3 - <<PYEOF
import json, urllib.request, base64
with open("${REPO_ROOT}/examples/grafana/pgpilot-overview.json") as f:
    dash = json.load(f)
def fix(o):
    if isinstance(o, dict):
        if o.get("uid") == "\${DS_PROMETHEUS}":
            o.update({"type": "prometheus", "uid": "prometheus"})
        for v in o.values(): fix(v)
    elif isinstance(o, list):
        for v in o: fix(v)
fix(dash)
req = urllib.request.Request(
    "http://localhost:13000/api/dashboards/db",
    data=json.dumps({"dashboard": dash, "overwrite": True, "folderId": 0}).encode(),
    headers={
        "Content-Type": "application/json",
        "Authorization": "Basic " + base64.b64encode(b"admin:admin").decode(),
    },
)
with urllib.request.urlopen(req) as resp:
    print("  dashboard:", json.load(resp).get("url", "(unknown)"))
PYEOF

ok "dashboard imported"
echo ""
echo "  Grafana:    http://localhost:13000  (admin/admin) — port-forward already running"
echo "  Dashboard:  http://localhost:13000/d/pgpilot-overview/pgpilot-operator-overview"
echo "  Prometheus: kubectl port-forward -n ${NS} svc/${RELEASE}-prometheus 9090:9090"
echo ""
echo "  Teardown:   hack/monitoring/setup.sh teardown"

# leave port-forward running
trap - EXIT
wait $PF_PID

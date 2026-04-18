# Changelog

## v1.0.0

Initial release of pgpilot-operator.

### Features

- `PgpilotMonitor` CRD (`pgpilot.io/v1`) — one CR per monitored PostgreSQL database, creates an isolated pgwatch v5 pod
- `PgpilotMetricLibrary` CRD — reusable sets of custom SQL metric definitions, same-namespace references
- Reconciliation: ConfigMap (sources.yaml + metrics.yaml), Deployment, Service, optional ServiceMonitor
- Metric merge: pgwatch built-in presets + libraries + inline custom metrics, later entries override by name
- Prometheus sink enabled by default; gRPC sink as pass-through option
- User-controlled `podMetadata` / `serviceMetadata` (labels + annotations) for vmagent / Prometheus annotation-based discovery
- Automatic ServiceMonitor generation when prometheus-operator CRD is detected
- Content-hash annotation (`pgpilot.io/config-hash`) for automatic rollouts on config change
- Status conditions: `Ready`, `ConfigGenerated`, `DatabaseReachable`
- Finalizer for clean deletion
- Event recorder: Normal/Warning events for key lifecycle transitions
- Hardened security: non-root, read-only root FS, drop ALL capabilities, least-privilege RBAC
- Helm chart with toggles for CRDs, leader election, RBAC, ServiceMonitor
- GitHub Actions CI: lint (Go + Helm), unit tests, envtest, e2e (kind)
- Release pipeline: goreleaser, Docker image (ghcr.io), Helm OCI push
- Documentation: quickstart, CRD reference, architecture, observability, troubleshooting

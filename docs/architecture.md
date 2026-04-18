# Architecture

## Overview

pgpilot-operator is a Kubernetes operator that manages [pgwatch v5](https://github.com/cybertec-postgresql/pgwatch) instances declaratively through CRDs. It does not reinvent metrics collection — pgwatch handles that. The operator handles the Kubernetes lifecycle.

## Core design decisions

1. **One PgpilotMonitor = one pgwatch pod.** Even though pgwatch can monitor many databases from a single process, we use one pod per database for namespace isolation, small blast radius, and a simple mental model.

2. **pgwatch runs in file-based (read-only) mode.** The operator generates `sources.yaml` and `metrics.yaml`, mounts them via a ConfigMap, and passes `--sources` / `--metrics` flags. pgwatch cannot mutate its own configuration.

3. **This operator only observes.** It does not act on metrics. Future action operators (bloat, advisor, emergency) are separate projects.

## Resource flow

```
PgpilotMonitor CR
       │
       ▼
  ┌─────────────────────────────────────────┐
  │         PgpilotMonitor Controller       │
  │                                         │
  │  1. Validate credentials source         │
  │     (inline or Secret — exactly one)    │
  │  2. Resolve PgpilotMetricLibrary refs   │
  │  3. Merge metrics (preset + libs + custom) │
  │  4. Generate ConfigMap (sources.yaml +  │
  │     metrics.yaml + SHA-256 hash)        │
  │  5. Generate Deployment (1 replica)     │
  │  6. Generate Service (ClusterIP)        │
  │  7. Generate ServiceMonitor (optional)  │
  │  8. Update status + conditions          │
  └─────────────────────────────────────────┘
       │
       ▼
  ┌───────────────┐  ┌────────────────┐  ┌──────────┐  ┌────────────────┐
  │   ConfigMap   │  │  Deployment    │  │ Service  │  │ ServiceMonitor │
  │ sources.yaml  │  │  1 replica     │  │ :9187    │  │ (if prom-op)   │
  │ metrics.yaml  │  │  pgwatch pod   │  │          │  │                │
  └───────────────┘  └────────────────┘  └──────────┘  └────────────────┘
```

All child resources have `OwnerReferences` pointing to the `PgpilotMonitor`, so garbage collection is automatic on deletion.

## ConfigMap content

### sources.yaml

A single-element list in pgwatch format. The `conn_str` never contains
credentials — pgwatch's pgx driver picks up `PGUSER` / `PGPASSWORD`
from the environment:

```yaml
- name: <monitor-name>
  conn_str: postgresql://<host>:<port>/<db>?sslmode=<mode>
  kind: postgres
  preset_metrics: <preset>          # if only preset is set (no custom)
  custom_metrics:                   # if custom definitions are set, or
                                    # if preset + custom are combined
                                    # (operator expands preset into this)
    metric_name: <interval_seconds>
  is_enabled: true
  custom_tags:
    env: prod
```

`PGUSER` / `PGPASSWORD` come from either:
- inline `spec.database.username` / `password` (plain env values), or
- `spec.database.credentialsSecret` (via `valueFrom.secretKeyRef`).

### metrics.yaml

Present whenever `spec.metrics` has either a preset or custom definitions
(i.e. in almost all real cases). The operator embeds pgwatch's own
`metrics.yaml` at build time via `go:embed`, merges user custom metrics
on top, and emits the combined file — this is required because pgwatch's
`--metrics <file>` flag **replaces** its built-in registry rather than
merging, so a custom metrics.yaml without the built-in metric
definitions would break preset resolution.

```yaml
metrics:
  # All pgwatch built-in metrics (when preset is used) + user custom metrics.
  metric_name:
    sqls:
      13: |
        SELECT ...
    gauges: ["*"]
    is_instance_level: false
presets:
  # All built-in presets, shipped so preset_metrics still resolves.
  exhaustive:
    description: ...
    metrics:
      db_stats: 60
      ...
```

## Config hash rollout

The ConfigMap content is hashed (SHA-256). The hash is set as the `pgpilot.io/config-hash` annotation on the Deployment's pod template. When the config changes, the hash changes, triggering a rolling update.

## Metric merge order

When a PgpilotMonitor references a preset, libraries, and inline custom metrics:

1. **Libraries** — resolved in the order listed in `spec.metrics.fromLibraries`
2. **Inline custom** — from `spec.metrics.custom` (later wins on name collision)

The selected **preset** is then combined with the above depending on what's
present:

- **Preset only** (no custom): `preset_metrics: <name>` is set in
  sources.yaml; pgwatch resolves it from its metrics.yaml
- **Custom only** (no preset): `custom_metrics: {...}` is set
- **Both preset + custom**: the operator expands the preset's metric list
  into `custom_metrics` (user intervals override preset intervals on name
  collision) and **does not** set `preset_metrics`. pgwatch's
  preset-resolution code would otherwise silently overwrite
  `custom_metrics`, losing the user's definitions.

## PgpilotMetricLibrary

A namespace-scoped CR containing reusable metric definitions. It has its own controller that validates the library and sets a `Valid` condition. When a library changes, the PgpilotMonitor controller re-enqueues all monitors in the same namespace that reference it.

## ServiceMonitor detection

At startup, the operator checks (via discovery API) whether the `monitoring.coreos.com/v1` CRD exists. If yes, it creates a `ServiceMonitor` for each `PgpilotMonitor` that selects the pgwatch Service. If the CRD is absent, no ServiceMonitor is created and no error is reported.

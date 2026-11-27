# Architecture

## Overview

pgpilot-operator is a Kubernetes operator that manages [pgwatch v6](https://github.com/cybertec-postgresql/pgwatch) instances declaratively through CRDs. It does not reinvent metrics collection — pgwatch handles that. The operator handles the Kubernetes lifecycle.

## Core design decisions

1. **One PgpilotMonitor = one pgwatch pod.** Even though pgwatch can monitor many databases from a single process, we use one pod per database for namespace isolation, small blast radius, and a simple mental model.

2. **pgwatch runs in file-based (read-only) mode.** The operator generates `sources.yaml` and, for a monitor with metric definitions of its own, `metrics.yaml`, mounts them via a ConfigMap, and passes `--sources` (and then `--metrics`). pgwatch cannot mutate its own configuration.

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
  │     (inline or Secret — exactly one);   │
  │     inline: generate a Secret for them  │
  │  2. Resolve PgpilotMetricLibrary refs   │
  │  3. Merge metrics (preset + libs + custom) │
  │  4. Generate ConfigMap (sources.yaml,   │
  │     metrics.yaml if needed, SHA-256)    │
  │  5. Generate Deployment (1 replica)     │
  │  6. Generate Service (Prometheus sink)  │
  │  7. Generate ServiceMonitor (optional)  │
  │  8. Update status + conditions          │
  └─────────────────────────────────────────┘
       │
       ▼
  ┌───────────────┐  ┌────────────────┐  ┌──────────┐  ┌────────────────┐
  │   ConfigMap   │  │  Deployment    │  │ Service  │  │ ServiceMonitor │
  │ sources.yaml  │  │  1 replica     │  │ :9187    │  │ (if prom-op)   │
  │ (metrics.yaml)│  │  pgwatch pod   │  │          │  │                │
  └───────────────┘  └────────────────┘  └──────────┘  └────────────────┘
```

All child resources have `OwnerReferences` pointing to the `PgpilotMonitor`, so garbage collection is automatic on deletion. The Service and the ServiceMonitor exist only while the monitor's Prometheus sink is on.

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
    env: prod                       # spec.database.customTags
    namespace: <monitor-namespace>  # always, set by the operator
```

The name of the source is the monitor's name; pgwatch exports it as the
`dbname` label. The `namespace` tag tells two monitors of one name in two
namespaces apart, and is the operator's: a `namespace` key in
`spec.database.customTags` does not replace it.

`PGUSER` / `PGPASSWORD` always come from a Secret (`valueFrom.secretKeyRef`),
never from a value in the pod template:
- `spec.database.credentialsSecret`, the monitor's own, or
- `pgpilot-<monitor>-credentials`, which the operator generates from inline
  `spec.database.username` / `password` and removes again when the monitor
  no longer has them.

### metrics.yaml

Present only for a monitor with metric definitions of its own, inline or
from a library, and only then is the collector started with `--metrics`. A
monitor that just names a preset has no such file: its collector resolves the
preset from the registry inside its image, the definitions that belong to the
pgwatch release it runs.

pgwatch's `--metrics <file>` flag **replaces** that registry rather than
adding to it. For a monitor that combines a preset with metrics of its own,
the file therefore has to hold everything: the operator embeds pgwatch's own
`metrics.yaml` at build time via `go:embed`, hands every built-in metric and
preset on as it is written there, and puts the monitor's definitions on top
(a definition with the name of a built-in metric replaces it).

The embedded file is the unmodified registry of one pgwatch release, the same
release whose image a monitor runs by default
(`internal/builder/pgwatchdata/VERSION`; `make update-pgwatch-registry` fetches
another one). A monitor that combines a preset with metrics of its own and
pins another `spec.image.tag` still receives this registry: the two are meant to be used
together, which is why leaving the tag unset is the recommended setting. The
names of the presets are checked against this registry for every monitor.

In `sources.yaml` such a monitor lists its preset's metrics one by one, at the
preset's intervals, next to its own. A metric of its own that replaces one of
the preset keeps the preset's interval unless its definition names one.

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
  # All built-in presets, as the release ships them.
  exhaustive:
    description: ...
    metrics:
      db_stats: 60
      ...
```

## The collector pod

One Deployment with one replica runs pgwatch for a monitor.

- **Port and probes.** With the Prometheus sink the container has the port
  `metrics` (9187 unless `spec.sinks.prometheus.port` says otherwise) and TCP
  liveness and readiness probes of it. Without that sink nothing listens in
  the pod (pgwatch's web server is turned off, and the gRPC sink only
  connects out), so there is neither a port nor a probe. The kubelet restarts
  a collector that exits in either case; in place of a readiness probe the
  Deployment then has `minReadySeconds: 60`. pgwatch asks its gRPC receiver
  for a sign of life when it starts and exits without one: at once when the
  connection is refused or the name does not resolve, after 20 to 50 seconds
  when nothing answers at all (a NetworkPolicy or firewall that drops the
  packets). A minute outlasts that, so a collector that cannot deliver never
  counts as running, and never takes the place of one that can.
- **Changes.** A changed configuration, changed credentials or a new image
  replace the pod. The Deployment keeps Kubernetes' default strategy: the new
  pod is started first and the old one stopped when the new one is ready.
  For a moment two collectors query the database, and with the gRPC sink
  both send; with the gRPC sink alone that moment is the minute above. In return a change that cannot start (an image that cannot be
  pulled, a `ResourceQuota` without room for a second pod) leaves the
  collector that runs in place, and the monitor says why it is not ready
  (`ImagePullBackOff`, `PodCreateFailed`, `RolloutStuck`). In a namespace
  whose quota has room for exactly one collector pod, a change therefore
  waits until there is room for a second one.
- **Security.** The container runs as a non-root user with a read-only root
  filesystem, without capabilities and without privilege escalation, under
  the `RuntimeDefault` seccomp profile; the pod passes the `restricted` Pod
  Security Standard. No ServiceAccount token is mounted.
- **Scratch space.** `/tmp` is an `emptyDir`, the one writable place. The pod
  template names it in `cluster-autoscaler.kubernetes.io/safe-to-evict-local-volumes`,
  so that it does not keep the cluster autoscaler from removing a node.
- **Image.** `spec.image`, with the operator's pgwatch release for whatever
  is left unset; `spec.image.pullPolicy` is set on the container.
- **Resources.** `spec.resources` as it is, see
  [crd-reference.md](crd-reference.md#specresources).

## Config hash rollout

The ConfigMap content is hashed (SHA-256). The hash is set as the `pgpilot.io/config-hash` annotation on the Deployment's pod template. When the config changes, the hash changes, triggering a rolling update.

## Credentials and what the operator reads

A monitor's credentials are either inline or in a Secret of the same namespace.
For a Secret the operator only checks that it exists and has both keys, and
points the pod's `PGUSER` / `PGPASSWORD` at it; the values never pass through
the operator. Inline credentials it copies into a Secret of its own making,
`pgpilot-<monitor>-credentials`, owned by the monitor, so that the pod can
read them the same way and its template holds no password.

- That generated Secret is the only one the operator writes (`create`,
  `patch`, `delete`). The role cannot tie those verbs to a name: it allows
  them for every Secret of the cluster, and whoever got hold of the
  operator's ServiceAccount token could overwrite or delete any of them, or
  have a token made for any ServiceAccount. The operator's code is what
  keeps to the one Secret, by never writing to a Secret that the monitor
  does not control. One that already carries the name is left alone and
  reported as `NameConflict`.
- The generated Secret is removed when the monitor no longer has inline
  credentials and no pod reads it any more, that is, once the pod of the new
  template is available. It cannot be named as the monitor's own
  `credentialsSecret` (`GeneratedSecretReferenced`).
- Otherwise the operator may `get` a Secret by name and nothing else: no list, no watch,
  and Secrets are never cached.
- Because nothing watches Secrets, the one a monitor references is read again
  periodically (five minutes by default). A token derived from the Secret
  object's identity and version - not from its values - is set as the
  `pgpilot.io/credentials-version` annotation on the pod template, so a changed
  Secret replaces the pod just like a changed config does.
- ConfigMaps, Deployments, Services, Pods and ServiceMonitors are cached only
  when they carry `app.kubernetes.io/managed-by: pgpilot-operator`, which every
  generated object does. The operator's memory use therefore follows the number
  of monitors, not the size of the cluster.

## Metric merge order

When a PgpilotMonitor references a preset, libraries, and inline custom metrics:

1. **Libraries** — resolved in the order listed in `spec.metrics.fromLibraries`
2. **Inline custom** — from `spec.metrics.custom` (later wins on name collision)

The selected **preset** is then combined with the above depending on what's
present:

- **Preset only** (no custom): `preset_metrics: <name>` is set in
  sources.yaml; pgwatch resolves it from the registry inside its image
- **Custom only** (no preset): `custom_metrics: {...}` is set
- **Both preset + custom**: the operator expands the preset's metric list
  into `custom_metrics` (on a name collision an interval the definition
  names overrides the preset's; a definition without one keeps the
  preset's) and **does not** set `preset_metrics`. pgwatch's
  preset-resolution code would otherwise silently overwrite
  `custom_metrics`, losing the user's definitions.

## PgpilotMetricLibrary

A namespace-scoped CR containing reusable metric definitions. It has its own controller that validates the library and sets a `Valid` condition. When a library changes, the PgpilotMonitor controller re-enqueues all monitors in the same namespace that reference it.

## ServiceMonitor detection

At startup, the operator checks (via discovery API) whether the `monitoring.coreos.com/v1` CRD exists, and logs the answer. If yes, it creates a `ServiceMonitor` for each `PgpilotMonitor` whose Prometheus sink is on, selecting the pgwatch Service. If the CRD is absent, no ServiceMonitor is created and no error is reported. If the question cannot be answered, the operator exits and is restarted.

Should the CRD be removed while the operator runs, monitors keep being reconciled; each reports it in its `ServiceMonitorApplied` condition.

## Deletion

A PgpilotMonitor carries no finalizer. Everything generated for it lives in its namespace and is owned by it, so the Kubernetes garbage collector removes it when the monitor is deleted, whether the operator is running or not. Releases before 1.1 put `pgpilot.io/finalizer` on every monitor; the operator takes it off again when it starts. The one exception is a monitor from such a release that breaks a validation rule added since, on Kubernetes 1.28 and 1.29: the API server refuses every write to it, this one included, until its spec is corrected.

## Names

The generated objects are called `pgpilot-<monitor>` and `pgpilot-<monitor>-config`. Before the operator writes one it looks whether the name is taken: in its cache, and at the API server for what the cache does not hold. An object that is there and not controlled by the monitor is not written to and not deleted; the monitor reports `NameConflict` instead. Looking and writing are two requests, so this protects what exists, not an object created under the name in the instant between them.

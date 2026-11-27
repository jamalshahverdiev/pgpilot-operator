# Troubleshooting

## Monitor stuck in Ready=False

**Check conditions:**

```bash
kubectl describe pgpilotmonitor <name> -n <namespace>
```

`Ready` is true when the pgwatch pod **of the current spec** is running and
available. Otherwise the reason of the `Ready` condition says what is in the
way, and the message carries the detail (pod name, image, the API server's
error):

| Reason | Meaning |
|--------|---------|
| `PodRunning` | `Ready` is true: the pod of the current spec is running and available |
| `RolloutInProgress` | The Deployment is being rolled out. If the monitor was healthy before, the pod of the previous spec keeps collecting until the new one is up |
| `PodNotReady` | The pod of the current spec exists and has not become ready yet. If its container exited once and is being restarted (a node that restarted, one out-of-memory kill), the message says so, with the exit code |
| `ImagePullBackOff` | The pod cannot pull its image: wrong `spec.image`, a registry that needs credentials, or no network path to it |
| `CrashLoopBackOff` | pgwatch keeps starting and exiting. See its last words with `kubectl logs -l pgpilot.io/monitor=<name> --previous` |
| `CreateContainerConfigError`, `CreateContainerError`, `RunContainerError` | The kubelet cannot start the container; the message says why |
| `Unschedulable` | No node can take the pod: requests too large, taints, node selectors |
| `PodCreateFailed` | No pod could be created at all: a ResourceQuota, a LimitRange, Pod Security admission. The message is the API server's |
| `RolloutStuck` | The rollout made no progress for ten minutes and nothing more specific is known. Look at `kubectl describe deploy,rs,pod -l pgpilot.io/monitor=<name>` |
| `SecretNotFound`, `SecretKeyMissing`, `SecretReadFailed`, `NoCredentials` | Credentials, see below |
| `LibraryResolveFailed` | A referenced metric library is missing, see below |
| `InvalidMetric` | A metric definition, of a library or of the monitor itself, was stored before a validation rule existed and breaks it, see below |
| `InvalidPreset` | See [Monitor reports InvalidPreset](#monitor-reports-invalidpreset) |
| `InvalidName` | The monitor's name does not fit the generated objects, see below |
| `NameConflict` | An object with the name of a generated one exists and is not this monitor's, see below |
| `ApplyRejected` | The API server, or an admission policy, refused a generated object as invalid, see below |
| `ApplyFailed`, `ConfigBuildFailed` | The operator could not write or render something; it retries. If it persists, the message and the operator's log say why |

`status.ready` and the READY column of `kubectl get pm` always say the same as
this condition. While a change cannot be rolled out the monitor is not ready
although the previous pod still runs and collects: what you asked for is not
what is running. `status.podName` names the pod that runs.

How soon the operator looks again at a monitor that is not ready depends on
what it is waiting for:

- a collector that is being rolled out or starting (`RolloutInProgress`,
  `PodNotReady`): every 15 seconds, so a reason that only shows on the pod
  can take that long to appear;
- a collector that is held up (`ImagePullBackOff`, `CrashLoopBackOff`,
  `Unschedulable`, ...): every minute, to see whether the reason is still the
  same. The end of the problem does not wait for that: a pod that becomes
  ready changes its Deployment, which the operator watches;
- a missing Secret, Secret key or metric library, and `NameConflict`: every
  30 seconds;
- `ApplyRejected`: every five minutes;
- `InvalidName` and `InvalidPreset`: not until the monitor is changed;
- `InvalidMetric`: not until the monitor, or the library that is named, is
  changed;
- `SecretReadFailed`, `ApplyFailed`, `ConfigBuildFailed`: retried with
  growing pauses, from milliseconds up to about a quarter of an hour.

A change of the monitor's spec, labels or annotations is looked at at once in
every case.

### SecretNotFound

The credentials Secret referenced by `spec.database.credentialsSecret.name` does not exist in the monitor's namespace.

```bash
kubectl get secret <secret-name> -n <namespace>
```

Fix: create the Secret or correct the name.

### NoCredentials

Neither inline `username`/`password` nor `credentialsSecret` is set on
`spec.database`. This should normally be blocked by CRD validation, but
shows up as a `Ready=False` condition if it slips through (e.g. older
CRD without CEL rules).

Fix: set exactly one of the two credential sources per
[CRD Reference](crd-reference.md#specdatabase).

### LibraryResolveFailed

A `PgpilotMetricLibrary` referenced in `spec.metrics.fromLibraries` does not exist.

```bash
kubectl get pml -n <namespace>
```

Fix: create the library or remove the reference.

### InvalidMetric

One of the metric definitions the monitor is made of breaks a rule:

- a `sqls` key that is not a PostgreSQL major version written as a plain
  integer (`"*"`, `"14.2"`, `"v14"`, `"170000"`),
- an empty SQL statement, or no statement at all,
- `masterOnly` together with `standbyOnly`,
- an `interval` below one second,
- a name that is not a lower-case identifier, or that is used twice in one
  list.

The API server refuses such a definition when it is written, so this is about
objects stored by an operator release up to v1.0.2, before the rules existed.
Those releases made what they could of them without a word: a key like `"*"`
was dropped, and a metric left without any statement was handed to the
collector with none. Others were collected as they were written (`"014"` was
read as 14, the last of two metrics of one name was used); they are refused
all the same and have to be corrected. The message names where the
definition is (the `PgpilotMetricLibrary`, or `spec.metrics.custom` of the
monitor), the metric and what is wrong with it. A name or a key longer than
63 characters is shown up to there, with its length.

The operator changes nothing for the monitor until the definition is
corrected. A collector that runs keeps running with the configuration it has;
a rotated Secret, a change of the spec and a new pgwatch release do not reach
it. Correcting the definition is accepted by every Kubernetes version, also by
those that refuse other changes to an object that breaks a rule:

```bash
kubectl edit pgpilotmetriclibrary <library> -n <namespace>
```

The monitor is reconciled as soon as the library, or its own spec, changes.

A library says the same about itself in its `Valid` condition
(`kubectl get pml <library> -o jsonpath='{.status.conditions}'`).

On Kubernetes before 1.33 the API server does not let the operator write the
status of an object that itself breaks a rule, and the operator logs
`update status ... is invalid` for it:

- A library that breaks a rule keeps the status it had, `Valid=True` if an
  earlier release wrote it. The monitors that use it are where to look: they
  report `InvalidMetric` and name the library.
- A monitor whose own `spec.metrics.custom` breaks a rule keeps the status it
  had as well, and `READY` can go on saying `true` although nothing is done
  for it. What is wrong is said in a `Warning` event with reason
  `InvalidMetric` on the monitor, recorded again at every attempt:

  ```bash
  kubectl describe pgpilotmonitor <name> -n <namespace>
  kubectl get events -A --field-selector reason=InvalidMetric
  ```

### PodNotReady

The pgwatch pod of the current spec exists but has not passed its readiness
probe. Usually it was created only a moment ago; the probe starts after a few
seconds. If it stays, look at the pod:

```bash
kubectl describe pod -n <namespace> -l pgpilot.io/monitor=<name>
kubectl logs -n <namespace> -l pgpilot.io/monitor=<name>
```

A database that cannot be reached, a wrong password or a wrong `sslmode` do
**not** make the pod unready: pgwatch keeps running and retries. See
[Monitor is Ready but there are no metrics](#monitor-is-ready-but-there-are-no-metrics).

### InvalidName

The objects generated for a monitor are named after it: the Service is
`pgpilot-<name>`, and a Service name may have at most 63 characters and no
dots. A monitor called `db.prod`, or with a name longer than 55 characters,
cannot get one. Nothing is generated for it.

A name cannot be changed. Create the monitor again under a name of at most 55
characters made of lower-case letters, digits and `-`, and delete this one. If
it was created by an older operator release it may still have a ConfigMap and
a Deployment (without a Service); they are removed with it.

Until then such a leftover collector keeps running as it is: the operator no
longer applies anything for the monitor, so a rotated Secret, a change of the
spec and a new pgwatch version do not reach it.

### NameConflict

Everything the operator generates for a monitor is named after it:
`pgpilot-<name>` (Deployment, Service, ServiceMonitor) and
`pgpilot-<name>-config` (ConfigMap). When an object of that kind and name is
already in the namespace and was not generated for this monitor, the operator
leaves it exactly as it is and reports the conflict; the message names the
object and who owns it. It does not write to it, and it would not delete it
with the monitor.

Delete the object that is in the way (or recreate it under another name), or
create the monitor under another name. The operator looks again every 30
seconds.

The message `it is owned by PgpilotMonitor <name>, an earlier object of that
name that is being removed` appears for a moment when a monitor is deleted and
created again at once: the objects of the first one are still being garbage
collected. It clears by itself.

`it has no owner`, about objects that carry the operator's labels, means
leftovers of an earlier monitor of that name: one deleted with
`kubectl delete --cascade=orphan`, or objects restored from a backup without
their owner references. The operator does not take them back. Until they are
gone the old collector keeps running as it was, and no change of the monitor
or of its Secret reaches it. Delete them and the operator creates its own:

```bash
kubectl delete deployment,service,configmap -n <namespace> -l pgpilot.io/monitor=<name>
kubectl delete servicemonitor -n <namespace> -l pgpilot.io/monitor=<name>   # with the prometheus-operator
```

### ApplyRejected

One of the generated objects was refused as invalid; the message names the
object and repeats the reason given. There are two kinds of cause:

- a value the PgpilotMonitor accepts and Kubernetes does not, for example
  `spec.resources` with a request above its limit. Correct the spec; the
  operator applies the new one at once;
- something in the cluster that refuses the object although the spec is fine:
  a ValidatingAdmissionPolicy (the message names it), an admission webhook
  that answers "invalid", or an installed ServiceMonitor CRD older than what
  the operator sends. Nothing on the monitor has to change for these.

The operator sends the object again every five minutes, so a monitor recovers
on its own once the cause is gone. To have it try at once, touch the monitor:

```bash
kubectl annotate pgpilotmonitor <name> -n <namespace> retry="$(date +%s)" --overwrite
```

While a monitor is `ApplyRejected` the collector it already has keeps running
as it was; a rotated Secret is not rolled out to it.

### SecretKeyMissing

The Secret exists, but the key named in the message is absent (or, for the
username, empty). The keys are `username` and `password` unless the monitor
sets `usernameKey` / `passwordKey`:

```bash
kubectl get secret <secret-name> -n <namespace> -o jsonpath='{.data}'
```

An empty password is accepted, for databases that trust the monitoring role;
the key has to exist.

### SecretReadFailed

The operator could not ask the API server for the Secret (the message carries
the error). It retries on its own. If it persists, check that the operator's
ClusterRole still grants `get` on Secrets and that the operator image and its
role come from the same release.

## Monitor is Ready but there are no metrics

`Ready: True` means the pgwatch pod runs and listens. It says nothing about the
database: when pgwatch cannot connect or log in, it logs the failure, stays up
and retries every two minutes. Look at:

```bash
kubectl logs -n <namespace> -l pgpilot.io/monitor=<name>
```

and at the `pgwatch_instance_up` metric (`0` while the database cannot be
reached). Usual causes: wrong host or port, a wrong password, a `sslmode` the
server does not support, a network policy or firewall in between.

The first line pgwatch logs for a source tells which case it is:

- `Connect OK. Version: PostgreSQL ...` - connected and logged in.
- `could not init connection, retrying on next iteration` followed by the
  server's error (`password authentication failed`, `no such host`, `connection
  refused`, ...) - it cannot connect; fix what the error names. When the
  password comes from a Secret, the operator replaces the pod once the Secret
  changes.

## Some metrics are missing, late or arrive in bursts

With pgwatch 6 all metrics of a monitor are fetched by one worker, the ones due
at the same time in one batch. A metric whose query fails or runs too long is
taken out of the batch and tried on its own. In the pod log:

- `failed to fetch metric` / `degraded metric fetch failed` with the server's
  error - the query itself fails. The usual reasons are a missing extension
  (`relation "pg_stat_statements" does not exist`), a missing helper function
  (`cpu_load` and the `psutil_*` metrics in presets `standard`, `exhaustive`
  and `full`; use `exhaustive_no_python` or install pgwatch's helpers), or a
  monitoring role without `pg_monitor`. pgwatch 6 logs this on every attempt,
  not once in a while as pgwatch 5 did.
- `metric degraded after repeated failures, switching to individual fetch` -
  the metric is from now on fetched alone.
- `timeout: context deadline exceeded` - the queries ran longer than pgwatch
  allows. The metrics due at the same moment share one limit: 30 seconds, or
  the greatest common divisor of the monitor's intervals if that is longer.
  What has not finished by then is cancelled on the server (`canceling
  statement due to user request` in the PostgreSQL log) and tried again alone,
  this time with the metric's own interval (at least 30 seconds) as limit. A
  query slower than the first limit thus runs twice at every collection; one
  slower than the second never yields data.

While a slow query runs, every other metric of the monitor waits, including
`pgwatch_instance_up`. A slow custom metric can thus make all series of its
monitor arrive late. Set `statement_timeout` (and `lock_timeout`) on the
monitoring role, make the query cheaper, or move it into a PgpilotMonitor of
its own; see [query time](crd-reference.md#query-time).

A metric that is simply not defined for the server's PostgreSQL version is
skipped and writes no line of its own. With pgwatch 6.2.0 its name can still
show up in the `metric` field of a `failed to fetch metric` line that reports
another metric's error; the metric that really failed is the one named in the
error text (`fetch <metric>: ...`).

## Monitor reports InvalidPreset

`Ready: False` and `ConfigGenerated: False` with reason `InvalidPreset` mean
that `spec.metrics.preset` is not a preset the collector can gather from a
PostgreSQL database: a misspelt name, or one of pgwatch's presets for other
kinds of sources (`patroni`, `pgbouncer`, `pgpool`). The condition message and
the warning event list the names that can be used. (A name with characters
other than lower-case letters, digits and `_`, such as `exhaustive-no-python`,
does not get this far: the API server refuses it when the object is written.)

Nothing is generated for such a monitor. If it had a collector before the
change, that collector keeps running with its last valid configuration until
the preset is corrected, and the operator does not look at the monitor again
until then: a rotated credentials Secret, for instance, does not reach that
collector.

## Collector still runs an old pgwatch after an operator upgrade

```bash
kubectl get deployments -A -l app.kubernetes.io/managed-by=pgpilot-operator \
  -o custom-columns='NAMESPACE:.metadata.namespace,NAME:.metadata.name,IMAGE:.spec.template.spec.containers[0].image'
kubectl get pgpilotmonitors -A \
  -o custom-columns='NAMESPACE:.metadata.namespace,NAME:.metadata.name,REPOSITORY:.spec.image.repository,TAG:.spec.image.tag'
```

First look at the monitor's `Ready` condition and its events. A monitor that
is not ready for a reason of its own (`InvalidMetric`, which shows up with
this very upgrade, `InvalidPreset`, `InvalidName`, a missing Secret or
library) is not changed at all until that is corrected, whatever its tag; see
the table at the top.

A monitor follows the operator's pgwatch release only while `spec.image.tag` is
unset. Operator releases up to v1.0.2 wrote `tag: "5.1.0"` into every monitor
they reconciled; the current operator removes that tag when it starts (an
`ImageDefaultRestored` event is recorded on the monitor), but only where the
API server's record of who wrote the field (`managedFields`) shows no writer
other than the old operator. It leaves the tag alone when

- another writer owns it: the tag was set when the monitor was created, or
  set to another value later, or applied with server-side apply,
- the manifest last applied with `kubectl apply` contains a tag,
- the monitor is annotated `pgpilot.io/keep-image-tag: "true"`,
- the monitor uses another `spec.image.repository` (a mirror may not have the
  newer release),
- the object has no ownership record (restored from a backup, for example),
- the operator runs with `--migrate-stored-image-tag=false`
  (`migrations.storedImageTag: false` in the Helm chart).

A monitor whose tag is kept for one of these reasons may carry the annotation
`pgpilot.io/operator-wrote-image-tag: "5.1.0"`. The operator writes it when it
takes its old finalizer off the monitor: the finalizer was half of what showed
that the tag is an earlier release's and not yours, and the annotation records
that. With it, the tag is still removed at the next start after you turn the
option back on or delete the keep annotation. The operator removes the
annotation when it removes the tag. If you change or remove the tag yourself,
the annotation stays and means nothing any more (it only counts while the tag
is still `5.1.0`); delete it if you like:

```bash
kubectl annotate pgpilotmonitor <name> -n <namespace> pgpilot.io/operator-wrote-image-tag-
```

At start the operator logs which monitors with tag 5.1.0 it left alone, and
why (`monitors with image tag 5.1.0 were left as they are`). Two messages in
the same place point at something to fix first:

- `the installed PgpilotMonitor CRD still defaults spec.image.tag` - the CRDs
  are older than the operator; the old CRD puts the tag back as soon as it is
  removed. Upgrade the CRDs; the operator tries again at its next start.
- `... is invalid` for a monitor - on Kubernetes 1.28 and 1.29 an object that
  breaks a validation rule added since it was stored cannot be changed until
  its spec is corrected.

The same holds for the finalizer of earlier releases, which the operator takes
off every monitor when it starts: `could not take the finalizer off every
monitor` in the operator's log, with `... is invalid` for a monitor, names one
that keeps `pgpilot.io/finalizer` until its spec is corrected. Such a monitor
is deleted as before this release, by the running operator, and on Kubernetes
1.28 and 1.29 not at all while it is invalid; see the table under
"Upgrade notes" in the changelog. To see which monitors still carry a
finalizer:

```bash
kubectl get pgpilotmonitors -A \
  -o jsonpath='{range .items[?(@.metadata.finalizers)]}{.metadata.namespace}/{.metadata.name}{"\n"}{end}'
```

To move a monitor yourself, remove the tag:

```bash
kubectl patch pgpilotmonitor <name> -n <namespace> --type=json \
  -p '[{"op":"remove","path":"/spec/image/tag"}]'
```

If the tag comes from a manifest in Git or a Helm values file, remove it there.

A rollout that does not finish after the upgrade (`ImagePullBackOff` on the new
pod while the old one keeps running) means the cluster cannot pull the new
image; mirror `cybertecpostgresql/pgwatch:6.2.0` to where the old one came
from.

## ConfigMap not updating after spec change

First look at the monitor's `Ready` condition: for a monitor that is not ready
for a reason of its own (`InvalidMetric`, `InvalidPreset`, `InvalidName`, a
missing Secret or library) nothing is generated until that is corrected, and
the ConfigMap stays as it was; see the table at the top.

The operator uses a content-hash annotation (`pgpilot.io/config-hash`) on the pod template to trigger rollouts. If the hash does not change, no rollout occurs.

Check:

```bash
kubectl get configmap pgpilot-<name>-config -n <namespace> -o yaml
```

If the ConfigMap content matches your spec, the hash is correct and no rollout is needed.

## ServiceMonitor not created

The operator checks for the `monitoring.coreos.com/v1` API once, at
**startup**, and says what it found in its log (`ServiceMonitor CRD detected`
or `ServiceMonitor CRD not installed`). If the prometheus-operator was
installed after the pgpilot-operator, restart the operator:

```bash
kubectl rollout restart deployment -n pgpilot-system <operator-deployment>
```

If the API server cannot answer that question when the operator starts, the
operator exits and is restarted, rather than run without ServiceMonitors
until someone notices.

A monitor has a Service and a ServiceMonitor only while its Prometheus sink is
on (`spec.sinks.prometheus.enabled`, the default). With a gRPC sink alone
nothing listens on the metrics port, so the operator creates neither and
removes the ones an earlier spec had.

Monitors that are to have a ServiceMonitor say so in a condition of their own:

```bash
kubectl get pgpilotmonitor <name> -n <namespace> \
  -o jsonpath='{.status.conditions[?(@.type=="ServiceMonitorApplied")]}'
```

`False` with reason `ServiceMonitorAPIMissing` means the cluster stopped
serving ServiceMonitors while the operator was running (the
prometheus-operator or its CRDs were removed). The collector is not affected
and `Ready` does not change. The operator tries again at least every five
minutes and at every change of the monitor, so the ServiceMonitor comes back
once the API does; if it was removed for good, restart the operator and the
condition goes away.

When the Prometheus sink of a monitor is turned off, the operator deletes the
Service and the ServiceMonitor it had generated and records a
`ScrapeTargetRemoved` event for each on the monitor.

## pgwatch pod CrashLoopBackOff

Check logs:

```bash
kubectl logs -n <namespace> -l pgpilot.io/monitor=<name> --previous
```

pgwatch exits when its own configuration is unusable, for example when it has
no sink, or a gRPC sink it cannot reach at startup. Connection and login
failures do not make it exit (see
[Monitor is Ready but there are no metrics](#monitor-is-ready-but-there-are-no-metrics)).

A related problem that shows in the log but does not crash the pod:
- **Permission denied** — pgwatch user lacks `pg_monitor` role. Grant it:

```sql
GRANT pg_monitor TO pgwatch;
```

## Operator pod not starting

```bash
kubectl logs -n pgpilot-system -l control-plane=controller-manager
```

Common issues:
- RBAC misconfigured (check ClusterRole and ClusterRoleBinding)
- Leader election conflict (another instance holds the lease)
- CRDs not installed (`installCRDs: false` in Helm values but no manual CRD install —
  apply them with
  `kubectl apply --server-side --force-conflicts -f charts/pgpilot-operator/files/crds/`)

## Events

The operator records Kubernetes events on PgpilotMonitor resources:

```bash
kubectl get events -n <namespace> --field-selector involvedObject.name=<monitor-name>
```

An event is recorded when the `Ready` condition changes its status or reason,
not each time the operator looks: a monitor that waits ten minutes for its
Secret has one `SecretNotFound` event, not twenty. A problem with the same
reason is announced again only when the spec was changed and the message is
another one (a second preset that does not exist, another Secret that is
missing).

- `Normal/ConfigMapUpdated` — the generated ConfigMap changed (i.e. a rollout
  is about to happen)
- `Normal/PodRunning` — the pod of the current spec is running and available
- `Normal/RolloutInProgress` — a monitor that was ready is rolling out a new
  pod: a changed spec, a rotated Secret, an operator upgrade that changes the
  pod
- `Normal/ImageDefaultRestored` — see
  [Collector still runs an old pgwatch](#collector-still-runs-an-old-pgwatch-after-an-operator-upgrade)
- `Warning/PodNotReady` — the pod of a monitor that was ready is no longer
  available
- `Normal/ScrapeTargetRemoved` — the Service or the ServiceMonitor of a
  monitor was deleted because its Prometheus sink is off
- `Warning/ServiceMonitorAPIMissing` — see
  [ServiceMonitor not created](#servicemonitor-not-created); recorded once
  when the API goes missing
- `Warning/<reason>` — for every other reason in the table at the top of this
  page (`ImagePullBackOff`, `CrashLoopBackOff`, `PodCreateFailed`,
  `SecretNotFound`, `InvalidPreset`, ...), with the condition's message

A new monitor on its way up (`RolloutInProgress`, `PodNotReady`) records
nothing until it is ready or something is wrong. Check `status.conditions` for
the current state; events expire.

Condition reasons on `CredentialsAvailable`:

- `InlineCredentials` — spec uses inline `username`/`password`
- `CredentialsFound` — the `credentialsSecret` exists and has a value under both keys
- `SecretNotFound` — the referenced Secret is missing
- `SecretKeyMissing` — the Secret exists but has no value under the key named in the
  message (`username` / `password`, or the `usernameKey` / `passwordKey` you set)
- `SecretReadFailed` — the API server could not be asked for the Secret; the operator
  retries on its own
- `NoCredentials` — neither inline credentials nor a `credentialsSecret` are set

`CredentialsAvailable: True` means the pod can start with the configured
credentials. It does not mean the database accepts them: pgwatch keeps running
and retrying when a login fails. Check `pgwatch_instance_up` and the pod log
for that. (Up to v1.0.2 this condition was called `DatabaseReachable`.)

## Rotating the database password

Update the Secret. The operator does not watch Secrets; it reads the one a
monitor references again every five minutes and, when the Secret has changed,
replaces the collector pod so that pgwatch starts with the new values. Until
then pgwatch keeps using the old password.

To apply a rotation at once, touch the monitor; any change to its annotations
makes the operator look at the Secret immediately and replace the pod once:

```bash
kubectl annotate pgpilotmonitor <monitor-name> -n <namespace> \
  pgpilot.example/rotated-at="$(date +%s)" --overwrite
```

(`kubectl rollout restart` on the Deployment also picks up the new password,
but the operator then replaces the pod a second time at its next recheck.)

The interval is the operator flag `--credentials-recheck-interval` (at least
`10s`); with the Helm chart set `credentials.recheckInterval`, e.g. `1m`.

Any change to the Secret object replaces the pod, including a change to its
labels or annotations only. Tools that rewrite a Secret on every sync without
changing its content therefore restart the collector on every sync.

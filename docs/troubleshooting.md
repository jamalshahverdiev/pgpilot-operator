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
| `PodCreateFailed` | No pod could be created at all: a ResourceQuota, a LimitRange, Pod Security admission, a ServiceAccount or PriorityClass that does not exist, a security context constraint on OpenShift (see [OpenShift](#openshift)). The message is the API server's |
| `RolloutStuck` | The rollout made no progress for ten minutes and nothing more specific is known. Look at `kubectl describe deploy,rs,pod -l pgpilot.io/monitor=<name>` |
| `SecretNotFound`, `SecretKeyMissing`, `SecretReadFailed`, `NoCredentials` | Credentials or certificates, see below. The message names the Secret; for a certificate also the field of the monitor that names it |
| `InvalidCertificate` | A Secret named under `spec.database.tls` or `spec.sinks.grpc.tls` has its keys, and they do not hold what the collector can load, see below |
| `GeneratedSecretReferenced` | `spec.database.credentialsSecret`, or a field that names a Secret with a certificate, names `pgpilot-<name>-credentials`, the Secret the operator generates from inline credentials and removes once a monitor has none. Create a Secret of your own under another name, with the same `username` and `password`, and name that; or keep the inline credentials. Nothing is changed for the monitor until then |
| `LibraryResolveFailed` | A referenced metric library is missing, see below |
| `InvalidMetric` | A metric definition, of a library or of the monitor itself, was stored before a validation rule existed and breaks it, see below |
| `InvalidPreset` | See [Monitor reports InvalidPreset](#monitor-reports-invalidpreset) |
| `InvalidName` | The monitor's name does not fit the generated objects, see below |
| `NoSinkEnabled` | Neither sink is in use: `spec.sinks.prometheus.enabled` is `false`, and `spec.sinks.grpc` is not enabled or has no `endpoint`. pgwatch exits at once without a sink, so no collector is generated. The API server refuses such a monitor today; this is one stored before it did, and the Deployment, Service and ServiceMonitor that release made for it are left as they are, the pod restarting. Enable a sink |
| `NameConflict` | An object with the name of a generated one exists and is not this monitor's, see below |
| `ApplyRejected` | The API server, or an admission policy, refused a generated object as invalid, see below |
| `ApplyFailed`, `ConfigBuildFailed` | The operator could not write or render something; it retries. `exceeded quota` on a ConfigMap means the namespace's ResourceQuota has no room for a second one, which every change of the configuration needs until its rollout is complete. If it persists, the message and the operator's log say why |

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
- a missing Secret, Secret key or metric library, `InvalidCertificate` and
  `NameConflict`: every 30 seconds;
- `ApplyRejected`: every five minutes;
- `InvalidName`, `InvalidPreset`, `NoSinkEnabled`,
  `GeneratedSecretReferenced` and a `SecretNotFound` for a name that no
  Secret can have: not until the monitor is changed;
- `InvalidMetric`: not until the monitor, or the library that is named, is
  changed;
- `SecretReadFailed`, `ApplyFailed`, `ConfigBuildFailed`: retried with
  growing pauses, from milliseconds up to about a quarter of an hour.

A change of the monitor's spec, labels or annotations is looked at at once in
every case.

### SecretNotFound

A Secret the monitor names does not exist in the monitor's namespace: the
credentials Secret (`spec.database.credentialsSecret.name`), or one that holds
a certificate (`spec.database.tls.caSecretRef`,
`spec.database.tls.clientCertSecretRef`, `spec.sinks.grpc.tls.caSecretRef`).
For a certificate the message says which field names it.

```bash
kubectl get secret <secret-name> -n <namespace>
```

Fix: create the Secret or correct the name. A Secret that cert-manager is
still issuing appears on its own; the operator looks again every 30 seconds.

`cannot exist: that is not the name of a Secret` is a value in
`spec.sinks.grpc.tls.caSecretRef` that no Secret can be called, such as
`monitoring/receiver-ca`. The API server takes whatever is written there,
because the field was stored before it had a meaning. The Secret has to be
in the monitor's namespace and is named without one. The operator does not
look again until the monitor is changed.

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
seconds. A collector without the Prometheus sink has no port to probe: its pod
counts as available once it has run for a minute, and the message says so
while it waits. If it stays, look at the pod:

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
`pgpilot-<name>` (Deployment, Service, ServiceMonitor),
`pgpilot-<name>-config-<hash>` (ConfigMap, one for each configuration) and, for a monitor with inline
credentials, `pgpilot-<name>-credentials` (Secret). When an object of that kind and name is
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
kubectl delete deployment,service,configmap,secret -n <namespace> -l pgpilot.io/monitor=<name>
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

For a certificate the keys are `ca.crt` for an authority, and `tls.crt` and
`tls.key` for a client certificate, unless the monitor names others (`key`,
`certKey`, `keyKey`). The Secret of the gRPC sink's authority always uses
`ca.crt`. A key with an empty value counts as absent.

### InvalidCertificate

The Secret and its keys are there, and what they hold is not what the
collector can load. The operator checks it the way pgwatch would, before a
pod is made: a pod with such a Secret starts, and then cannot connect to the
database, or exits over its gRPC sink, and only its log says why. The message
names the Secret, the field and the key, and never repeats what the Secret
holds.

- `holds no PEM certificate`: the key of an authority, or of a client
  certificate, has no `-----BEGIN CERTIFICATE-----` block in it; for an
  authority also when no block of it can be read as a certificate. Usual
  reasons: the certificate was base64-encoded once more before it was put
  into the Secret, it is in DER form, or the key holds a private key.
- `holds a PEM certificate that cannot be read`: the block is there and what
  is in it is damaged or cut short (`openssl x509 -noout` on it says how).
- `holds no PEM block`: the same for the private key of a client
  certificate: DER, or encoded once more.
- `do not hold a PEM certificate and, as the first PEM block, its private
  key`: the private key is not the first block of its value, or cannot be
  read as a key. pgwatch reads the first PEM block as the key and nothing
  after it: a key written by `openssl ecparam -genkey` comes after a block of
  curve parameters and is not found (`openssl pkcs8 -topk8 -nocrypt -in
  tls.key` writes it alone). With `the key is not the one of the
  certificate` at the end, both were read and do not belong together.
- `holds a private key under a passphrase`: nothing hands the collector a
  passphrase. Store the key without one.

```bash
# the certificate: whose it is and until when
kubectl get secret <secret-name> -n <namespace> -o jsonpath='{.data.ca\.crt}' \
  | base64 -d | openssl x509 -noout -subject -enddate
# the private key: only its first line, which has to be -----BEGIN ... PRIVATE KEY-----
kubectl get secret <secret-name> -n <namespace> -o jsonpath='{.data.tls\.key}' \
  | base64 -d | head -1
```

A monitor that was running keeps its collector, with the certificate it
started with, while this is reported. The operator does not look at dates: a
certificate that has expired is loaded, and refused by the other side, see
[Monitor is Ready but there are no metrics](#monitor-is-ready-but-there-are-no-metrics).

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
server does not support, a certificate the collector does not accept, a
network policy or firewall in between.

The first line pgwatch logs for a source tells which case it is:

- `Connect OK. Version: PostgreSQL ...` - connected and logged in.
- `could not init connection, retrying on next iteration` followed by the
  server's error (`password authentication failed`, `no such host`, `connection
  refused`, ...) - it cannot connect; fix what the error names. When the
  password comes from a Secret, the operator replaces the pod once the Secret
  changes.
- `x509: certificate signed by unknown authority` - with `sslmode:
  verify-ca` or `verify-full` the server's certificate is checked against
  the public authorities of the pgwatch image unless the monitor names its
  own in `spec.database.tls.caSecretRef`; with one named, also under
  `sslmode: require`, it is not the one that signed the server's
  certificate.
- `x509: certificate is valid for ..., not ...` - `verify-full` also compares
  the name in the certificate with `spec.database.host`. Use the name the
  certificate was issued for, or `verify-ca`.
- `x509: certificate has expired or is not yet valid` - the server's
  certificate, or the authority in the Secret.
- `tls: certificate required` or `connection requires a valid client
  certificate` - the server asks for a client certificate
  (`spec.database.tls.clientCertSecretRef`); `certificate authentication
  failed for user` - it got one whose common name is not the database user.

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

Each configuration has a ConfigMap of its own, `pgpilot-<name>-config-<hash>`,
named after the hash of its content; `status.configHash` and the pod
annotation `pgpilot.io/config-hash` carry the same hash. A change of the spec
that changes the configuration therefore shows as a new ConfigMap and a
rollout; one that does not (a label, a resource limit) leaves the ConfigMap
alone.

```bash
kubectl get configmap -n <namespace> -l pgpilot.io/monitor=<name>
kubectl get pgpilotmonitor <name> -n <namespace> -o jsonpath='{.status.configHash}'
```

The current one is the one the collector mounts; its name ends in the first
ten characters of `status.configHash`:

```bash
kubectl get deployment pgpilot-<name> -n <namespace> \
  -o jsonpath='{.spec.template.spec.volumes[*].configMap.name}'
```

More than one ConfigMap for a monitor means that the others have not been
cleared away yet. During a rollout the other one is what the pod of the
previous configuration reads. The operator removes them at its first look
that finds the monitor `Ready` (`ConfigMapRemoved` event), so they also stay
while the monitor is not ready for a reason of its own, a Service that
cannot be written for example, although the pods have long been replaced;
the `Ready` condition names the reason.

The generated ConfigMaps are immutable: the API server refuses a change to
their content, because the name stands for it. One that is deleted by hand
comes back at once if it is the current one. The ConfigMap of an earlier
configuration does not: the pod that still reads it keeps running on what it
has, and cannot be started again until the rollout is complete.

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

pgwatch exits when its own configuration is unusable, for example when it
cannot reach its gRPC sink at startup: at once when the connection is refused
or the name does not resolve (`rpc error: code = Unavailable` in the log),
after 20 to 50 seconds when nothing answers, as behind a NetworkPolicy that
does not allow the collector out to the receiver. (For a monitor without any
sink no collector is generated; it reports `NoSinkEnabled`.) Connection and
login failures of the database do not make it exit (see
[Monitor is Ready but there are no metrics](#monitor-is-ready-but-there-are-no-metrics)).

A related problem that shows in the log but does not crash the pod:
- **Permission denied** — pgwatch user lacks `pg_monitor` role. Grant it:

```sql
GRANT pg_monitor TO pgwatch;
```

## OpenShift

The collector pod has to run as some user that is not root, and the pgwatch
image names none. Elsewhere the operator names 65534. OpenShift's default
security context constraint, `restricted-v2`, assigns a user from the range
of the namespace and refuses a pod that asks for another:

```text
PodCreateFailed: ... unable to validate against any security context constraint:
... runAsUser: Invalid value: 65534: must be in the ranges: [1000680000, 1000689999]
```

The operator therefore names no user on OpenShift, which it recognises when
it starts by the API group `security.openshift.io`, and its log says what it
decided (`user of the collector containers`). If it decided wrongly, say it
yourself: `collector.runAsUser` in the chart, or `--collector-run-as-user`:

- `none`: name no user, in any namespace;
- a user ID (1 to 2147483647): every collector runs as that user. On
  OpenShift that needs the constraint `nonroot-v2`, or one of your own that
  allows the user and the `runtime/default` seccomp profile, granted to the
  ServiceAccount of the collector (`default`, or `spec.serviceAccountName`).
  `anyuid` does not do: it allows no seccomp profile, and the collector pod
  sets one (`seccomp may not be set`);
- `auto`: the default.

A pod that names no user and is assigned none does not start:
`CreateContainerConfigError`, "container has runAsNonRoot and image will run
as root". That happens

- with `none` on a cluster that is not OpenShift;
- in the namespaces OpenShift does not check pods in. `default`,
  `kube-system`, `kube-public`, `openshift`, `openshift-infra` and
  `openshift-node` are known to the operator, and under `auto` a collector
  there keeps 65534. A namespace that was exempted with the label
  `openshift.io/run-level` is not known to it: name a user ID, or keep
  monitors out of it;
- under a constraint of your own that lets any user in and ranks before
  `restricted-v2`: it assigns none;
- on a cluster that serves `security.openshift.io` without enforcing it,
  because the group was installed for another reason: set
  `collector.runAsUser: "65534"`.

A change of the setting replaces every collector pod it changes the user of.

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
- `Warning/NamespaceTagReplaced` — `spec.database.customTags` has a `namespace`
  key with another value than the monitor's namespace. The tag is the
  operator's; the value is not used. Use another key
- `Normal/ConfigMapRemoved` — the ConfigMap of an earlier configuration, or the
  one of an earlier release, was deleted once no pod read it any more
- `Normal/CredentialsSecretRemoved` — the Secret the operator generated from
  inline credentials was deleted, because the monitor now names a Secret of
  its own
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

- `InlineCredentials` — spec uses inline `username`/`password`; the operator keeps
  them in the Secret `pgpilot-<monitor>-credentials` for the pod to read
- `NameConflict` — a Secret of that name exists and is not the monitor's, see
  [NameConflict](#nameconflict)
- `ApplyFailed` — that Secret could not be written; the usual reason is an
  operator newer than its ClusterRole (`is forbidden: ... cannot patch
  resource "secrets"`, or `cannot create`, in the message)
- `GeneratedSecretReferenced` — `credentialsSecret`, or a certificate, names the Secret the
  operator generates; see the table at the top
- `CredentialsFound` — the `credentialsSecret` exists and has a value under both keys
- `SecretNotFound` — the referenced Secret is missing: the credentials Secret, or
  one a certificate is mounted from
- `SecretKeyMissing` — the Secret exists but has no value under the key named in the
  message (`username` / `password`, or the `usernameKey` / `passwordKey` you set;
  `ca.crt`, `tls.crt`, `tls.key` or the keys you set for a certificate)
- `InvalidCertificate` — a Secret a certificate is mounted from does not hold one
  the collector can load
- `SecretReadFailed` — the API server could not be asked for the Secret; the operator
  retries on its own
- `NoCredentials` — neither inline credentials nor a `credentialsSecret` are set

`CredentialsAvailable: True` means the pod can start with the configured
credentials and certificates. It does not mean the database accepts them: pgwatch keeps running
and retrying when a login fails. Check `pgwatch_instance_up` and the pod log
for that. (Up to v1.0.2 this condition was called `DatabaseReachable`.)

## Rotating the database password

With inline credentials, change `spec.database.password`: the operator updates
the Secret it generated and replaces the collector pod at once.

With a Secret of your own, update the Secret. The operator does not watch Secrets; it reads the one a
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

## Renewing a certificate

The Secrets named under `spec.database.tls` and `spec.sinks.grpc.tls` are
followed like a credentials Secret: read again every five minutes, and a
changed one replaces the collector pod, because pgwatch reads the files once,
when it starts. cert-manager renews a certificate by rewriting its Secret, so
a renewal reaches the collector on its own within that interval; annotate the
monitor as above to have it at once.

Renew before the old certificate expires by more than the interval. When an
authority is replaced, put the old and the new certificate into the Secret
together (`ca.crt` may hold several) until every server has a certificate of
the new one.

A renewal that leaves the Secret unusable (`SecretKeyMissing`,
`InvalidCertificate`) is reported and changes nothing for the running
collector, which keeps what it started with.

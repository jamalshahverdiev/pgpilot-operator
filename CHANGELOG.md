# Changelog

## Unreleased

### Where the collector pod runs, and as whom

- `spec.scheduling` places the collector pod: `nodeSelector`, `tolerations`,
  `affinity`, `topologySpreadConstraints`, `priorityClassName`, passed on to
  the pod as they are.
- `spec.image.pullSecrets` names the Secrets with credentials for the
  registry of the pgwatch image. A private mirror no longer needs them
  attached to the `default` ServiceAccount of every namespace.
- `spec.serviceAccountName` names the ServiceAccount of the collector pod.
  The kubelet still mounts no token into it. With this field, permission to
  write a monitor is permission to run a container as any ServiceAccount of
  the namespace, the identity a cloud provider binds to it included: see
  `spec.serviceAccountName` in `docs/crd-reference.md`.
- `spec.env` adds environment variables to the pgwatch container, for
  settings of the PostgreSQL driver and of pgwatch that have no field of
  their own (`PGOPTIONS`, `PGCONNECT_TIMEOUT`, `PW_MIN_DB_SIZE_MB`, a proxy).
  The variables the operator sets or passes to pgwatch itself are refused.
- **OpenShift.** The collector pod asked for user 65534, which the
  `restricted-v2` security context constraint refuses: outside the few
  namespaces OpenShift does not check, no collector started without
  granting `nonroot-v2` to the ServiceAccount of its namespace. On
  OpenShift, recognised by the API group `security.openshift.io`, the pod
  now names no user and gets one from the range of its namespace; in the
  namespaces where OpenShift assigns none (`default`, `kube-system`,
  `kube-public`, `openshift`, `openshift-infra`, `openshift-node`) it keeps
  65534. `--collector-run-as-user` (`collector.runAsUser` in the chart) says
  it explicitly: `auto`, `none` or a user ID.

Upgrade notes:

- Monitors that set none of the new fields get the pod they had, with one
  addition: the pod template names the ServiceAccount `default`, which is
  the account it ran as anyway. **An account that was set on a collector's
  Deployment by hand, or by a policy, is replaced by `default`**: put it into
  `spec.serviceAccountName` of the monitor before upgrading.
- On OpenShift the collectors of a namespace where `nonroot-v2` was granted
  to make them start now run as a user of the namespace's range, because
  `restricted-v2` validates them again and is tried first. Set
  `collector.runAsUser: "65534"` to keep them as they were. A namespace that
  is exempt from the constraints by the label `openshift.io/run-level`, and
  not one of those named above, needs that setting as well.

### A ConfigMap for each configuration

- The ConfigMap of a monitor is no longer changed in place. Each rendered
  configuration gets a ConfigMap of its own, named after the hash of its
  content (`pgpilot-<monitor>-config-<hash>`), and each pod template mounts
  the one it was made for. pgwatch reads its files again every two minutes,
  and the pod of the previous configuration runs until its successor is
  available: with one ConfigMap for both it was handed the successor's
  configuration, which could name certificate files it never mounted or
  leave it without the `metrics.yaml` its arguments pointed at. It stopped
  collecting exactly when the new pod could not start, which is when the old
  one is all there is.
- The ConfigMaps are immutable: the API server refuses a change to their
  content.
- The ConfigMaps of earlier configurations are removed once the rollout is
  complete and nothing mounts them any more (`ConfigMapRemoved` event).

Upgrade notes:

- The ConfigMap of earlier releases, `pgpilot-<monitor>-config`, is left as
  it is while the pod that mounts it runs, and removed after the rollout that
  the upgrade causes. Anything that reads the ConfigMap by that name has to
  find it another way: `kubectl get configmap -l pgpilot.io/monitor=<name>`,
  or the hash in `status.configHash`.
- **A monitor needs room for two ConfigMaps while a change rolls out**, and
  the upgrade is such a change for every monitor at once. In a namespace
  whose ResourceQuota limits `configmaps` and has no room, the new ConfigMap
  cannot be created: the monitor reports `ApplyFailed` with `exceeded quota`
  and its collector goes on as it was. Raise the quota by one for each
  monitor before upgrading.
- A ConfigMap of your own under a name of the form
  `pgpilot-<monitor>-config-<ten hex characters>` is never touched; if the
  operator needs that very name, the monitor reports `NameConflict`.
- After a downgrade the earlier release writes `pgpilot-<monitor>-config`
  again and leaves the hash-named ConfigMaps behind. They belong to the
  monitor and go with it, or can be deleted by hand.

### TLS certificates

- `spec.database.tls` names the certificates for the connection to the
  database: `caSecretRef`, the certificate authority the server is checked
  against, and `clientCertSecretRef`, a client certificate for a server that
  asks for one. Until now `sslmode: verify-ca` and `verify-full` could only
  be used with a server whose certificate a public authority had signed: the
  collector had no way to be given another, and a database with a
  certificate of an authority of one's own was refused as `certificate
  signed by unknown authority`.
- `spec.sinks.grpc.tls.caSecretRef` is used. The field existed and did
  nothing: the collector connected in plaintext whatever it said, and exited
  at once at a receiver that only speaks TLS. Naming a Secret now turns TLS
  on for the sink, with the receiver checked against that authority and the
  host of `endpoint`.
- The Secrets are mounted into the collector pod read-only, key by key.
  Before that the operator checks that each exists, has its keys and holds
  what pgwatch can load, and says what is wrong in the monitor's status:
  `SecretNotFound` and `SecretKeyMissing`, for these Secrets with the field
  that names them, and the new reason `InvalidCertificate`. A certificate
  and its key can be one value of a Secret (`certKey` and `keyKey` the same).
- A renewed certificate replaces the collector pod, within the interval at
  which credentials Secrets are read again (five minutes by default,
  `--credentials-recheck-interval`): pgwatch reads the files once, when it
  starts. This works with Secrets that cert-manager writes.
- The API server refuses certificates with an `sslmode` that would not use
  them: `tls.caSecretRef` needs `require`, `verify-ca` or `verify-full`,
  `tls.clientCertSecretRef` anything but `disable`. `sslmode` is `disable`
  unless it is set.

Upgrade notes:

- **A monitor that already sets `spec.sinks.grpc.tls.caSecretRef` changes
  behaviour**: its collector connected without TLS and will now use TLS, with
  the Secret mounted. If the Secret does not exist, has no `ca.crt` or holds
  no certificate, the monitor reports `SecretNotFound`, `SecretKeyMissing` or
  `InvalidCertificate` and keeps the collector it has until that is put
  right; if the receiver does not speak TLS, the new collector exits, the
  monitor reports `CrashLoopBackOff`, and the pod it had goes on as it was.
  Remove the field to stay on plaintext. Find such monitors before the
  upgrade:

  ```bash
  kubectl get pgpilotmonitors -A -o json | jq -r '.items[]
    | select((.spec.sinks.grpc.tls.caSecretRef // "") != "")
    | "\(.metadata.namespace)/\(.metadata.name): \(.spec.sinks.grpc.tls.caSecretRef)"'
  ```
- An `endpoint` of the gRPC sink that carries `?sslrootca=<file>` itself,
  which was the only way to get TLS, keeps working as long as
  `tls.caSecretRef` is not set; once it is, the Secret is what counts.
- Adding certificates to a monitor that runs replaces its pod, and the pod
  it replaces keeps collecting, without them, until the new one is
  available.
- Monitors without certificates are rendered as before; their collectors are
  not restarted by this change.
- No change to the operator's role: the Secrets are read with the `get` it
  already has.

### Inline credentials

- An inline `spec.database.password` no longer shows in the collector's pod
  template. It was written there as the plain value of `PGPASSWORD`: readable
  by everyone who may look at a Deployment, a ReplicaSet or a Pod (the
  built-in `view` role, which may read neither Secrets nor the monitor), and
  kept in the ReplicaSets of earlier revisions after it was changed. The
  operator now generates a Secret, `pgpilot-<monitor>-credentials`, owned by
  the monitor, and the pod reads `PGUSER` and `PGPASSWORD` from it. The
  password is still in the PgpilotMonitor itself; a Secret of your own
  (`credentialsSecret`) remains the way to keep it out of there.
- A password with `$$` or `$(NAME)` in it arrives unchanged. As a plain
  value the kubelet rewrote it (`Pa$$w0rd` became `Pa$w0rd`), and the
  collector could not log in.
- A changed inline password replaces the collector pod, as a changed Secret
  does.
- The collector's Deployment keeps no ReplicaSets of earlier revisions
  (`revisionHistoryLimit: 0`). Nothing rolls back to one, and they are where
  an inline password of an earlier release would stay.

Upgrade notes:

- **The operator's ClusterRole changes on Secrets**: `get`, `list`, `watch`
  in the last release; `get`, `create`, `patch`, `delete` in this one. `list`
  and `watch` are gone (see "Credentials" below); `create`, `patch` and
  `delete` are new, and are what generating that Secret takes. RBAC cannot
  limit them to a name, so the role allows them for every Secret of the
  cluster: whoever holds the operator's ServiceAccount token could overwrite
  or delete any Secret, or have a token made for any ServiceAccount. The
  operator itself uses them for the Secret it generates and for no other: a
  Secret of that name that is not the monitor's is left as it is, and the
  monitor reports `NameConflict`. Upgrade the role together with the image,
  as `helm upgrade` does; a new operator under the old role reports
  `ApplyFailed` for every monitor with inline credentials and leaves their
  collectors as they are.
- The ReplicaSets of earlier revisions of a collector are removed once its
  pod of the new template is available, and with them the inline passwords
  they held. A password that was readable there should be considered seen:
  rotate it if that matters. This happens for a monitor the operator
  reconciles to the end. One that reports `NameConflict`, `ApplyFailed`,
  `InvalidMetric`, `InvalidPreset`, `LibraryResolveFailed`, `InvalidName` or
  `NoSinkEnabled` keeps the Deployment and the ReplicaSets of the earlier
  release, password included, until that is corrected. To find Deployments
  that still hold one:

  ```bash
  kubectl get deployments -A -l app.kubernetes.io/managed-by=pgpilot-operator -o json | jq -r '
    .items[] | select(any(.spec.template.spec.containers[].env[]?; .name == "PGPASSWORD" and .value != null))
    | "\(.metadata.namespace)/\(.metadata.name)"'
  ```
- A username or password that was written with doubled `$` to make an
  earlier release log in (`a$$$$b` for the password `a$$b`) now arrives as
  written: set it back to the real value when upgrading. The monitor is
  `Ready` either way; a failed login shows in the pod's log and in
  `pgwatch_instance_up`.
- `pgpilot-<monitor>-credentials` cannot be named as the monitor's own
  `credentialsSecret`: the operator removes it once a monitor has no inline
  credentials. Such a monitor reports `GeneratedSecretReferenced` and is left
  as it is. To move from inline credentials to a Secret, create one under
  another name.
- The generated Secret of a monitor that turns to a Secret of its own is
  removed once the pod of the new template is available, not before: until
  then the pod that runs still reads it.
- `kubectl rollout undo` on a collector's Deployment has nothing to go back
  to. It never lasted: the operator applies the pod template again.

### The collector pod

- A monitor with the gRPC sink alone gets a collector that becomes ready.
  Its pod carried the port and the TCP probes of the Prometheus sink, on
  which nothing listened: it never became ready, was restarted about once a
  minute, and the monitor stayed `PodNotReady`. Without the Prometheus sink
  the pod now has no port and no probes. In place of a readiness probe it
  counts as available once it has run for a minute. That is longer than
  pgwatch takes to give up on a receiver it cannot use (at once when the
  connection is refused or the name does not resolve, 20 to 50 seconds when
  nothing answers), so a collector that cannot deliver is reported as
  `CrashLoopBackOff` and never as running.
- A monitor without any sink in use reports `NoSinkEnabled`, and no collector
  is generated for it; one without a sink exits as soon as it starts. The API
  server refuses such a monitor, so this is about one stored before it did,
  and about a gRPC sink that is enabled and names no endpoint. What an
  earlier release generated for such a monitor is left as it is, the pod
  restarting, until a sink is enabled or the monitor is deleted.
- `spec.image.pullPolicy` reaches the pod. The field was accepted and
  ignored.
- The pod passes the `restricted` Pod Security Standard: its seccomp profile
  is `RuntimeDefault`. In a namespace that enforces that standard no
  collector pod could be created.
- The pod no longer mounts a ServiceAccount token. The collector never talks
  to the Kubernetes API.
- The pod template carries the annotation
  `cluster-autoscaler.kubernetes.io/safe-to-evict-local-volumes: tmp`.
  Without it the pod's `emptyDir` kept the cluster autoscaler from removing
  the node it ran on. The annotation gives way to one of the same name in
  `spec.podMetadata.annotations`.
- [observability.md](docs/observability.md#sample-timestamps-and-metrics-on-long-intervals)
  says why a metric collected less often than every five minutes shows gaps
  in Prometheus, and how to query it. The generated ServiceMonitor is not
  changed for it: the setting that closes the gaps makes every `rate()`
  uneven.

Upgrade notes:

- Every collector pod is replaced once when the operator is upgraded, because
  the pod template changed. As with every change, the new pod is started
  before the old one is stopped.
- A monitor with an `image` block that names no `pullPolicy` has
  `IfNotPresent`, the default of the field, and its pod now gets exactly
  that. For the tag `latest` Kubernetes used to pull at every start; set
  `pullPolicy: Always` to keep that.
- A collector with the gRPC sink alone has no `ports`, no probes, and
  `minReadySeconds: 60` on its Deployment. Its monitor turns `Ready` a minute
  after the pod has started, and at every change the pod of the previous
  spec keeps running for that minute next to the new one; both send to the
  receiver meanwhile.

### What the collector is given to work with

- A monitor that only names a preset no longer gets a `metrics.yaml`, and
  its collector no `--metrics` flag: it runs on the metric definitions inside
  its image. The operator used to hand every such monitor its own copy of
  the pgwatch 6.2.0 registry, which took the place of the one in the image.
  A monitor that pins another pgwatch release in `spec.image.tag` therefore
  ran that release with the definitions of 6.2.0; it now runs it with its
  own. The ConfigMap of such a monitor shrinks from about 250 kB to a few
  hundred bytes. A monitor with metrics of its own (inline or from a
  library) still gets the file, with the built-in definitions next to its
  own when it uses a preset as well: pgwatch reads either the file or its
  own registry, not both.
- A metric that replaces one of the preset by name keeps the preset's
  interval unless its definition names one. It used to be collected every 60
  seconds, so that correcting the statement of a metric the preset runs every
  two hours made it run every minute.
- The built-in definitions are handed on as pgwatch ships them, with every
  key. Keys the operator had no field for (`statement_timeout_seconds`,
  `is_private`) were dropped on the way; pgwatch 6.2.0 does not read them,
  a later release may.
- Every series carries the namespace of its monitor: the operator adds
  `namespace` to the custom tags of the source, next to `dbname`, which is
  the monitor's name alone. Two monitors of one name in two namespaces were
  one database to a shared gRPC receiver and to a scraper that sets no
  namespace label of its own. A target scraped through the generated
  ServiceMonitor has a `namespace` label already, so Prometheus would keep
  the scraped one as `exported_namespace`; the ServiceMonitor removes that
  label where it only repeats the target's. A metric of your own with a
  `tag_namespace` column carries that column's value in the label instead of
  the monitor's namespace, as before.
- The example dashboard (`examples/grafana/pgpilot-overview.json`) has a
  Namespace variable and tells monitors of one name in two namespaces apart;
  it used to draw them as one and add their lock counts together.
- The operator parses the pgwatch registry once, not several times in every
  reconcile of every monitor.

Upgrade notes:

- Every collector pod is replaced once when the operator is upgraded: the
  generated configuration of every monitor changes.
- **Make the new image pullable before upgrading** (mirror
  `cybertecpostgresql/pgwatch:6.2.0` first where the cluster cannot reach
  it). Until the new pod can start, the collector of the previous release
  goes on as it was, on the ConfigMap it was started with, and the monitor
  reports why it is not ready (`ImagePullBackOff`, ...).
- Scraped through the generated ServiceMonitor, the series end up as they
  were. Until Prometheus has loaded the changed ServiceMonitor, the first
  scrapes of a new pod are stored with an extra `exported_namespace` label;
  those series go on answering queries for five minutes next to the final
  ones, so a sum over a monitor can be too high for that long, once.
- Scraped another way (pod or Service annotations, a scrape configuration of
  your own), every series of a database gains the label `namespace`, which
  makes it a new series to Prometheus: graphs continue, an alert that is
  pending starts its `for:` period again. Where the scrape configuration
  sets a `namespace` label on the target itself, Prometheus keeps the
  scraped one as `exported_namespace`;
  [observability.md](docs/observability.md#prometheus-annotation-based-discovery)
  has the rule that removes it. A gRPC receiver finds `namespace` among the
  custom tags of each measurement.
- A `namespace` key in `spec.database.customTags` is replaced by the
  namespace the monitor is in, and the operator records a `Warning` event
  (`NamespaceTagReplaced`) on the monitor. Under the generated
  ServiceMonitor such a monitor used to have `exported_namespace=<your
  value>` on every series; that label no longer exists, and a selector on it
  matches nothing. Move the value to another key before upgrading.
- A metric of your own that carries the name of one in the preset and names
  no `interval` is now collected at the preset's interval, not every 60
  seconds. Set `interval: 60s` to keep that.
- `spec.metrics.preset` is checked against the presets of pgwatch 6.2.0, as
  before, also for a monitor whose image is another release.

### Metric definitions stored before the validation rules

- The rules this release adds for metric definitions (`sqls` keys that are
  plain major versions, no empty statements, `masterOnly` or `standbyOnly`
  but not both, unique names) are applied by the API server when an object is
  written. A definition stored earlier still reached the operator, which
  made what it could of it without a word: a key such as `"*"` was dropped,
  and a metric left without any statement was handed to the collector with
  none. The operator now checks stored definitions by the same rules. A
  monitor that uses one that breaks a rule, from a library or inline, reports
  `InvalidMetric` and names the library, the metric and what is wrong with
  it.
- `PgpilotMetricLibrary`: the `Valid` condition says whether every definition
  can be used (`False` / `InvalidMetric` names the ones that cannot). It used
  to be true for every library that had at least one entry. The status is
  written only when it changes, and no longer with a full update of the
  object.

Upgrade notes:

- Before upgrading, on every Kubernetes version, run the command further
  down under "Upgrade notes" (`kubectl get pgpilotmonitors,pgpilotmetriclibraries
  ... | jq`) and correct what it prints. Correcting a definition is accepted
  by every Kubernetes version, before and after the upgrade.
- A monitor that uses a definition breaking one of the rules becomes
  `Ready=False` with reason `InvalidMetric` when the operator is upgraded.
  Its collector keeps running with the configuration it has, and nothing is
  changed for the monitor until the definition is corrected: no rotated
  Secret, no change of the spec and no new pgwatch release reach it. One
  such metric in a library does that to every monitor that lists the library.
- This includes definitions that were collected as they were written: a
  `"*"` key next to a version key (the key was dropped, the rest collected),
  `"014"` or `"+14"` (read as 14), two metrics of one name in one list (the
  last one was used) and `masterOnly` with `standbyOnly` (collected on
  primaries). A metric of the same name in a library and in the monitor, or
  in two libraries, is still the documented way to override one.
- Kubernetes before 1.33 does not let the operator write the status of an
  object that itself breaks a rule. A monitor whose own
  `spec.metrics.custom` holds the definition therefore keeps the status it
  had, and `READY` can go on saying `true` while nothing is done for it. The
  operator records a `Warning` event with reason `InvalidMetric` on the
  monitor at every attempt and logs `update status ... is invalid`. A library
  that breaks a rule likewise keeps the `Valid=True` an earlier release
  wrote; the monitors that use it report `InvalidMetric` as on every version.
- The `ImageDefaultRestored` event no longer says that the collector "now
  runs" the operator's default image: the tag is removed from the spec when
  the operator starts, and the collector follows at the monitor's next
  successful reconcile.

### Monitors that can always be deleted, and names that are not taken over

- The operator no longer puts a finalizer on PgpilotMonitors. The finalizer
  protected nothing (everything generated for a monitor is owned by it and
  garbage collected with it) and made every deletion wait for a running
  operator: with the operator uninstalled, scaled to zero or without its
  permissions, a monitor, its namespace and the CRD stayed in `Terminating`.
  `helm uninstall` before the monitors were deleted was the usual way there.
  When it starts, the operator takes `pgpilot.io/finalizer` off the monitors
  that still carry it; a monitor that is being deleted is let go at once.
- An object that already carries the name of a generated one (`pgpilot-<name>`,
  `pgpilot-<name>-config-<hash>`, `pgpilot-<name>-credentials`) and is not the
  monitor's is left alone.
  The monitor reports `NameConflict` and names the object. Such an object
  used to be overwritten, given the monitor as its owner and deleted together
  with it; anyone allowed to create a PgpilotMonitor could rewrite a Service
  or ConfigMap that way.
- A monitor has a Service and a ServiceMonitor only while its Prometheus sink
  is on. With the sink off nothing listens on the port they point at; they
  are no longer created, and those of an earlier spec are removed, with a
  `ScrapeTargetRemoved` event for each on the monitor.
- When the cluster stops serving ServiceMonitors while the operator runs (the
  prometheus-operator was uninstalled), monitors keep being reconciled. Each
  says so in a new condition, `ServiceMonitorApplied`, and the ServiceMonitor
  is applied again once the API is back. Every reconcile used to fail at that
  point, before the status was written.
- At start the operator logs whether it found the ServiceMonitor CRD, and
  exits when the API server cannot say. A failed check used to count as "not
  installed", silently, until the next restart.
- Helm chart: with `leaderElection.enabled: false` the operator Deployment
  uses the `Recreate` strategy, so that the pod of the old release is gone
  before the one of the new release starts.

Upgrade notes:

- Monitors lose their `pgpilot.io/finalizer` at the first start of this
  release. Nothing else about them changes: the spec is not written and no pod
  is restarted. Tools that wait for the finalizer to appear on a new monitor
  have nothing to wait for any more.
- The exception, on Kubernetes 1.28 and 1.29 only: a monitor from an earlier
  release that breaks one of the validation rules added in this release keeps
  the finalizer until its spec is corrected, because those API servers refuse
  every write to it (see the table under "Upgrade notes" further down). The
  operator logs `could not take the finalizer off every monitor` with the
  names.
- An operator that runs without leader election outside the Helm chart
  (`--leader-elect` not set) has to be restarted once more after the upgrade:
  during a rolling update the pod of the old release puts the finalizer back
  on the monitors the new one has just released. To see whether any monitor
  carries one:

  ```bash
  kubectl get pgpilotmonitors -A \
    -o jsonpath='{range .items[?(@.metadata.finalizers)]}{.metadata.namespace}/{.metadata.name}{"\n"}{end}'
  ```
- A monitor whose stored image tag `5.1.0` was written by an earlier release
  and is kept for now, for any of the reasons listed in
  docs/troubleshooting.md (`--migrate-stored-image-tag=false` and the
  annotation `pgpilot.io/keep-image-tag` among them), gets the annotation
  `pgpilot.io/operator-wrote-image-tag: "5.1.0"` in exchange for the
  finalizer. It records what the finalizer proved, so that the tag can still
  be removed later, when the reason is gone. The operator removes it when it
  removes the tag; after a tag you changed or removed yourself it is left
  behind, means nothing and can be deleted.
- Rolling back to a release before 1.1 puts the finalizer back on every
  monitor that release reconciles.
- Monitors with `spec.sinks.prometheus.enabled: false` lose their Service and
  ServiceMonitor. Nothing answered on that port.
- A monitor whose generated name is taken by an object the operator had
  adopted keeps it: the object carries the monitor as its controller. Only
  objects that are not controlled by the monitor count as a conflict.
- Generated objects that lost their owner references are no longer taken
  back. Deleting a monitor with `--cascade=orphan` and creating it again used
  to hand the running collector to the new monitor; now the new monitor
  reports `NameConflict` until the leftovers are deleted, and the old
  collector keeps running unchanged meanwhile. A monitor that is deleted and
  created again while its objects are still being collected reports
  `NameConflict` for a moment and then gets new ones, a new pod among them.

### Status that says what is running

- `Ready` now means that the pgwatch pod **of the current spec** is running
  and available. It used to be true as soon as the Deployment had one ready
  replica, and during a rollout that replica is the pod of the previous spec:
  a change whose pod could never start (image typo, a Secret key that does not
  exist, resources no node can give) left the monitor at `READY=true` for
  good, at the new generation, while the old configuration kept running.
- When a monitor is not ready, the `Ready` condition says why:
  `RolloutInProgress`, `PodNotReady`, `ImagePullBackOff`, `CrashLoopBackOff`,
  `CreateContainerConfigError`, `Unschedulable`, `PodCreateFailed` (quota, Pod
  Security), `RolloutStuck`, with the pod's or the API server's message. It
  was always `PodNotReady: pgwatch pod is not ready yet`.
- Every way a reconcile can end writes one consistent status. `status.ready`
  always says what the `Ready` condition says; `observedGeneration` is always
  the generation that was looked at; a condition the reconcile did not get to
  (for example `ConfigGenerated` while the Secret is missing) is `Unknown`
  instead of whatever was left from before; `status.podName` names a pod that
  runs now, a ready one first, and is empty when none runs.
- A pod is judged by what it is doing now. A container that exited once and
  is being restarted is `PodNotReady` with the exit code in the message, and
  `CrashLoopBackOff` only from the second restart or on the kubelet's word.
  Init containers that ran to their end (as service meshes inject them), pods
  that failed for good and wait to be garbage collected, and pods of a
  previous spec are not mistaken for the pod that is starting.
- Failures after the configuration is rendered are reported. An object
  refused as invalid (`ApplyRejected`, e.g. a memory request above its limit,
  or an admission policy that denies the Deployment) and any other failed
  write (`ApplyFailed`) used to be an error in the operator's log and nothing
  else. A refused object is sent again every five minutes and whenever the
  monitor changes; other failures are retried with backoff as before.
- A monitor whose name the generated Service cannot carry (more than 55
  characters, or a dot) is reported with `InvalidName` and nothing is
  generated for it. With a dot, or with 56 to 63 characters, it used to get a
  ConfigMap and a Deployment and then fail on the Service indefinitely, with
  an empty status; with a longer name nothing could be created at all.
- Events are recorded when the `Ready` condition changes, not on every look.
  A monitor waiting for its Secret produced a `SecretNotFound` event every 30
  seconds.
- The status is written only when it changes. `status.lastReconciled` is the
  time of the last change, not a heartbeat. `status.ready` is part of every
  such write, so a monitor that an earlier release left without the field
  gets it.
- A monitor whose collector is held up (`ImagePullBackOff`,
  `CrashLoopBackOff`, `Unschedulable`, ...) is looked at once a minute; one
  that is rolling out or starting, every 15 seconds.

Upgrade notes:

- Tools that wait for `status.ready` or `Ready=True` now wait for the rollout
  to finish. A monitor whose last change never rolled out, and which showed
  `READY=true` regardless, turns `false` with the reason; its old pod is not
  touched.
- `READY` is `false` for the duration of every rollout, also the one a
  rotated Secret or an operator upgrade causes, while the previous pod keeps
  collecting. Give alerts on `Ready=False` a hold time of a few minutes.
- `Ready=True` has the message `pgwatch pod is running` (was `... running and
  scraping`; the operator does not know whether it scrapes).
- Monitors with a name longer than 55 characters or containing a dot: see
  `InvalidName` in docs/troubleshooting.md. The operator stops applying
  anything for them: their existing pod keeps running, but a rotated Secret,
  a change of the spec and the move to pgwatch 6.2.0 no longer reach it.
  Recreate the monitor under a shorter name. To find them before upgrading:

  ```bash
  kubectl get pgpilotmonitors -A --no-headers \
    -o custom-columns=NS:.metadata.namespace,NAME:.metadata.name |
    awk 'length($2) > 55 || $2 ~ /\./'
  ```
- Alerts on `Warning` events of PgpilotMonitors see one event per problem
  instead of a growing count. At the upgrade, a monitor whose problem was not
  reported before (a rollout that never finished, an invalid name) records
  one `Warning` event.

### pgwatch 6.2.0

Collectors now run pgwatch 6.2.0 instead of 5.1.0, and the metric definitions
the operator hands to them are those of the same release (78 metrics and 17
presets instead of 74 and 15).

- The default image tag is no longer part of the CRD. `spec.image.repository`
  and `spec.image.tag` have no schema default any more; the operator fills them
  in when it builds the pod and does not write them into the object. A monitor
  that leaves the tag unset follows the operator from now on, and one that sets
  it keeps it.
- Operator releases up to v1.0.2 wrote `image: {repository:
  cybertecpostgresql/pgwatch, tag: "5.1.0"}` into every monitor they
  reconciled, which would keep all existing monitors on 5.1.0. When it starts,
  the operator removes that tag again, once, and records an
  `ImageDefaultRestored` event on the monitor. It goes by the record the API
  server keeps of who wrote each field (`managedFields`) and removes the tag
  only where that record shows no writer but the old operator. See "Which
  monitors move to 6.2.0" below for what it leaves alone and how to keep 5.1.0
  on purpose. `--migrate-stored-image-tag=false`
  (`migrations.storedImageTag: false` in the chart) turns it off.
- `spec.metrics.preset` is checked against pgwatch's presets, also when it is
  used without custom metrics. A name that is not a preset, and pgwatch's
  presets for other kinds of sources (`patroni`, `pgbouncer`, `pgpool`), used
  to give a collector that ran, looked healthy and collected nothing. Such a
  monitor now gets `Ready=False` / `ConfigGenerated=False` with reason
  `InvalidPreset`, a message listing the usable presets, and a warning event.
- The operator's writes are recorded by the API server under the field manager
  `pgpilot-operator` instead of `manager`.
- The copy of pgwatch's metric definitions in the operator comes with pgwatch's
  licence (BSD 3-Clause), in the source tree and in the image (`/licenses`).
  `make update-pgwatch-registry PGWATCH_VERSION=x.y.z` fetches the definitions
  of another release; `make test-collector` runs the generated configuration
  against the real pgwatch image and a PostgreSQL container, and CI does so for
  PostgreSQL 14, 18 and 19.

Upgrade notes:

- **Every collector pod is replaced once**, with the new image, the new metric
  definitions, or both. The new image has to be pullable:
  `cybertecpostgresql/pgwatch:6.2.0`. Where it is not (air-gapped clusters,
  mirrors), the new pod stays in `ImagePullBackOff` and the old one keeps
  collecting until the image is available, or until its container is next
  started: see "What the collector is given to work with" above, and mirror
  the image before upgrading.
- **Upgrade the CRDs together with the operator.** The old CRD defaults
  `spec.image.tag` to 5.1.0 and puts it back as fast as it is removed. The
  operator notices (`the installed PgpilotMonitor CRD still defaults
  spec.image.tag` in its log), retries for a few minutes and then waits for
  its next start. The Helm chart and `dist/install.yaml` update both at once.
- **Which monitors move to 6.2.0**: those without `spec.image.tag`, and those
  whose tag 5.1.0 nobody but an earlier operator release wrote (or nobody at
  all: a default the old CRD filled in). The operator leaves a tag alone, and
  names the monitor in its log at start, when

  - another writer owns the field: a tag you set when creating the monitor, or
    set, changed or applied later with a different value, or with server-side
    apply (Flux, `kubectl apply --server-side`),
  - the manifest last applied with `kubectl apply` (which is also what Argo CD
    uses by default) contains a tag,
  - the monitor has its own `spec.image.repository`,
  - the object carries no ownership record (restored from a backup without
    `managedFields`).

  One case cannot be told apart: writing `tag: "5.1.0"` over the identical
  value the old operator had already stored - with `kubectl patch`, `kubectl
  edit`, `kubectl replace` or a Helm upgrade - changes nothing in the object,
  and the API server keeps no trace of it. Such a tag is taken for the old
  operator's and removed. **To keep 5.1.0 on a monitor on purpose, annotate it
  before upgrading:**

  ```bash
  kubectl annotate pgpilotmonitor <name> -n <namespace> pgpilot.io/keep-image-tag=true
  ```

  The same applies to monitors created by another controller whose binary is
  called `manager`. To see what is left on the old tag after the upgrade, and
  to move a monitor by hand:

  ```bash
  kubectl get pgpilotmonitors -A \
    -o custom-columns='NAMESPACE:.metadata.namespace,NAME:.metadata.name,REPOSITORY:.spec.image.repository,TAG:.spec.image.tag'
  kubectl patch pgpilotmonitor <name> -n <namespace> --type=json \
    -p '[{"op":"remove","path":"/spec/image/tag"}]'
  ```

  On Kubernetes 1.28 and 1.29 a monitor that also breaks one of the validation
  rules introduced in this release cannot be changed at all until its spec is
  corrected (see the table under "Validation"); that includes removing the
  tag, by the operator or by hand.

- **Monitors that stay on pgwatch 5.x.** One that only names a preset runs on
  the definitions inside its own image and keeps its series
  (`pgwatch_settings_<setting>` and so on). One with metrics of its own,
  inline or from a library, is handed the operator's copy of the 6.2.0
  registry next to them, since pgwatch reads either a file or its own
  registry: pgwatch 5.1.0 runs with it, but collects the 6.2.0 preset
  contents (see the next point) and logs `no SQL found for metric version`
  once per interval for `checkpointer` on PostgreSQL 14-16 and for the
  PostgreSQL 19 metrics. Staying on 5.x is meant as a short-term measure.
- **Series that change name or meaning.** Check dashboards and alert rules:

  | Before | With pgwatch 6.2.0 |
  |--------|--------------------|
  | `pgwatch_settings_<setting>` (presets `exhaustive`, `exhaustive_no_python`, `full`, `rds`, `aurora`, `azure`, `gce`, `aiven`, `debug`) | `pgwatch_settings_num_value{name="<setting>"}`, for every numeric setting. `pgwatch_settings_max_connections` becomes `pgwatch_settings_num_value{name="max_connections"}` |
  | `pgwatch_recommendations_duplicate_count` | `pgwatch_recommendations_affected_count`, now also present (value 1) for unused and invalid indexes |
  | `pgwatch_backends_longest_*_seconds` in whole seconds, rounded up | fractional seconds |
  | `pgwatch_backends_total`, `_active`, `_idle` include the collector's other sessions | the collector holds one session, so the counts are 2-3 lower per monitor; the `longest_*`, `avg_*` and `max_xmin_age_tx` series are absent while no other session exists in the database |
  | `NaN` and `Inf` values of float columns are exported | the sample is left out |
  | label and metric names with characters outside `[a-zA-Z0-9_]` (custom tag keys, `tag_` columns, column names of custom metrics) are exported verbatim to Prometheus 3.4 and newer | exported with `_`, as they always were to Prometheus 2.x. Prometheus 3.0-3.3 gets the verbatim names from both versions |

  The dashboard in `examples/grafana` uses none of the renamed series.
- **Slow metric queries are cancelled.** pgwatch 6 fetches all metrics of a
  monitor with one worker, those due at the same moment in one batch. The
  batch has 30 seconds (or the greatest common divisor of the monitor's
  intervals, if that is longer); what has not finished is cancelled and tried
  again alone, with the metric's own interval (at least 30 seconds) as limit.
  pgwatch 5 let a query run and delivered late. A custom metric slower than
  the second limit stops producing data, and every slow query delays the other
  metrics of the same monitor while it runs. See "Query time" in
  docs/crd-reference.md. `statementTimeoutSeconds` is still ignored.
- **Failing metrics are logged every time**, not once in ten minutes: a
  monitor on preset `standard`, `exhaustive` or `full` without pgwatch's
  PL/Python helpers, or any preset with `stat_statements` without the
  `pg_stat_statements` extension, logs an error per interval for those metrics.
  `exhaustive_no_python` avoids the first.
- **TLS connections** (`sslmode` other than `disable`) use SCRAM channel
  binding when the server offers it; nothing to configure.
- PostgreSQL 14 remains the oldest version with built-in metrics; PostgreSQL 19
  is supported, with three metrics of its own (`autovacuum_scores`, `recovery`,
  `stat_lock`). Two metrics of preset `debug` (`subscription_stats`,
  `table_stats_approx`) fail on PostgreSQL 19 with pgwatch 6.2.0's own SQL.

### Validation

The API server now rejects objects the operator cannot run. These were accepted
before and failed later, usually without a clear message:

- `spec.sinks` without an enabled sink, or with the gRPC sink enabled but no
  `endpoint`. `sinks: {}` now really means "Prometheus on port 9187": the
  Prometheus sink is enabled whenever `sinks.prometheus` is left out. To run
  with gRPC only, set `sinks.prometheus.enabled: false`.
- `spec.metrics` selecting nothing.
- The same library listed twice, and two metrics with the same name in one
  list (the last one used to win silently).
- `sqls` keys that are not PostgreSQL major versions written as plain integers.
  `"*"`, `"14.2"` and `"v14"` were dropped silently, leaving the metric without
  SQL; use `"0"` for "every version". `"014"` and `"+14"` were read as 14 and
  now have to be written `"14"`. Keys above 999 (a `server_version_num` such as
  `170000`) never matched a server. Empty statements are rejected too.
- `masterOnly` and `standbyOnly` both set on one metric.
- A `host` that is not a host name or IP address (URL metacharacters, a port
  typed into the host) and a database name over 63 characters.
- `customTags` keys that cannot work: empty, starting with `__`, or `dbname`
  (pgwatch sets it to the monitor name). Other keys are still accepted; on the
  Prometheus sink a character outside `[a-zA-Z0-9_]` is exported as `_`.
- `podMetadata` / `serviceMetadata` labels and annotation keys Kubernetes
  itself would refuse on the Pod or Service. Keys under `pgpilot.io/` are
  reserved for the operator.

New limits, to keep validation cost bounded: 256 metrics per library and per
`custom` list, 32 `sqls` entries per metric, 32 libraries per monitor, 32
`customTags`, 32 labels and 64 annotations per metadata block. `preset` must be
lower-case letters, digits and `_` (at most 63 characters), Secret keys letters,
digits, `-`, `_` and `.`, and a gRPC `endpoint` must not contain whitespace.

Fixed alongside:

- An IPv6 `host` works, with or without brackets; it used to be glued to the
  port into a different address. Database names with `#`, `?`, `%` or spaces
  no longer cut the connection URL short. Connection strings for ordinary host
  and database names are unchanged, so no pod is rolled because of this.
- The finalizer of earlier releases is removed without rewriting the spec.
  Adding it used to send the whole object back, which made the API server
  store defaults for `spec.image` on every monitor and counted as a spec
  change.
- `status.ready` is always written, so `kubectl get pm` shows `false` instead
  of an empty READY column.
- `kubectl get pml` shows the number of metrics (`status.metricCount`); the
  column was always empty.
- `statementTimeoutSeconds` is deprecated. It was never applied: pgwatch has no
  per-metric timeout. The field is still accepted and ignored.

Upgrade notes:

- A monitor that omitted `sinks.prometheus` (`sinks: {}`, or only a `grpc`
  block) gets the Prometheus sink as soon as the CRD is updated: defaults also
  apply to stored objects when they are read, so its pod is rolled with
  `/metrics` on port 9187. Add `prometheus: {enabled: false}` to keep a
  gRPC-only monitor without it.
- A monitor that set both `masterOnly` and `standbyOnly` was collecting on
  primaries only; keep `masterOnly` to preserve that.
- Existing objects that break one of the new rules are not changed, but how
  much the API server tolerates them depends on the Kubernetes version:

  | Kubernetes | Editing other fields, labels | Deleting | Status written by the operator |
  |------------|------------------------------|----------|--------------------------------|
  | 1.33+      | works                        | works    | works                          |
  | 1.30–1.32  | works                        | works    | refused until the object is fixed: READY, conditions and `metricCount` stop updating and the operator logs `update status ... is invalid`. The pgwatch pod keeps running; where the rule is one about metric definitions it is no longer brought up to date (`InvalidMetric`, see above), and a `Warning` event on the monitor says so. |
  | 1.28, 1.29 | refused                      | hangs in `Terminating` | refused |

  In every case the way out is to correct the offending field, which is always
  accepted - also while the object is `Terminating`. A list that exceeds a new
  size limit has to be shortened before any entry of it can be edited.
- Before upgrading, on any Kubernetes version, look for objects that will be
  affected. The usual ones are the `"*"` key in `sqls` and a monitor without
  an enabled sink. The command prints the monitors and libraries with a
  metric definition that breaks a rule (a `sqls` key that is no plain major
  version, an empty or missing statement, both roles, a name that is no
  lower-case identifier or is used twice in one list), the monitors that use
  such a library, and the monitors without a sink:

  ```bash
  kubectl get pgpilotmonitors,pgpilotmetriclibraries -A -o json | jq -r '
    def metrics: if .kind == "PgpilotMonitor" then (.spec.metrics.custom // []) else (.spec.metrics // []) end;
    def broken: metrics as $m
      | ($m | any((.sqls // {}) | keys | any(test("\\A(0|[1-9][0-9]{0,2})\\z") | not)))
        or ($m | any((.sqls // {}) | (length == 0 or any(. == ""))))
        or ($m | any(.masterOnly == true and .standbyOnly == true))
        or ($m | any((.name // "") | test("\\A[a-z_][a-z0-9_]{0,62}\\z") | not))
        or (($m | map(.name) | unique | length) != ($m | length));
    [.items[] | select(.kind == "PgpilotMetricLibrary" and broken)
      | "\(.metadata.namespace)/\(.metadata.name)"] as $libraries
    | .items[] | . as $o
    | select(
        broken
        or (.kind == "PgpilotMonitor"
            and any(.spec.metrics.fromLibraries[]?; "\($o.metadata.namespace)/\(.name)" | IN($libraries[])))
        or (.kind == "PgpilotMonitor"
            and .spec.sinks.prometheus.enabled == false
            and .spec.sinks.grpc.enabled != true)
      )
    | "\(.kind) \(.metadata.namespace)/\(.metadata.name)"'
  ```

  It does not look at `interval`: a value below `1s`, which releases up to
  v1.0.1 accepted and collected every 60 seconds, is refused as well. On
  Kubernetes 1.33 and later the API server tolerates what the command prints;
  the operator does not use a metric definition that breaks a rule on any
  version (`InvalidMetric`, see "Metric definitions stored before the
  validation rules" above).

### Credentials

- The operator no longer lists or watches Secrets. It reads them one at a
  time, by name (for what its role allows besides, see "Inline credentials"
  above), and Secrets are never cached: it used to keep every Secret of
  the cluster, with its data, in memory. ConfigMaps, Deployments, Services,
  Pods and ServiceMonitors are cached only when they carry the operator's
  `managed-by` label, so the operator's memory no longer grows with the size of
  the cluster.
- A rotated password reaches the collector. The Secret a monitor references is
  read again every five minutes (`--credentials-recheck-interval`, Helm value
  `credentials.recheckInterval`); when it has changed, the pod is replaced.
  Before, pgwatch kept the old password until someone restarted it, while the
  monitor reported `Ready`. Annotating the monitor makes the operator look at
  once.
- A Secret without the username or password key is reported
  (`SecretKeyMissing`, naming the key) instead of leaving the pod in
  `CreateContainerConfigError` behind a green condition. A Secret that cannot
  be read is reported as `SecretReadFailed` and retried with backoff rather
  than being called "not found". `credentialsSecret.name` has to be a valid
  object name.
- While credentials are unusable, `status.ready` is `false` together with the
  `Ready` condition; the READY column used to keep showing `true`.
- The `DatabaseReachable` condition is now `CredentialsAvailable`. It never
  checked that the database could be reached, only that credentials were
  configured; the old condition is removed from existing objects. Anything
  that reads `DatabaseReachable` (alerts, scripts) has to be changed.
- A monitor is no longer reconciled again by its own status updates.

Upgrade notes:

- Upgrade (and roll back) the operator image and its ClusterRole together, as
  `helm upgrade` / `helm rollback` do. An operator older than this release
  under the new role cannot list Secrets any more: it logs `secrets is
  forbidden` and stops reconciling every monitor while its pod stays Ready.
  Do not pin `image.tag` to an older version with the new chart.
- Every collector that takes its credentials from a Secret is restarted once
  when the operator is upgraded (its pod template gains the
  `pgpilot.io/credentials-version` annotation).
- From then on any change to a referenced Secret object restarts the
  collector, also a change to its labels or annotations only.

### Changed

- Built with Go 1.27 (was 1.25, which no longer receives security fixes). The
  builder and base images are pinned by digest.
- Kubernetes libraries 0.37, controller-runtime 0.25, prometheus-operator API
  types 0.95; `golang.org/x/net`, `x/text`, gRPC and OpenTelemetry moved to
  versions without known vulnerabilities.
- `make deploy` and `make build-installer` no longer rewrite
  `config/manager/kustomization.yaml`; `make install`, `uninstall`, `deploy`
  and `undeploy` fail when kustomize fails; `make docker-buildx` fails when the
  build or push fails and builds for `linux/amd64` and `linux/arm64`.
- `make test-e2e` uses a kubeconfig of its own (`bin/e2e-kubeconfig`) and
  refuses to run against any other context, so the suite can no longer install
  or remove anything in the cluster your kubeconfig points at. The Kind cluster
  is deleted even when the suite fails.
- New `make helm-validate` (renders the chart with its toggles and validates
  the objects against the Kubernetes schemas) and `make govulncheck`.
- CI: actions pinned by commit, least-privilege token, timeouts, no duplicate
  runs for pull requests; the linter and all tools come from the versions
  pinned in the Makefile; `go.mod` must be tidy; weekly vulnerability scan.

## v1.0.2

Operator image `v1.0.2`, Helm chart `0.1.2`.

Security and installation fixes. Upgrading is recommended for every install.

### Security

- pgwatch's built-in web UI and REST API are now disabled (`--web-disable=all`)
  in every collector pod. They listened on `0.0.0.0:8080` without
  authentication, and the `/test-connect` endpoint could be used by any
  workload able to reach the pod to obtain the monitored database's
  credentials. Existing collector pods are rolled automatically when the
  operator is upgraded.

### Fixed

- Helm: `helm install` with default values no longer fails with
  `invalid ownership metadata` — the CRDs were shipped twice (in the chart's
  `crds/` directory and again through a template). They are now rendered only
  by the template, so `installCRDs=false` really skips them and `helm upgrade`
  keeps them current.
- Helm: the chart's `PgpilotMonitor` CRD was a stale copy without inline
  `username`/`password`, the credential validation rules and
  `status.configHash`. Inline-credential monitors were rejected on Helm
  installs and a `ConfigMapUpdated` event was emitted on every reconcile. The
  chart CRDs are now copied from the generated ones by `make manifests`.
- Helm: the default image tag now matches the published `vX.Y.Z` image tag
  (it pointed at `1.0.1`, kustomize at `v1.0.1`).
- A metric `interval` that is not a valid duration (`"60"`, `"1d"`, `""`) is
  now rejected by the API server. Such a value used to be stored and then
  stopped the operator from reading any PgpilotMonitor or
  PgpilotMetricLibrary, crash-looping it after a restart. Intervals must be at
  least `1s`.
- A Prometheus sink on port 8080 no longer collides with pgwatch's web server.

### Changed

- The CRDs installed by the chart carry `helm.sh/resource-policy: keep`:
  `helm uninstall` leaves them (and your monitors) in place.
- The documented `helm install` command pins the chart version.
- `make manifests` also refreshes `charts/pgpilot-operator/files/crds`; CI
  fails when generated files are stale.

### Upgrade notes

- Helm, existing releases. First check whether the release already owns the
  CRDs:

  ```bash
  kubectl get crd pgpilotmonitors.pgpilot.io \
    -o jsonpath='{.metadata.annotations.meta\.helm\.sh/release-name}'
  ```

  - It prints the release name (the release was installed with `--skip-crds`
    or `--take-ownership`): a plain `helm upgrade --set installCRDs=true` is
    enough. Do not set `installCRDs=false` on such a release before this
    upgrade has run — the CRDs only get `helm.sh/resource-policy: keep` from
    this version on, and without it Helm would delete them.
  - It prints nothing (the CRDs came from the chart's `crds/` directory, i.e.
    the release was installed with `installCRDs=false`): either let the
    release adopt them with
    `helm upgrade --set installCRDs=true --take-ownership` (Helm >= 3.17), or
    keep managing them yourself with `--set installCRDs=false` and
    `kubectl apply --server-side --force-conflicts -f charts/pgpilot-operator/files/crds/`.

  `helm upgrade` reuses the values of the previous release, so
  `installCRDs=true` has to be passed explicitly if it was `false` before;
  otherwise the stale CRDs stay in place without any error.
- Objects that already store an unparseable `interval` (`"60"`, `"1d"`) must
  have it corrected with `kubectl edit` or `kubectl patch`. Until then the API
  server rejects every other write to the object, including the removal of its
  finalizer, so deleting such a monitor only completes once the interval is
  fixed. Objects with a parseable interval below `1s` keep working; the next
  edit of their metrics has to raise it to at least `1s`.

## v1.0.1

Testbed enhancements — no operator code changes.

- Added third test database (`app_tertiary` on `:15434`) with analytics schema
  (tenants, events, reports), FK-missing joins, unused indexes and HOT-update
  churn to exercise a domain distinct from primary (users/orders) and secondary
  (payments/merchants)
- `hack/testdb/init/tertiary.sql`, `init/workload-tertiary.sh`,
  `docker-compose.yaml` service entries, and `setup.sh` heredocs for
  `PgpilotMetricLibrary/tertiary-metrics` + `PgpilotMonitor/tertiary-db` so a
  single `setup.sh` run brings up all three monitors
- Demonstrates zero-config extensibility: adding the third monitor required no
  changes to the operator, Grafana dashboard JSON, or Prometheus scrape config

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
- Release artifacts: Docker image (Docker Hub) + Helm OCI chart, published manually via `make docker-push` + `helm push`
- Documentation: quickstart, CRD reference, architecture, observability, troubleshooting

# Troubleshooting

## Monitor stuck in Ready=False

**Check conditions:**

```bash
kubectl describe pgpilotmonitor <name> -n <namespace>
```

Look at the `Conditions` section. Common causes:

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

### PodNotReady

The pgwatch pod exists but has not passed its readiness probe. Check pod logs:

```bash
kubectl logs -n <namespace> -l pgpilot.io/monitor=<name>
```

Common causes:
- The pod cannot be scheduled or its image cannot be pulled
  (`kubectl describe pod`)
- The pod was created only a moment ago; the probe starts after a few seconds

A database that cannot be reached, a wrong password or a wrong `sslmode` do
**not** make the pod unready: pgwatch keeps running and retries. See
[Monitor is Ready but there are no metrics](#monitor-is-ready-but-there-are-no-metrics).

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

## ConfigMap not updating after spec change

The operator uses a content-hash annotation (`pgpilot.io/config-hash`) on the pod template to trigger rollouts. If the hash does not change, no rollout occurs.

Check:

```bash
kubectl get configmap pgpilot-<name>-config -n <namespace> -o yaml
```

If the ConfigMap content matches your spec, the hash is correct and no rollout is needed.

## ServiceMonitor not created

The operator checks for `monitoring.coreos.com/v1` CRD at **startup**. If prometheus-operator was installed after the pgpilot-operator, restart the operator:

```bash
kubectl rollout restart deployment -n pgpilot-system <operator-deployment>
```

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

Event types. The `Normal` ones are emitted on transitions; the `Warning` ones
about credentials are repeated each time the operator looks again (every 30
seconds while the problem lasts; Kubernetes folds repeats into one event with
a count):

- `Normal/ConfigMapUpdated` — generated ConfigMap hash changed (i.e. rollout
  is about to happen)
- `Normal/PodRunning` — pgwatch pod became Ready after being NotReady
- `Warning/PodNotReady` — pgwatch pod lost readiness after being Ready
- `Warning/SecretNotFound` — referenced `credentialsSecret` doesn't exist
- `Warning/SecretKeyMissing` — the Secret has no value under the username or password key
- `Warning/SecretReadFailed` — the Secret could not be read from the API server
- `Warning/NoCredentials` — the monitor has neither inline credentials nor a `credentialsSecret`
- `Warning/LibraryResolveFailed` — a referenced `PgpilotMetricLibrary` is missing

The absence of a `PodRunning` event does not mean the pod is down —
events are only fired on transitions to reduce noise. Check
`status.ready` / `status.conditions` for the current state.

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

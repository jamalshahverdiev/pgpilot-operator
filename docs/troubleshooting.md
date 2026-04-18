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
- Wrong database credentials (pgwatch fails to connect)
- Database host unreachable (DNS, network policy, firewall)
- Wrong `sslmode` (e.g. `require` when the server does not support TLS)

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

Common issues:
- **Connection refused** — database host/port wrong or not reachable
- **Authentication failed** — wrong username/password in the Secret
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
- CRDs not installed (`installCRDs: false` in Helm values but no manual CRD install)

## Events

The operator records Kubernetes events on PgpilotMonitor resources:

```bash
kubectl get events -n <namespace> --field-selector involvedObject.name=<monitor-name>
```

Event types (emitted only on condition transitions, not every reconcile):

- `Normal/ConfigMapUpdated` — generated ConfigMap hash changed (i.e. rollout
  is about to happen)
- `Normal/PodRunning` — pgwatch pod became Ready after being NotReady
- `Warning/PodNotReady` — pgwatch pod lost readiness after being Ready
- `Warning/SecretNotFound` — referenced `credentialsSecret` doesn't exist
- `Warning/LibraryResolveFailed` — a referenced `PgpilotMetricLibrary` is missing

The absence of a `PodRunning` event does not mean the pod is down —
events are only fired on transitions to reduce noise. Check
`status.ready` / `status.conditions` for the current state.

Condition reasons on `DatabaseReachable`:

- `InlineCredentials` — spec uses inline `username`/`password`
- `CredentialsFound` — spec uses a `credentialsSecret` and it exists
- `SecretNotFound` — the referenced Secret is missing

# Test Database Environment

Local development and smoke-testing environment for pgpilot-operator. Brings up
three PostgreSQL instances with distinct realistic schemas and workload
generators, then wires PgpilotMonitor CRs pointing at them.

> **⚠ SECURITY WARNING**
>
> **All credentials in this directory are local test values only — never reuse
> them anywhere real.**
>
> - `pgwatch / pgwatch_secret` — pgwatch monitoring user (init SQL + setup.sh)
> - `postgres / postgres` — superuser (docker-compose.yaml)
>
> These passwords are committed to the repository intentionally so contributors
> can reproduce the test setup. They must **never** be used:
>
> - on any publicly reachable PostgreSQL instance
> - in staging or production
> - as the starting point for a real user's credentials
> - in any Secret or config referenced from a non-test PgpilotMonitor
>
> If you ever copy-paste from here into a real setup, regenerate the values.

## What's here

| File | Purpose |
|------|---------|
| `docker-compose.yaml` | Three PostgreSQL 16 containers (`primary`, `secondary`, `tertiary`) plus matching `workload-*` containers that run continuous psql loops |
| `init/primary.sql` | Schema + seed data for `app_primary` (10k users, 100k orders) |
| `init/secondary.sql` | Schema + seed data for `app_secondary` (500 merchants, 50k payments) |
| `init/tertiary.sql` | Schema + seed data for `app_tertiary` (500 tenants, 80k events, 10k reports) |
| `init/workload-primary.sh` | Continuous INSERT/UPDATE/DELETE + slow joins against `app_primary` |
| `init/workload-secondary.sh` | Continuous activity against `app_secondary` |
| `init/workload-tertiary.sh` | Continuous analytics workload against `app_tertiary` |
| `setup.sh` | Orchestrates: `docker compose up` → create K8s namespace, Secret, PgpilotMetricLibraries, PgpilotMonitors |

## Usage

```bash
# Bring everything up (requires the operator to already be deployed to the cluster)
hack/testdb/setup.sh

# Tear down
hack/testdb/setup.sh teardown
```

`setup.sh` automatically picks up the host IP from interface `enp0s8` (or falls
back to `hostname -I`). Override with `HOST_IP=x.y.z.w hack/testdb/setup.sh`
when running on a differently-named interface.

## Intentional problems to exercise monitoring signals

The schemas are crafted to produce the kinds of issues that show up in a real
fleet:

- **Missing indexes** — `orders.user_id`, `payments.user_id`, `events.tenant_id`
  have no index; joins cause `seq_scan` on these tables
- **Unused indexes** — `idx_orders_note_unused`, `idx_users_email_unused`,
  `idx_payments_description_unused`, `idx_events_trace_id_unused` are never
  read by any query
- **Bloat churn** — the workload scripts `UPDATE` and `DELETE` without
  `VACUUM`, so `approx_bloat_percentage` and `n_dead_tup` accumulate
- **Slow analytical queries** — the workload runs `GROUP BY` over the full
  payments/orders tables; visible in `stat_statements`

Watch these metrics in a port-forward to the pgwatch pod to see them move over
time.

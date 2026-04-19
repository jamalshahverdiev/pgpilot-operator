# Stress scenarios

Production-like incident simulations you can layer on top of the regular
testdb environment. Each scenario targets one class of real-world
PostgreSQL pain so the corresponding pgwatch metric moves visibly.

**Opt-in by design.** The default `hack/testdb/setup.sh` does not start
these — they affect tuple visibility, row locks, and autovacuum behavior
in ways that would muddy quick smoke tests.

## Running

Bring up the regular environment, then add one or more stress scenarios:

```bash
# Normal environment (always needed as the base)
docker compose -f hack/testdb/docker-compose.yaml up -d --wait

# All four scenarios
docker compose -f hack/testdb/docker-compose.yaml --profile stress up -d

# Just one
docker compose -f hack/testdb/docker-compose.yaml up -d stress-bloat

# Stop a specific scenario (environment keeps running)
docker compose -f hack/testdb/docker-compose.yaml stop stress-bloat

# Tear down everything
docker compose -f hack/testdb/docker-compose.yaml --profile stress down -v
```

## Watching results

Port-forward to the pgwatch pod for the primary monitor (all scenarios hit
`app_primary`):

```bash
kubectl port-forward -n pgpilot-test svc/pgpilot-primary-db 19187:9187
curl -s http://localhost:19187/metrics | grep '<metric-name>'
```

## Scenarios

### stress-bloat — runaway bloat with autovacuum disabled

**Script:** `stress/bloat.sh`

Turns off autovacuum on `orders`, then hammers the table with ~100
UPDATE/s that change an indexed column (forces HOT-chain breakage).
Simulates an operator forgetting to re-enable autovacuum after a manual
migration.

Expected signals (climb over ~2–5 minutes):

| Metric | Direction |
|--------|-----------|
| `pgwatch_table_bloat_approx_summary_sql_approx_bloat_percentage` | ↑ |
| `pgwatch_table_stats_n_dead_tup{table_name="orders"}` | ↑ |
| `pgwatch_table_stats_n_mod_since_analyze{table_name="orders"}` | ↑ |
| `pgwatch_db_size_size_b` | ↑ |

Recovery: the script's SIGTERM handler re-enables autovacuum so the env
recovers once you stop it.

### stress-locks — blocking chain from idle-in-transaction holders

**Script:** `stress/locks.sh`

Opens `HOLDERS=5` long-lived transactions that take `FOR UPDATE` on the
same user rows (IDs 1–10), then sleep for `HOLDER_SECS=600` seconds.
The regular workload's `UPDATE users SET last_seen = now()` blocks
behind them. Simulates a leaky connection pool that never commits.

Expected signals (note: since all holders try `FOR UPDATE` on the same
rows, PostgreSQL serializes them — only the first one gets the lock and
becomes idle-in-transaction; the rest sit waiting and show up as blocked
queries / tuple locks):

| Metric | Expected value |
|--------|---------------|
| `pgwatch_backends_idleintransaction` | `== 1` (the current holder) |
| `pgwatch_backends_longest_tx_seconds` | grows while a holder sleeps |
| `pgwatch_backends_longest_query_seconds` | grows on waiters (they're blocked mid-query) |
| `pgwatch_locks_count{locktype="tuple"}` | `== HOLDERS - 1` (waiters) |
| `pgwatch_locks_mode_count{lockmode="RowShareLock"}` | grows (FOR UPDATE individual row locks) |
| `pgwatch_db_stats_deadlocks` | stays `0` (no cycles, just blocking) |

### stress-planner — snapshot pinning + seq_scan storm

**Script:** `stress/planner.sh`

Three concurrent patterns:

1. Holds a `REPEATABLE READ` transaction with `SELECT pg_sleep(600)` —
   pins `xmin`, prevents VACUUM's visibility-map updates
2. Runs big JOIN on `orders.user_id` (no index) on repeat — drives
   `seq_scan` counter
3. Bulk `INSERT 500 orders` every 3s — skews planner statistics vs the
   last ANALYZE

Simulates the "long-running analytical query holding back VACUUM" anti-pattern.

Expected signals:

| Metric | Direction |
|--------|-----------|
| `pgwatch_table_stats_seq_scan{table_name="orders"}` | ↑ fast |
| `pgwatch_table_stats_seq_scan{table_name="users"}` | ↑ |
| `pgwatch_backends_longest_query_seconds` | spikes to snapshot duration |
| `pgwatch_backends_max_xmin_age_tx` | ↑ (xmin pinned by REPEATABLE READ snapshot holder) |
| `pgwatch_backends_longest_tx_seconds` | ↑ to `SNAPSHOT_SECS` |
| `pgwatch_table_stats_n_tup_ins{table_name="orders"}` | ↑ (bulk INSERT loop) |
| `pgwatch_db_stats_blks_read` | ↑ only if data doesn't fit in shared_buffers; on a small test DB this may stay flat because the whole table is cached |

### stress-idle-tx — lone idle-in-transaction holder

**Script:** `stress/idle-tx.sh`

Single `BEGIN;` + `pg_sleep(7200)` = 2 hours. Nothing else — just holds
a transaction open. Simulates a pod or developer session that crashed
without rolling back. Quiet but blocks VACUUM across the whole DB.

Expected signals:

| Metric | Expected |
|--------|----------|
| `pgwatch_backends_idleintransaction` | `>= 1` |
| `pgwatch_backends_longest_tx_seconds` | grows to `IDLE_TX_SECS` |
| `pgwatch_backends_max_xmin_age_tx` | grows with every committed tx on the DB |
| `pgwatch_table_stats_n_dead_tup` | ↑ slowly (VACUUM ineffective) |

## Combining scenarios

All four can run at once. A common production failure mode is
`stress-idle-tx` + normal workload → gradual bloat without any obvious
smoking gun until xid wraparound risk becomes visible. Try that combo
to see `max_xmin_age_tx` and `n_dead_tup` grow together while the
system looks otherwise fine.

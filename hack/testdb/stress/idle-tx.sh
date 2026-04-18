#!/bin/sh
# stress-idle-tx: open a plain BEGIN; and sit forever. Simulates an
# application bug where connections leak with open transactions, which
# blocks VACUUM, prevents tuple freezing, and accumulates bloat silently.
#
# Metrics to watch:
#   pgwatch_backends_idleintransaction                             — stays >= 1
#   pgwatch_backends_longest_tx_seconds                            — grows indefinitely
#   pgwatch_backends_max_xmin_age_tx                               — grows every tx on DB
#   pgwatch_table_stats_n_dead_tup                                 — grows (VACUUM can't reclaim)
#
# Duration controlled by IDLE_TX_SECS (default 2h). Set very high to
# demonstrate "forgot about this pod" effect.

set -eu

PGPASSWORD="${POSTGRES_PASSWORD:-postgres}"
export PGPASSWORD
HOST="${TARGET_HOST:-postgres-primary}"
DB="${TARGET_DB:-app_primary}"
USER="${TARGET_USER:-postgres}"
IDLE_TX_SECS="${IDLE_TX_SECS:-7200}"

echo "[stress-idle-tx] opening BEGIN; and going idle for ${IDLE_TX_SECS}s (${HOST}/${DB})"

# Use psql's \! (shell sleep) rather than SELECT pg_sleep(). pg_sleep runs
# as an active query, so pg_stat_activity.state would be 'active' — not
# 'idle in transaction'. \! sleeps in the psql client, meaning the
# backend has no query running and truly enters the idle-in-transaction
# state we want to simulate.
command psql -h "$HOST" -U "$USER" -d "$DB" -v ON_ERROR_STOP=0 <<SQL
BEGIN;
SELECT 'tx-start' AS status, txid_current() AS tx;
\! sleep ${IDLE_TX_SECS}
ROLLBACK;
SQL

echo "[stress-idle-tx] done"

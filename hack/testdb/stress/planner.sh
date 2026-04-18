#!/bin/sh
# stress-planner: keep expensive sequential-scan queries in flight to keep
# shared buffers polluted, and simulate stale statistics. Exercises the
# query planner/analysis metrics pgpilot-operator watches on real fleets.
#
# Runs three patterns in parallel:
#   1. Repeated seq_scan on orders.user_id JOIN users (no index on user_id)
#   2. Bulk INSERT that skews statistics vs what planner last saw
#   3. A snapshot-holding transaction that prevents VACUUM visibility-map
#      updates — forcing later queries to seq_scan more pages.
#
# Metrics to watch:
#   pgwatch_table_stats_seq_scan{table_name="users"}               — climbs
#   pgwatch_table_stats_seq_scan{table_name="orders"}              — climbs
#   pgwatch_backends_longest_query_seconds                         — spikes
#   pgwatch_backends_max_xmin_age_tx                               — grows
#   pgwatch_stat_statements_rows{query~="SELECT .* FROM orders.*JOIN.*"} — climbs
#   pgwatch_db_stats_blks_read                                     — climbs (cache misses)

set -eu

PGPASSWORD="${POSTGRES_PASSWORD:-postgres}"
export PGPASSWORD
HOST="${TARGET_HOST:-postgres-primary}"
DB="${TARGET_DB:-app_primary}"
USER="${TARGET_USER:-postgres}"
SNAPSHOT_SECS="${SNAPSHOT_SECS:-600}"

psql() { command psql -h "$HOST" -U "$USER" -d "$DB" -v ON_ERROR_STOP=0 "$@"; }

echo "[stress-planner] starting patterns against ${HOST}/${DB}"

# Pattern 3: long-running read transaction pinning xmin.
(
    # \! sleep so the backend stays idle-in-transaction after the count(*)
    # instead of being 'active' via pg_sleep().
    psql <<SQL
BEGIN ISOLATION LEVEL REPEATABLE READ;
SELECT count(*) FROM orders;
\! sleep ${SNAPSHOT_SECS}
ROLLBACK;
SQL
) >/dev/null 2>&1 &
SNAP_PID=$!

cleanup() {
    echo "[stress-planner] stopping snapshot holder (pid ${SNAP_PID})"
    kill $SNAP_PID 2>/dev/null || true
    exit 0
}
trap cleanup INT TERM

# Pattern 1 + 2 in a loop.
while true; do
    # Big seq-scan JOIN (no index on orders.user_id).
    psql -c "
        SELECT u.tier, count(*), sum(o.amount)
          FROM orders o
          JOIN users u ON u.id = o.user_id
         WHERE o.status IN ('pending', 'completed')
         GROUP BY u.tier;
    " >/dev/null 2>&1 || true

    # Bulk insert to skew statistics vs planner's last ANALYZE.
    psql -c "
        INSERT INTO orders (user_id, product_id, status, note, amount, created_at)
        SELECT
            (random() * 9999 + 1)::int,
            (random() * 999 + 1)::int,
            'pending',
            'planner-stress',
            (random() * 500)::numeric(10,2),
            now()
        FROM generate_series(1, 500) AS i;
    " >/dev/null 2>&1 || true

    sleep 3
done

#!/bin/sh
# Continuous realistic workload for app_primary.
# Generates signals pgwatch picks up:
#   - INSERT/UPDATE/DELETE churn → db_stats, table_stats, WAL activity
#   - Slow JOIN on user_id (no index) → seq_scan counters climb
#   - Queries that hit indexes → index_scan counters
#   - DELETEs without VACUUM → dead tuples, bloat

set -eu

PGPASSWORD="${POSTGRES_PASSWORD:-postgres}"
export PGPASSWORD
HOST="${TARGET_HOST:-postgres-primary}"
DB="${TARGET_DB:-app_primary}"
USER="${TARGET_USER:-postgres}"

run() {
    psql -h "$HOST" -U "$USER" -d "$DB" -v ON_ERROR_STOP=0 -c "$1" >/dev/null 2>&1 || true
}

echo "[workload-primary] starting workload against ${HOST}/${DB}"

while true; do
    # Batch 1: INSERT fresh orders (activity on orders table).
    run "
        INSERT INTO orders (user_id, product_id, status, note, amount)
        SELECT
            (random() * 9999 + 1)::int,
            (random() * 999 + 1)::int,
            'pending',
            'auto-generated',
            (random() * 500 + 10)::numeric(10,2)
        FROM generate_series(1, 20);
    "

    # Batch 2: UPDATE users.last_seen (WAL / HOT updates).
    run "
        UPDATE users
           SET last_seen = now()
         WHERE id IN (SELECT id FROM users ORDER BY random() LIMIT 50);
    "

    # Batch 3: Slow JOIN on orders.user_id (NO INDEX → seq_scan).
    # Forces pgwatch table_stats to show growing seq_scan counter.
    run "
        SELECT u.tier, count(*) AS total, sum(o.amount) AS revenue
          FROM orders o
          JOIN users u ON u.id = o.user_id
         WHERE o.status = 'pending'
         GROUP BY u.tier;
    "

    # Batch 4: Index-using query (orders.status is indexed).
    run "
        SELECT status, count(*)
          FROM orders
         WHERE created_at > now() - interval '1 day'
         GROUP BY status;
    "

    # Batch 5: Mark some pending orders completed (UPDATE churn).
    run "
        UPDATE orders
           SET status = 'completed'
         WHERE id IN (
            SELECT id FROM orders WHERE status = 'pending' ORDER BY random() LIMIT 10
        );
    "

    # Batch 6: DELETE a few old archived orders (creates dead tuples → bloat).
    # We intentionally don't VACUUM here so bloat accumulates.
    run "
        DELETE FROM orders
         WHERE status = 'archived'
           AND created_at < now() - interval '28 days'
         RETURNING id
         LIMIT 5;
    "

    sleep 2
done

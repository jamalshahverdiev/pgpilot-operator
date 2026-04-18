#!/bin/sh
# stress-bloat: disable autovacuum on `orders`, then hammer it with UPDATEs
# that force HOT chain breakage (update indexed column = random()).
# Simulates runaway bloat on a large production table where autovacuum
# can't keep up.
#
# Metrics to watch:
#   pgwatch_table_bloat_approx_summary_sql_approx_bloat_percentage  — climbs
#   pgwatch_table_stats_n_dead_tup{table_name="orders"}             — climbs
#   pgwatch_table_stats_n_mod_since_analyze{table_name="orders"}    — climbs
#   pgwatch_db_size_size_b                                          — climbs
#
# On shutdown, re-enables autovacuum so the environment recovers cleanly.

set -eu

PGPASSWORD="${POSTGRES_PASSWORD:-postgres}"
export PGPASSWORD
HOST="${TARGET_HOST:-postgres-primary}"
DB="${TARGET_DB:-app_primary}"
USER="${TARGET_USER:-postgres}"

psql() { command psql -h "$HOST" -U "$USER" -d "$DB" -v ON_ERROR_STOP=0 "$@"; }

cleanup() {
    echo "[stress-bloat] re-enabling autovacuum on orders..."
    psql -c "ALTER TABLE orders RESET (autovacuum_enabled);" >/dev/null 2>&1 || true
    exit 0
}
trap cleanup INT TERM

echo "[stress-bloat] disabling autovacuum on table orders..."
psql -c "ALTER TABLE orders SET (autovacuum_enabled = false);"

echo "[stress-bloat] entering update storm loop (~100 updates/s)..."
while true; do
    psql -c "
        UPDATE orders
           SET amount = (random() * 500 + 10)::numeric(10,2),
               status = CASE floor(random() * 4)::int
                            WHEN 0 THEN 'pending'
                            WHEN 1 THEN 'completed'
                            WHEN 2 THEN 'failed'
                            ELSE 'archived'
                        END
         WHERE id IN (SELECT id FROM orders ORDER BY random() LIMIT 100);
    " >/dev/null 2>&1 || true
    sleep 1
done

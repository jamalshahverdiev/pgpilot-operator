#!/bin/sh
# Continuous realistic workload for app_secondary.
# Focus on payments: expensive aggregations, state transitions, update churn.

set -eu

PGPASSWORD="${POSTGRES_PASSWORD:-postgres}"
export PGPASSWORD
HOST="${TARGET_HOST:-postgres-secondary}"
DB="${TARGET_DB:-app_secondary}"
USER="${TARGET_USER:-postgres}"

run() {
    psql -h "$HOST" -U "$USER" -d "$DB" -v ON_ERROR_STOP=0 -c "$1" >/dev/null 2>&1 || true
}

echo "[workload-secondary] starting workload against ${HOST}/${DB}"

while true; do
    # Batch 1: INSERT new payments.
    run "
        INSERT INTO payments (user_id, merchant_id, amount, state, description)
        SELECT
            (random() * 9999 + 1)::int,
            (random() * 499 + 1)::int,
            (random() * 500 + 1)::numeric(10,2),
            'pending',
            'transaction ' || gen_random_uuid()::text
        FROM generate_series(1, 30);
    "

    # Batch 2: Slow aggregation — no index on user_id means seq_scan.
    run "
        SELECT m.category, count(*) AS cnt, sum(p.amount) AS total
          FROM payments p
          JOIN merchants m ON m.id = p.merchant_id
         WHERE p.state = 'completed'
           AND p.created_at > now() - interval '7 days'
         GROUP BY m.category;
    "

    # Batch 3: State churn — high UPDATE rate → dead tuples, WAL.
    run "
        UPDATE payments
           SET state = (ARRAY['completed', 'failed', 'refunded'])[1 + floor(random() * 3)::int],
               updated_at = now()
         WHERE state = 'pending'
           AND id IN (SELECT id FROM payments WHERE state = 'pending' ORDER BY random() LIMIT 20);
    "

    # Batch 4: Index-using query (state is indexed).
    run "
        SELECT state, count(*), sum(amount)
          FROM payments
         WHERE created_at > now() - interval '1 hour'
         GROUP BY state;
    "

    # Batch 5: Expensive analytical query.
    run "
        SELECT date_trunc('hour', created_at) AS hour,
               state,
               count(*) AS cnt,
               avg(amount) AS avg_amount
          FROM payments
         WHERE created_at > now() - interval '24 hours'
         GROUP BY hour, state
         ORDER BY hour DESC;
    "

    sleep 2
done

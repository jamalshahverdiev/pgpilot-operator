#!/bin/sh
# Continuous analytics workload for app_tertiary.
# Generates signals pgwatch picks up on the events/reports schema:
#   - High-cardinality INSERT stream into events (WAL, table_stats)
#   - Slow JOIN on events.tenant_id (no FK index → seq_scan grows)
#   - UPDATE churn on reports (HOT updates / dead tuples)
#   - Aggregations by event_type / occurred_at (index usage)

set -eu

PGPASSWORD="${POSTGRES_PASSWORD:-postgres}"
export PGPASSWORD
HOST="${TARGET_HOST:-postgres-tertiary}"
DB="${TARGET_DB:-app_tertiary}"
USER="${TARGET_USER:-postgres}"

run() {
    psql -h "$HOST" -U "$USER" -d "$DB" -v ON_ERROR_STOP=0 -c "$1" >/dev/null 2>&1 || true
}

echo "[workload-tertiary] starting workload against ${HOST}/${DB}"

while true; do
    # Batch 1: INSERT fresh events (high-volume event stream).
    run "
        INSERT INTO events (tenant_id, event_type, payload, trace_id, occurred_at)
        SELECT
            (random() * 499 + 1)::int,
            (ARRAY['page_view', 'click', 'signup', 'purchase', 'logout'])[1 + floor(random() * 5)::int],
            jsonb_build_object('browser', 'chrome', 'ok', true),
            'trace-' || gen_random_uuid()::text,
            now()
        FROM generate_series(1, 50);
    "

    # Batch 2: Slow JOIN on events.tenant_id (NO INDEX → seq_scan climbs).
    run "
        SELECT t.plan, count(*) AS event_count
          FROM events e
          JOIN tenants t ON t.id = e.tenant_id
         WHERE e.occurred_at > now() - interval '1 day'
         GROUP BY t.plan;
    "

    # Batch 3: Index-using query on event_type.
    run "
        SELECT event_type, count(*)
          FROM events
         WHERE occurred_at > now() - interval '1 hour'
         GROUP BY event_type;
    "

    # Batch 4: UPDATE churn on reports (HOT updates).
    run "
        UPDATE reports
           SET status = 'running', computed_at = now()
         WHERE id IN (SELECT id FROM reports WHERE status = 'pending' ORDER BY random() LIMIT 10);
    "

    # Batch 5: Mark some running reports done.
    run "
        UPDATE reports
           SET status = 'done', computed_at = now()
         WHERE id IN (SELECT id FROM reports WHERE status = 'running' ORDER BY random() LIMIT 5);
    "

    # Batch 6: DELETE old events (dead tuples → bloat).
    run "
        DELETE FROM events
         WHERE occurred_at < now() - interval '14 days'
         RETURNING id
         LIMIT 20;
    "

    sleep 2
done

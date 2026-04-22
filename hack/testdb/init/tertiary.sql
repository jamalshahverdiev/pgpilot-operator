-- pgpilot-operator test database: tertiary
-- Analytics/events schema — different domain from primary (users/orders) and
-- secondary (payments/merchants), to show in Grafana as a visibly distinct
-- source. Same intentional problems: missing FK indexes, unused indexes,
-- high UPDATE churn.

CREATE USER pgwatch WITH PASSWORD 'pgwatch_secret' LOGIN;
GRANT pg_monitor TO pgwatch;

CREATE EXTENSION IF NOT EXISTS pg_stat_statements;

CREATE DATABASE app_tertiary OWNER postgres;

\c app_tertiary

CREATE EXTENSION IF NOT EXISTS pg_stat_statements;

-- pgwatch-contrib helper stub (we don't install PL/Python in tests).
CREATE OR REPLACE FUNCTION get_load_average()
RETURNS TABLE(load_1min float, load_5min float, load_15min float)
LANGUAGE SQL AS $$
  SELECT 0.0::float, 0.0::float, 0.0::float;
$$;
GRANT EXECUTE ON FUNCTION get_load_average() TO pgwatch;

CREATE TABLE tenants (
    id serial PRIMARY KEY,
    name text NOT NULL,
    plan text NOT NULL DEFAULT 'free'
);

INSERT INTO tenants (name, plan)
SELECT
    'Tenant ' || i,
    (ARRAY['free', 'starter', 'pro', 'enterprise'])[1 + (i % 4)]
FROM generate_series(1, 500) AS i;

CREATE INDEX idx_tenants_plan ON tenants(plan);

CREATE TABLE events (
    id bigserial PRIMARY KEY,
    tenant_id int NOT NULL,        -- no FK index on purpose
    event_type text NOT NULL,
    payload jsonb NOT NULL DEFAULT '{}',
    trace_id text,                 -- unused index
    occurred_at timestamptz DEFAULT now()
);

INSERT INTO events (tenant_id, event_type, payload, trace_id, occurred_at)
SELECT
    (random() * 499 + 1)::int,
    (ARRAY['page_view', 'click', 'signup', 'purchase', 'logout'])[1 + floor(random() * 5)::int],
    jsonb_build_object('browser', 'chrome', 'ok', true),
    'trace-' || gen_random_uuid()::text,
    now() - (random() * interval '14 days')
FROM generate_series(1, 80000) AS i;

CREATE INDEX idx_events_type ON events(event_type);
CREATE INDEX idx_events_occurred ON events(occurred_at DESC);
CREATE INDEX idx_events_trace_id_unused ON events(trace_id);
-- NOTE: no index on events.tenant_id — joins with tenants will seq_scan.

CREATE TABLE reports (
    id serial PRIMARY KEY,
    tenant_id int NOT NULL,
    status text NOT NULL DEFAULT 'pending',
    computed_at timestamptz
);

INSERT INTO reports (tenant_id, status, computed_at)
SELECT
    (random() * 499 + 1)::int,
    (ARRAY['pending', 'running', 'done', 'failed'])[1 + floor(random() * 4)::int],
    now() - (random() * interval '1 day')
FROM generate_series(1, 10000) AS i;

CREATE INDEX idx_reports_status ON reports(status);

GRANT CONNECT ON DATABASE app_tertiary TO pgwatch;
GRANT USAGE ON SCHEMA public TO pgwatch;
GRANT SELECT ON ALL TABLES IN SCHEMA public TO pgwatch;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT ON TABLES TO pgwatch;

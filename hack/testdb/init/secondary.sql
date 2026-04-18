-- pgpilot-operator test database: secondary
-- Realistic payments schema with intentional problems:
--   * Missing index on payments.user_id (seq_scan on JOINs)
--   * Unused index on payments.description (never queried)
--   * High update churn on payments.state creating dead tuples → bloat

CREATE USER pgwatch WITH PASSWORD 'pgwatch_secret' LOGIN;
GRANT pg_monitor TO pgwatch;

CREATE EXTENSION IF NOT EXISTS pg_stat_statements;

CREATE DATABASE app_secondary OWNER postgres;

\c app_secondary

CREATE EXTENSION IF NOT EXISTS pg_stat_statements;

-- pgwatch-contrib helper stub. The real get_load_average() reads /proc/loadavg
-- via PL/Python, which we don't install in the test database. Returning zeros
-- is fine for a test environment — it just silences the cpu_load metric error.
CREATE OR REPLACE FUNCTION get_load_average()
RETURNS TABLE(load_1min float, load_5min float, load_15min float)
LANGUAGE SQL AS $$
  SELECT 0.0::float, 0.0::float, 0.0::float;
$$;
GRANT EXECUTE ON FUNCTION get_load_average() TO pgwatch;

CREATE TABLE merchants (
    id serial PRIMARY KEY,
    name text NOT NULL,
    category text NOT NULL
);

INSERT INTO merchants (name, category)
SELECT
    'Merchant ' || i,
    (ARRAY['retail', 'food', 'services', 'subscription', 'travel'])[1 + (i % 5)]
FROM generate_series(1, 500) AS i;

CREATE INDEX idx_merchants_category ON merchants(category);

CREATE TABLE payments (
    id bigserial PRIMARY KEY,
    user_id int NOT NULL,            -- no index on purpose
    merchant_id int NOT NULL,
    amount numeric(10,2) NOT NULL,
    state text NOT NULL DEFAULT 'pending',
    description text,                -- has unused index below
    created_at timestamptz DEFAULT now(),
    updated_at timestamptz DEFAULT now()
);

INSERT INTO payments (user_id, merchant_id, amount, state, description, created_at, updated_at)
SELECT
    (random() * 9999 + 1)::int,
    (random() * 499 + 1)::int,
    (random() * 500 + 1)::numeric(10,2),
    (ARRAY['pending', 'completed', 'failed', 'refunded'])[1 + floor(random() * 4)::int],
    'transaction ' || i,
    now() - (random() * interval '90 days'),
    now() - (random() * interval '90 days')
FROM generate_series(1, 50000) AS i;

CREATE INDEX idx_payments_state ON payments(state);
CREATE INDEX idx_payments_created ON payments(created_at DESC);
-- Unused: we never query by description.
CREATE INDEX idx_payments_description_unused ON payments(description);
-- NOTE: no index on payments.user_id or merchant_id — JOINs will seq_scan.

GRANT CONNECT ON DATABASE app_secondary TO pgwatch;
GRANT USAGE ON SCHEMA public TO pgwatch;
GRANT SELECT ON ALL TABLES IN SCHEMA public TO pgwatch;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT ON TABLES TO pgwatch;

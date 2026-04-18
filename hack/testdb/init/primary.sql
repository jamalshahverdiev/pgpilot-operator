-- pgpilot-operator test database: primary
-- Realistic schema with intentional problems for pgwatch to detect:
--   * Missing index on orders.user_id (seq_scan on JOINs)
--   * Unused index on orders.note (never queried)
--   * DELETE without VACUUM → bloat

CREATE USER pgwatch WITH PASSWORD 'pgwatch_secret' LOGIN;
GRANT pg_monitor TO pgwatch;

CREATE EXTENSION IF NOT EXISTS pg_stat_statements;

CREATE DATABASE app_primary OWNER postgres;

\c app_primary

CREATE EXTENSION IF NOT EXISTS pg_stat_statements;

-- pgwatch-contrib helper stub (see secondary.sql for details).
CREATE OR REPLACE FUNCTION get_load_average()
RETURNS TABLE(load_1min float, load_5min float, load_15min float)
LANGUAGE SQL AS $$
  SELECT 0.0::float, 0.0::float, 0.0::float;
$$;
GRANT EXECUTE ON FUNCTION get_load_average() TO pgwatch;

CREATE TABLE users (
    id serial PRIMARY KEY,
    name text NOT NULL,
    email text NOT NULL,
    last_seen timestamptz DEFAULT now(),
    tier text NOT NULL DEFAULT 'free'
);

INSERT INTO users (name, email, last_seen, tier)
SELECT
    'user_' || i,
    'user_' || i || '@example.com',
    now() - (random() * interval '30 days'),
    (ARRAY['free', 'basic', 'pro', 'enterprise'])[1 + (i % 4)]
FROM generate_series(1, 10000) AS i;

CREATE INDEX idx_users_last_seen ON users(last_seen);
CREATE INDEX idx_users_tier ON users(tier);
-- Unused index: queries filter by lower(email) so this raw one is ignored.
CREATE INDEX idx_users_email_unused ON users(email);

CREATE TABLE products (
    id serial PRIMARY KEY,
    sku text UNIQUE NOT NULL,
    name text NOT NULL,
    price numeric(10,2) NOT NULL
);

INSERT INTO products (sku, name, price)
SELECT
    'SKU-' || lpad(i::text, 6, '0'),
    'Product ' || i,
    (random() * 900 + 10)::numeric(10,2)
FROM generate_series(1, 1000) AS i;

CREATE TABLE orders (
    id bigserial PRIMARY KEY,
    user_id int NOT NULL,            -- NO foreign key index on purpose
    product_id int NOT NULL,
    status text NOT NULL DEFAULT 'pending',
    note text,                       -- has unused index below
    amount numeric(10,2) NOT NULL,
    created_at timestamptz DEFAULT now()
);

INSERT INTO orders (user_id, product_id, status, note, amount, created_at)
SELECT
    (random() * 9999 + 1)::int,
    (random() * 999 + 1)::int,
    (ARRAY['pending', 'completed', 'failed', 'archived'])[1 + floor(random() * 4)::int],
    'auto-generated',
    (random() * 500 + 10)::numeric(10,2),
    now() - (random() * interval '30 days')
FROM generate_series(1, 100000) AS i;

-- Indexes: some useful, some intentionally unused.
CREATE INDEX idx_orders_status ON orders(status);
CREATE INDEX idx_orders_created ON orders(created_at DESC);
-- Unused: we never query orders by note.
CREATE INDEX idx_orders_note_unused ON orders(note);
-- NOTE: no index on orders.user_id — joins on user_id will seq_scan.

GRANT CONNECT ON DATABASE app_primary TO pgwatch;
GRANT USAGE ON SCHEMA public TO pgwatch;
GRANT SELECT ON ALL TABLES IN SCHEMA public TO pgwatch;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT ON TABLES TO pgwatch;

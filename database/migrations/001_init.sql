-- Schema for the load balancer project. Runs once on first Postgres start.
CREATE TABLE users (
  id            BIGSERIAL PRIMARY KEY,
  name          TEXT        NOT NULL CHECK (char_length(name) BETWEEN 1 AND 100),
  email         TEXT        NOT NULL UNIQUE CHECK (char_length(email) <= 254),
  password_hash TEXT        NOT NULL,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE products (
  id          BIGSERIAL PRIMARY KEY,
  name        TEXT           NOT NULL,
  description TEXT           NOT NULL DEFAULT '',
  price       NUMERIC(12,2)  NOT NULL CHECK (price >= 0),
  stock       INTEGER        NOT NULL DEFAULT 0 CHECK (stock >= 0),
  category    TEXT           NOT NULL,
  image       TEXT           NOT NULL DEFAULT '',
  created_at  TIMESTAMPTZ    NOT NULL DEFAULT now()
);
CREATE INDEX idx_products_category ON products (category);

CREATE TABLE cart (
  id         BIGSERIAL PRIMARY KEY,
  user_id    BIGINT      NOT NULL UNIQUE REFERENCES users(id) ON DELETE CASCADE,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE cart_items (
  cart_id    BIGINT  NOT NULL REFERENCES cart(id) ON DELETE CASCADE,
  product_id BIGINT  NOT NULL REFERENCES products(id),
  quantity   INTEGER NOT NULL CHECK (quantity BETWEEN 1 AND 99),
  added_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (cart_id, product_id)
);

CREATE SEQUENCE order_seq START 10001;

CREATE TABLE orders (
  id               BIGSERIAL PRIMARY KEY,
  order_number     TEXT          NOT NULL UNIQUE,
  user_id          BIGINT        NOT NULL REFERENCES users(id),
  status           TEXT          NOT NULL DEFAULT 'CONFIRMED',
  payment_status   TEXT          NOT NULL DEFAULT 'PAID',
  total            NUMERIC(12,2) NOT NULL CHECK (total >= 0),
  shipping_address TEXT          NOT NULL DEFAULT '',
  request_id       TEXT          NOT NULL DEFAULT '',
  backend_id       TEXT          NOT NULL DEFAULT '',
  idempotency_key  TEXT          NOT NULL,
  created_at       TIMESTAMPTZ   NOT NULL DEFAULT now(),
  UNIQUE (user_id, idempotency_key)
);
CREATE INDEX idx_orders_user ON orders (user_id, created_at DESC);
CREATE INDEX idx_orders_request ON orders (request_id);

CREATE TABLE order_items (
  id           BIGSERIAL PRIMARY KEY,
  order_id     BIGINT        NOT NULL REFERENCES orders(id) ON DELETE CASCADE,
  product_id   BIGINT        NOT NULL REFERENCES products(id),
  product_name TEXT          NOT NULL,
  unit_price   NUMERIC(12,2) NOT NULL,
  quantity     INTEGER       NOT NULL CHECK (quantity > 0)
);
CREATE INDEX idx_order_items_order ON order_items (order_id);

CREATE TABLE backend_servers (
  id         TEXT PRIMARY KEY,
  name       TEXT        NOT NULL,
  host       TEXT        NOT NULL,
  port       INTEGER     NOT NULL,
  weight     INTEGER     NOT NULL DEFAULT 1 CHECK (weight >= 1),
  enabled    BOOLEAN     NOT NULL DEFAULT TRUE,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE health_checks (
  id          BIGSERIAL PRIMARY KEY,
  backend_id  TEXT        NOT NULL,
  checked_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  healthy     BOOLEAN     NOT NULL,
  status_code INTEGER     NOT NULL DEFAULT 0,
  latency_ms  REAL        NOT NULL DEFAULT 0,
  error       TEXT        NOT NULL DEFAULT '',
  state       TEXT        NOT NULL DEFAULT '',   -- backend state after this check
  transition  BOOLEAN     NOT NULL DEFAULT FALSE -- TRUE when the state changed
);
CREATE INDEX idx_health_backend_time ON health_checks (backend_id, checked_at DESC);
CREATE INDEX idx_health_transitions ON health_checks (checked_at DESC) WHERE transition;

CREATE TABLE request_logs (
  id             BIGSERIAL PRIMARY KEY,
  ts             TIMESTAMPTZ NOT NULL,
  request_id     TEXT        NOT NULL,
  user_id        BIGINT,
  user_name      TEXT        NOT NULL DEFAULT '',
  client_ip      TEXT        NOT NULL DEFAULT '',
  method         TEXT        NOT NULL,
  path           TEXT        NOT NULL,
  action         TEXT        NOT NULL DEFAULT '',
  detail         TEXT        NOT NULL DEFAULT '',  -- product / quantity etc, from backend metadata headers
  order_id       TEXT        NOT NULL DEFAULT '',
  backend_id     TEXT        NOT NULL DEFAULT '',
  status         INTEGER     NOT NULL,
  latency_ms     REAL        NOT NULL,
  request_size   BIGINT      NOT NULL DEFAULT 0,
  response_size  BIGINT      NOT NULL DEFAULT 0,
  success        BOOLEAN     NOT NULL,
  failure_reason TEXT        NOT NULL DEFAULT '',
  retries        INTEGER     NOT NULL DEFAULT 0,
  load_test      BOOLEAN     NOT NULL DEFAULT FALSE
);
CREATE INDEX idx_rl_ts ON request_logs (ts DESC);
CREATE INDEX idx_rl_request ON request_logs (request_id);
CREATE INDEX idx_rl_user ON request_logs (user_id, ts DESC);
CREATE INDEX idx_rl_order ON request_logs (order_id) WHERE order_id <> '';
CREATE INDEX idx_rl_backend ON request_logs (backend_id, ts DESC);
CREATE INDEX idx_rl_failed ON request_logs (ts DESC) WHERE NOT success;

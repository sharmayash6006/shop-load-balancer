# Layer 7 Load Balancer + Shopping Website + Live Admin Dashboard

A complete, runnable system: a **Go Layer 7 HTTP load balancer** in front of three **Go backend servers**, a
**PostgreSQL** database, a **shopping website**, a real-time **admin dashboard**, and **k6** load tests.
Everything the dashboard shows is produced by the running system (real requests, real health checks, real database rows).

```
 CUSTOMER ─► Shopping website (:3000, static SPA) ──API_URL──┐
                                                              ▼
                                         GO LOAD BALANCER (:8080, Layer 7) ─► Backend 1 (:8001) ┐
                                                              ▲   │  health checks   Backend 2 (:8002) ├─► PostgreSQL (:5432)
 ADMIN ─► Admin dashboard (:3001, static SPA) ──API_URL/_lb───┘   │                  Backend 3 (:8003) ┘
                                                                  └─ request logs, metrics, health records ─► PostgreSQL
```

**Three independent applications** plus the data layer:

* **`shopping-website/`**: customer-facing SPA (login, register, products, cart, checkout, orders, profile). It only knows one setting, `API_URL`, the load balancer entry point. It contains no balancing, health-check, backend-selection or admin logic.
* **`admin-dashboard/`**: separate SPA for administrators. It talks to the load balancer's monitoring/control API (`API_URL/_lb/*`, admin-authenticated) and receives live updates over Server-Sent Events.
* **`load-balancer/`**: Go Layer 7 service: algorithms, health checks, failover, metrics, request tracking, rate limiting, admin API.
* **`backend/`** (×3): shop API (auth, products, cart, orders). **`database/`**: one central PostgreSQL schema.

The browser calls the load balancer **directly** (CORS + credentialed cookies). Neither frontend can reach Backend 1/2/3 by design: they only know `API_URL`.

## Quick start

```bash
cp .env.example .env        # (already present) change JWT_SECRET and ADMIN_PASSWORD
docker compose up --build
```

| What | URL |
|---|---|
| Shopping website | http://localhost:3000 (static, calls `API_URL`) |
| Admin dashboard | http://localhost:3001/admin/login (static, calls `API_URL/_lb`) (user/password from `.env`, default `admin` / `change-me-admin-password`) |
| Load balancer | http://localhost:8080 (`/health`, `/ready`, `/metrics`) |
| Backend 1 / 2 / 3 | http://localhost:8001 / :8002 / :8003 (direct access is for debugging; the site does not use it) |
| PostgreSQL | localhost:5432 (`shop` / `shop`) |

The schema and the 8 seed products are created automatically on first start (`database/migrations`).
`docker compose down -v` wipes the database.


## Running the frontends separately (without Docker)

```bash
cd shopping-website && API_URL=http://localhost:8080 PORT=3000 npm run dev    # zero dependencies, Node 18+
cd admin-dashboard  && API_URL=http://localhost:8080 PORT=3001 npm run dev
```
The load balancer must list those origins in `CORS_ORIGINS`.

## Authentication & CORS across separate origins

* Session = the same HttpOnly JWT cookie as before (users: `token`, admin: `lb_admin`), now set by the load balancer response and sent back with `fetch(..., { credentials: 'include' })` / `EventSource(..., { withCredentials: true })`.
* The load balancer answers CORS preflights itself and echoes only allowed origins with `Access-Control-Allow-Credentials: true`; unlisted origins get no CORS headers (preflight → 403). The wildcard `*` is never used. `X-Request-ID`, `X-Served-By`, `Idempotent-Replay` are exposed to the browser.
* `localhost:3000` → `localhost:8080` is *same-site* (ports are ignored), so `SameSite=Lax` cookies work in development. In production put the shop and API under one registrable domain (`yourshop.com`, `api.yourshop.com`, `dashboard.yourshop.com`) with HTTPS and `COOKIE_SECURE=true`; for unrelated domains use `COOKIE_SAMESITE=none`. Backend cookies are issued by the same code path as before.
* CSRF protection is unchanged (custom `X-Requested-With` header → forces a preflight that only allowed origins pass).

## Production layout

```
yourshop.com            → shopping-website container (SHOP_API_URL=https://api.yourshop.com)
dashboard.yourshop.com  → admin-dashboard container  (ADMIN_API_URL=https://api.yourshop.com)
api.yourshop.com        → load-balancer (TLS in front, CORS_ORIGINS=https://yourshop.com,https://dashboard.yourshop.com)
                          → backend-1..N → PostgreSQL
```
Never commit `.env`; `.gitignore` excludes it, `.env.example` documents every variable.

## Features

* **Layer 7 proxy** (own implementation on `net/http`): reads method, path, headers, cookies, body size, status, duration; attributes requests to the logged-in user (verifies the session token, never logs secrets); buffers bodies so requests can be retried.
* **Algorithms**: round robin, weighted round robin (smooth, 5/3/1 gives exactly 5:3:1), least connections, IP hash (sticky; remaps when the primary is down and returns when it recovers). Switchable live from the dashboard.
* **Health**: active `GET /health` checks (every 2 s, 2 failures to mark UNHEALTHY, 2 successes to return) **plus passive checks**: a refused connection during real traffic removes the server immediately. Backend `/health` pings the database.
* **Manual control**: enable / disable / recover / change weight per backend. Disabled = no new requests instantly, in-flight requests finish. Enable → RECOVERING → health check → HEALTHY.
* **Resilience**: failed attempts are retried on another healthy backend; all backends down → `503 NO_HEALTHY_BACKENDS`, shown as an alert on the dashboard.
* **Safe retries**: GET/PUT/DELETE retry freely; `POST` retries only with an `Idempotency-Key` (orders) or when the connection never reached a server. `POST /api/orders` requires the key; the same key returns the original order (`200` + `Idempotent-Replay: true`), even under concurrent retries (unique constraint + transaction).
* **Orders are transactional**: stock lock, stock decrement, order, order items, simulated payment (`PAID`) and cart clearing commit together or not at all.
* **Rate limiting** per client IP (token bucket), `429`, configurable at runtime (0 = off).
* **Timeouts**: connection, per-backend, and total request timeout, all live-configurable; plus retry count.
* **Observability**: structured JSON logs, `X-Request-ID` (`REQ-XXXXXXXX`) end to end (stored on the order), Prometheus `/metrics` (`lb_requests_total`, `lb_requests_failed_total`, `lb_request_duration_seconds`, `lb_active_connections`, `lb_backend_health`, `lb_backend_requests_total`, …), percentiles P50/P95/P99, per-second series for 1 min … 24 h.
* **Dashboard** (live via Server-Sent Events, no refresh): KPIs, backend table with controls, 7 charts with range selector, real-time request table, state-transition feed, failure-reason breakdown, request/failure search, user journeys, orders + **order trace**, load-test panel, settings.
* **Security**: bcrypt passwords, HttpOnly SameSite cookies, `X-Requested-With` CSRF guard, input validation, parameterised SQL, request size limit, security headers/CSP, admin login with brute-force throttling, user sessions cannot open the admin API, CORS disabled unless `CORS_ORIGINS` is set, passwords/tokens never logged.
* **Graceful shutdown** (SIGTERM): stop accepting, drain in-flight requests, flush buffered logs to the database.

## Folder structure

```
load-balancer/   Go LB: cmd/lb, internal/{algorithms,routing,proxy,health,metrics,middleware,store,admin,auth,config}
backend/         Go shop API: cmd/server, internal/{api,auth,database,middleware,models}
shopping-website/  public/ src/ package.json Dockerfile nginx-templates/   (separate app, static files only)
admin-dashboard/   public/ src/ package.json Dockerfile nginx-templates/   (separate app, static files only)
database/        migrations/001_init.sql (schema), 002_seed.sql (products)
load-tests/      k6: ecommerce-test.js load-test.js stress-test.js spike-test.js requests-test.js lib.js
scripts/         failure-test.sh (automated acceptance steps 9-14)
docker-compose.yml  .env.example  Makefile
```

## Environment variables (`.env`)

| Variable | Default | Meaning |
|---|---|---|
| `DATABASE_URL` | built from `POSTGRES_*` | Postgres connection string |
| `BACKEND_URLS` / `BACKEND_WEIGHTS` | 3 backends / `5,3,1` | Pool, in order (`backend-1` …, max 8) |
| `ALGORITHM` | `round_robin` | `round_robin`, `weighted_round_robin`, `least_connections`, `ip_hash` |
| `HEALTH_CHECK_INTERVAL` | `2s` | |
| `REQUEST_TIMEOUT` / `BACKEND_TIMEOUT` / `CONNECT_TIMEOUT` | `30s` / `10s` / `2s` | |
| `RETRY_COUNT` | `2` | extra attempts on other backends |
| `RATE_LIMIT` | `0` | requests/s per IP, `0` = off |
| `LOG_LEVEL` | `info` | |
| `JWT_SECRET` | required | ≥16 chars, shared by LB and backends |
| `ADMIN_USER` / `ADMIN_PASSWORD` | `admin` / required | dashboard login |
| `BCRYPT_COST` | `10` | lower for heavy login tests |
| `SHOP_API_URL` / `ADMIN_API_URL` | `http://localhost:8080` | URL the **browser** uses to reach the load balancer. Written to `config.js` when the frontend container starts: change it without rebuilding or editing source (the `VITE_API_URL` idea, without a build step) |
| `CORS_ORIGINS` | `http://localhost:3000,http://localhost:3001` | browser origins allowed to call the LB with credentials (exact match, never `*`) |
| `COOKIE_SAMESITE` | `lax` | `lax`/`strict` when shop, dashboard and API are on the same site (`localhost`, or `yourshop.com` + `api.yourshop.com`); `none` + `COOKIE_SECURE=true` if on unrelated domains |

## API (via the load balancer, `http://localhost:8080` or the website's `/api`)

Mutating requests need `X-Requested-With: <anything>`.

| Method & path | Auth | Notes |
|---|---|---|
| `POST /api/auth/register` `{name,email,password}` | – | 201 + session cookie |
| `POST /api/auth/login`, `POST /api/auth/logout`, `GET /api/auth/me` | | |
| `GET/PUT /api/profile` | user | |
| `GET /api/products?q=&category=`, `GET /api/products/{id}` | – | |
| `GET /api/cart`, `POST /api/cart {product_id,quantity}`, `PUT /api/cart/items/{id} {quantity}`, `DELETE /api/cart/items/{id}`, `DELETE /api/cart` | user | |
| `POST /api/checkout` | user | validates cart, returns summary |
| `POST /api/orders` + header `Idempotency-Key` | user | body `{address, items?}` (items = buy-now; otherwise the cart) |
| `GET /api/orders`, `GET /api/orders/{ORD-id}` | user | |
| `/_lb/login`, `/_lb/api/*` | admin | stats, backends (`enable|disable|recover|weight`), config, timeseries, requests, users, orders, order trace, loadtest, events (SSE) |

## Running the acceptance test (spec section 52)

1. `docker compose up --build`, open http://localhost:3000, register "Test User", browse, add to cart, **Place order**.
2. Open http://localhost:3001/admin/login. **Requests**/dashboard show `User=Test User, Action=CREATE_ORDER, Order ID=ORD-…, Backend=backend-N, Status=201, Request ID=REQ-…`. **Orders → click the order** for the trace.
3. Start load: `docker compose run --rm k6 run /tests/ecommerce-test.js` (watch the dashboard and **Load testing** page).
4. Disable/enable Backend 3 from the dashboard; `docker compose stop backend-2`; `docker compose stop backend-1 backend-2 backend-3` (503 + "NO HEALTHY BACKENDS"); `docker compose start backend-1 backend-2 backend-3`.
5. Or let `./scripts/failure-test.sh` do steps 9-14 automatically.

## Load testing

```bash
docker compose run --rm k6 run /tests/ecommerce-test.js                 # shopper journey (VUS=50 DURATION=60s)
VUS=200 DURATION=3m docker compose run --rm k6 run /tests/ecommerce-test.js
docker compose run --rm k6 run /tests/load-test.js                      # ramp 10→50→100→500→1000 VUs (STAGES=small stops at 100)
docker compose run --rm k6 run /tests/stress-test.js                    # arrival rate up to 3000 req/s
docker compose run --rm k6 run /tests/spike-test.js
REQUESTS=50000 VUS=200 docker compose run --rm k6 run /tests/requests-test.js   # fixed request counts: 100 / 1000 / 10000 / 50000
```

k6 tags every request with `X-LoadTest` and `X-VU`; the load balancer derives the dashboard's **Load testing** metrics (virtual users, req/s, P95/P99, 2xx/4xx/5xx, per-backend distribution) from that real traffic. Tests run on one machine, so very large VU counts measure your laptop too.
Heavy login traffic is CPU-bound by bcrypt: each VU logs in once, but set `BCRYPT_COST=6` in `.env` for 1000-VU runs. Seed stock is 100 000 per product so tests do not sell out.

## Automated tests

```bash
cd load-balancer && go mod tidy && go test ./...          # algorithms, failover, retry, timeouts, rate limit, health, recovery, JWT
cd backend && go mod tidy && TEST_DATABASE_URL=postgres://shop:shop@localhost:5432/shop?sslmode=disable go test ./...
```
Backend tests are integration tests (registration/login/logout, auth failures, products, cart persistence, checkout, order creation, history, idempotency, concurrent retries, stock rollback); they skip without `TEST_DATABASE_URL`. LB tests need no database. Admin-access control is covered in the manual steps above (user cookie → 401 on `/_lb/api/*`).

## Behaviour notes

* A request is **failed** if it is 5xx, 429, or the LB could not reach a backend. 4xx (wrong password, 404) are correct answers and count as successful; they appear in the 4xx counters and are searchable with status `≥ 400`.
* Failure reasons: `NO_HEALTHY_BACKENDS`, `CONNECTION_REFUSED`, `CONNECTION_TIMEOUT`, `BACKEND_TIMEOUT`, `REQUEST_TIMEOUT`, `DATABASE_UNAVAILABLE`, `HTTP_500/502/503/504`, `RATE_LIMITED`, `REQUEST_TOO_LARGE`, `INVALID_REQUEST`, `CLIENT_CANCELED`.
* "Availability" = share of seconds with at least one healthy backend since the LB started. Counters/charts live in memory (24 h of history, lost on LB restart); request logs, health records and orders persist in PostgreSQL. If the log buffer overflows under extreme load, rows are dropped and counted (`dropped_log_rows`), never blocking traffic.
* Request logging keeps working while the DB is down; the LB keeps routing, and DB-backed admin pages return 503.
* Browsers now connect to the load balancer directly, so IP hash and rate limiting see the real client address (on Docker Desktop this is the Docker gateway address). `X-Forwarded-For` is only trusted from private/loopback peers (e.g. a TLS terminator in front); k6 sets it to simulate many clients.
* User sessions are stateless 24 h tokens: logout clears the cookie but cannot revoke a stolen token. Admin/session cookies need `COOKIE_SECURE=true` behind HTTPS.
* The dashboard's "run load tests" button is not included; tests are started with the k6 container (the Load testing page then shows them live).

## Troubleshooting

* `JWT_SECRET`/`ADMIN_PASSWORD` error on start → fill them in `.env`.
* Dashboard shows "Reconnecting…" → the load balancer container is down: `docker compose logs load-balancer`.
* Port already in use → change the left side of the `ports:` mappings in `docker-compose.yml`.
* Everything returns 503 right after start → wait a few seconds for the first health checks.
* First build downloads Go modules; it needs internet access.

## Future improvements

TLS termination, HA load balancer pair, persisted dashboard metrics (Prometheus + Grafana), sticky sessions by cookie, WebSocket proxying, circuit breaker, request-log retention policy, starting k6 from the dashboard.

package api_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"shop/backend/internal/api"
	"shop/backend/internal/database"
	"shop/backend/internal/middleware"
)

// Integration tests: they need a PostgreSQL database with the migrations applied.
//
//	TEST_DATABASE_URL=postgres://shop:shop@localhost:5432/shop?sslmode=disable go test ./...
func setup(t *testing.T) (*httptest.Server, *api.Server) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	db, err := database.Open(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := &api.Server{DB: db, Log: log, BackendID: "backend-test", JWTSecret: []byte("test-secret-test-secret"), BcryptCost: 4}
	ts := httptest.NewServer(middleware.Chain(s.Routes(), "backend-test", 1<<20, log))
	t.Cleanup(ts.Close)
	return ts, s
}

type client struct {
	t  *testing.T
	ts *httptest.Server
	c  *http.Client
}

func newClient(t *testing.T, ts *httptest.Server) *client {
	jar, _ := cookiejar.New(nil)
	return &client{t: t, ts: ts, c: &http.Client{Jar: jar}}
}

func (c *client) do(method, path string, body any, hdr map[string]string) (int, map[string]any, http.Header) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.ts.URL+path, rd)
	req.Header.Set("X-Requested-With", "test")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.c.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return resp.StatusCode, m, resp.Header
}

func register(t *testing.T, ts *httptest.Server) (*client, string) {
	c := newClient(t, ts)
	email := fmt.Sprintf("u%d@example.com", time.Now().UnixNano())
	code, _, _ := c.do("POST", "/api/auth/register", map[string]string{"name": "Test User", "email": email, "password": "password123"}, nil)
	if code != 201 {
		t.Fatalf("register: %d", code)
	}
	return c, email
}

func TestHealth(t *testing.T) {
	ts, _ := setup(t)
	c := newClient(t, ts)
	if code, _, _ := c.do("GET", "/health", nil, nil); code != 200 {
		t.Fatalf("health %d", code)
	}
}

func TestRegisterLoginLogoutAndPasswordHashing(t *testing.T) {
	ts, s := setup(t)
	c, email := register(t, ts)
	var hash string
	if err := s.DB.QueryRow(`SELECT password_hash FROM users WHERE email=$1`, email).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if hash == "password123" || len(hash) < 50 || hash[:4] != "$2a$" {
		t.Fatalf("password is not bcrypt hashed: %q", hash)
	}
	if code, _, _ := c.do("POST", "/api/auth/register", map[string]string{"name": "X", "email": email, "password": "password123"}, nil); code != 409 {
		t.Fatalf("duplicate email must be 409, got %d", code)
	}
	if code, m, _ := c.do("GET", "/api/auth/me", nil, nil); code != 200 || m["email"] != email {
		t.Fatalf("me: %d %v", code, m)
	}
	c.do("POST", "/api/auth/logout", nil, nil)
	if code, _, _ := c.do("GET", "/api/auth/me", nil, nil); code != 401 {
		t.Fatalf("after logout expected 401, got %d", code)
	}
	if code, _, _ := c.do("POST", "/api/auth/login", map[string]string{"email": email, "password": "wrong-password"}, nil); code != 401 {
		t.Fatalf("bad password must be 401, got %d", code)
	}
	code, _, h := c.do("POST", "/api/auth/login", map[string]string{"email": email, "password": "password123"}, nil)
	if code != 200 || h.Get("X-Meta-User-Id") == "" {
		t.Fatalf("login: %d %v", code, h)
	}
}

func TestValidationAndCSRF(t *testing.T) {
	ts, _ := setup(t)
	c := newClient(t, ts)
	bad := []map[string]string{
		{"name": "", "email": "a@b.co", "password": "password123"},
		{"name": "A", "email": "not-an-email", "password": "password123"},
		{"name": "A", "email": "a@b.co", "password": "short"},
	}
	for _, b := range bad {
		if code, _, _ := c.do("POST", "/api/auth/register", b, nil); code != 400 {
			t.Fatalf("expected 400 for %v, got %d", b, code)
		}
	}
	req, _ := http.NewRequest("POST", ts.URL+"/api/auth/login", bytes.NewReader([]byte(`{}`))) // no X-Requested-With
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 403 {
		t.Fatalf("missing CSRF header must be 403, got %d", resp.StatusCode)
	}
	if code, _, _ := c.do("GET", "/api/products?q=%27%3B+DROP+TABLE+users%3B--", nil, nil); code != 200 {
		t.Fatalf("SQL-injection style input must be handled safely, got %d", code)
	}
}

func TestUnauthorizedAccess(t *testing.T) {
	ts, _ := setup(t)
	c := newClient(t, ts)
	for _, p := range []string{"/api/cart", "/api/orders", "/api/profile"} {
		if code, _, _ := c.do("GET", p, nil, nil); code != 401 {
			t.Fatalf("%s expected 401, got %d", p, code)
		}
	}
	if code, _, _ := c.do("POST", "/api/orders", map[string]any{}, map[string]string{"Idempotency-Key": "k"}); code != 401 {
		t.Fatalf("order without login must be 401, got %d", code)
	}
}

func TestProducts(t *testing.T) {
	ts, _ := setup(t)
	c := newClient(t, ts)
	req, _ := http.NewRequest("GET", ts.URL+"/api/products", nil)
	resp, _ := c.c.Do(req)
	var list []map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&list)
	if len(list) < 8 {
		t.Fatalf("expected seeded products, got %d", len(list))
	}
	if code, _, h := c.do("GET", "/api/products/1", nil, nil); code != 200 || h.Get("X-Meta-Product") == "" {
		t.Fatalf("product detail: %d", code)
	}
	if code, _, _ := c.do("GET", "/api/products/999999", nil, nil); code != 404 {
		t.Fatalf("missing product must be 404, got %d", code)
	}
}

func TestCartLifecycleAndPersistence(t *testing.T) {
	ts, s := setup(t)
	c, _ := register(t, ts)
	if code, _, h := c.do("POST", "/api/cart", map[string]any{"product_id": 1, "quantity": 1}, nil); code != 201 || h.Get("X-Meta-Product") == "" {
		t.Fatalf("add: %d", code)
	}
	c.do("POST", "/api/cart", map[string]any{"product_id": 1, "quantity": 2}, nil)
	_, m, _ := c.do("GET", "/api/cart", nil, nil)
	if m["count"].(float64) != 3 {
		t.Fatalf("expected quantity 3, got %v", m["count"])
	}
	if code, m, _ := c.do("PUT", "/api/cart/items/1", map[string]int{"quantity": 5}, nil); code != 200 || m["count"].(float64) != 5 {
		t.Fatalf("increase: %d %v", code, m)
	}
	c.do("PUT", "/api/cart/items/1", map[string]int{"quantity": 4}, nil) // decrease
	c.do("POST", "/api/cart", map[string]any{"product_id": 2, "quantity": 1}, nil)
	var n int
	_ = s.DB.QueryRow(`SELECT count(*) FROM cart_items ci JOIN cart c ON c.id=ci.cart_id`).Scan(&n)
	if n < 2 {
		t.Fatal("cart items must be persisted in the database")
	}
	if code, m, _ := c.do("DELETE", "/api/cart/items/2", nil, nil); code != 200 || m["count"].(float64) != 4 {
		t.Fatalf("remove: %d %v", code, m)
	}
	if code, _, _ := c.do("POST", "/api/cart", map[string]any{"product_id": 999999}, nil); code != 404 {
		t.Fatalf("unknown product must be 404, got %d", code)
	}
	c.do("DELETE", "/api/cart", nil, nil)
	_, m, _ = c.do("GET", "/api/cart", nil, nil)
	if m["count"].(float64) != 0 {
		t.Fatal("cart should be empty after clear")
	}
}

func TestCheckoutOrderAndHistory(t *testing.T) {
	ts, s := setup(t)
	c, _ := register(t, ts)
	if code, _, _ := c.do("POST", "/api/checkout", nil, nil); code != 400 {
		t.Fatalf("empty checkout must be 400, got %d", code)
	}
	c.do("POST", "/api/cart", map[string]any{"product_id": 1, "quantity": 1}, nil)
	c.do("POST", "/api/cart", map[string]any{"product_id": 5, "quantity": 2}, nil)
	if code, m, _ := c.do("POST", "/api/checkout", nil, nil); code != 200 || m["total"].(float64) != 65000+2*1299 {
		t.Fatalf("checkout: %d %v", code, m)
	}
	code, order, h := c.do("POST", "/api/orders", map[string]string{"address": "1 Test Street"}, map[string]string{"Idempotency-Key": "key-" + fmt.Sprint(time.Now().UnixNano()), "X-Request-ID": "REQ-TEST1234"})
	if code != 201 || h.Get("X-Meta-Order-Id") == "" {
		t.Fatalf("order: %d %v", code, order)
	}
	if order["request_id"] != "REQ-TEST1234" || order["backend_id"] != "backend-test" {
		t.Fatalf("request id / backend not stored: %v", order)
	}
	var items int
	_ = s.DB.QueryRow(`SELECT count(*) FROM order_items oi JOIN orders o ON o.id=oi.order_id WHERE o.order_number=$1`, order["id"]).Scan(&items)
	if items != 2 {
		t.Fatalf("expected 2 order items, got %d", items)
	}
	_, m, _ := c.do("GET", "/api/cart", nil, nil)
	if m["count"].(float64) != 0 {
		t.Fatal("cart must be cleared by the order")
	}
	if code, _, _ := c.do("GET", "/api/orders/"+order["id"].(string), nil, nil); code != 200 {
		t.Fatalf("order details: %d", code)
	}
	other, _ := register(t, ts)
	if code, _, _ := other.do("GET", "/api/orders/"+order["id"].(string), nil, nil); code != 404 {
		t.Fatalf("another user must not see the order, got %d", code)
	}
}

func TestOrderIdempotency(t *testing.T) {
	ts, s := setup(t)
	c, email := register(t, ts)
	c.do("POST", "/api/cart", map[string]any{"product_id": 1, "quantity": 1}, nil)
	hdr := map[string]string{"Idempotency-Key": "same-key-123"}
	code1, o1, _ := c.do("POST", "/api/orders", map[string]string{}, hdr)
	code2, o2, h2 := c.do("POST", "/api/orders", map[string]string{}, hdr)
	if code1 != 201 || code2 != 200 || o1["id"] != o2["id"] || h2.Get("Idempotent-Replay") != "true" {
		t.Fatalf("retry must replay the first order: %d/%d %v %v", code1, code2, o1["id"], o2["id"])
	}
	var n int
	_ = s.DB.QueryRow(`SELECT count(*) FROM orders o JOIN users u ON u.id=o.user_id WHERE u.email=$1`, email).Scan(&n)
	if n != 1 {
		t.Fatalf("expected exactly one order, got %d", n)
	}
	if code, _, _ := c.do("POST", "/api/orders", map[string]string{}, nil); code != 400 {
		t.Fatalf("missing Idempotency-Key must be 400, got %d", code)
	}
}

func TestConcurrentRetriesCreateOneOrder(t *testing.T) {
	ts, s := setup(t)
	c, email := register(t, ts)
	c.do("POST", "/api/cart", map[string]any{"product_id": 3, "quantity": 1}, nil)
	var wg sync.WaitGroup
	codes := make(chan int, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, _, _ := c.do("POST", "/api/orders", map[string]any{"items": []map[string]int{{"product_id": 3, "quantity": 1}}}, map[string]string{"Idempotency-Key": "race-key"})
			codes <- code
		}()
	}
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != 200 && code != 201 {
			t.Fatalf("unexpected status %d", code)
		}
	}
	var n int
	_ = s.DB.QueryRow(`SELECT count(*) FROM orders o JOIN users u ON u.id=o.user_id WHERE u.email=$1`, email).Scan(&n)
	if n != 1 {
		t.Fatalf("10 concurrent retries created %d orders, want 1", n)
	}
}

func TestInsufficientStockRollsBack(t *testing.T) {
	ts, s := setup(t)
	c, email := register(t, ts)
	var stock int
	_ = s.DB.QueryRow(`SELECT stock FROM products WHERE id=2`).Scan(&stock)
	body := map[string]any{"items": []map[string]int{{"product_id": 1, "quantity": 1}, {"product_id": 2, "quantity": 99}}}
	_, _, _ = c.do("POST", "/api/orders", body, map[string]string{"Idempotency-Key": "stock-ok"})
	var after int
	_ = s.DB.QueryRow(`SELECT stock FROM products WHERE id=2`).Scan(&after)
	if after != stock-99 {
		t.Fatalf("stock should drop by 99, %d -> %d", stock, after)
	}
	_, _ = s.DB.Exec(`UPDATE products SET stock=5 WHERE id=4`)
	var before1 int
	_ = s.DB.QueryRow(`SELECT stock FROM products WHERE id=1`).Scan(&before1)
	body = map[string]any{"items": []map[string]int{{"product_id": 1, "quantity": 1}, {"product_id": 4, "quantity": 6}}}
	code, _, _ := c.do("POST", "/api/orders", body, map[string]string{"Idempotency-Key": "stock-fail"})
	if code != 409 {
		t.Fatalf("expected 409, got %d", code)
	}
	var after1, n int
	_ = s.DB.QueryRow(`SELECT stock FROM products WHERE id=1`).Scan(&after1)
	_ = s.DB.QueryRow(`SELECT count(*) FROM orders o JOIN users u ON u.id=o.user_id WHERE u.email=$1 AND o.idempotency_key='stock-fail'`, email).Scan(&n)
	if after1 != before1 || n != 0 {
		t.Fatalf("failed order must leave no trace (stock %d->%d, orders %d)", before1, after1, n)
	}
	_, _ = s.DB.Exec(`UPDATE products SET stock=100000 WHERE id=4`)
}

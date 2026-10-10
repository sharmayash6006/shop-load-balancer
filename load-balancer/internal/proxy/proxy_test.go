package proxy

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lb/internal/algorithms"
	"lb/internal/auth"
	"lb/internal/config"
	"lb/internal/health"
	"lb/internal/metrics"
	"lb/internal/routing"
)

const secret = "test-secret-test-secret"

type rig struct {
	px   *Proxy
	pool *routing.Pool
	m    *metrics.Metrics
	mon  *health.Monitor
	cfg  *config.Config
}

func newRig(t *testing.T, urls []string, mut func(*config.Settings)) *rig {
	t.Helper()
	cfg := &config.Config{JWTSecret: secret, MaxBodyBytes: 1 << 20}
	cfg.Update(func(s *config.Settings) {
		*s = config.Settings{Algorithm: algorithms.RoundRobin, ConnectTimeoutMs: 500, RequestTimeoutMs: 5000, BackendTimeoutMs: 2000,
			RetryCount: 2, HealthIntervalMs: 100, HealthTimeoutMs: 500, HealthFall: 2, HealthRise: 2}
		if mut != nil {
			mut(s)
		}
	})
	pool, err := routing.NewPool(urls, nil)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := metrics.New(pool)
	mon := health.New(pool, cfg, log, nil)
	mon.CheckAll()
	return &rig{px: New(pool, cfg, m, mon, log), pool: pool, m: m, mon: mon, cfg: cfg}
}

func (r *rig) do(method, path string, hdr map[string]string, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = "198.51.100.9:5555"
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.px.ServeHTTP(w, req)
	return w
}

func okServer(id string, hits *atomic.Int64) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(200)
			return
		}
		if hits != nil {
			hits.Add(1)
		}
		w.Header().Set("X-Meta-Product", "Laptop")
		w.Header().Set("X-Meta-Order-ID", "ORD-1")
		w.WriteHeader(201)
		io.WriteString(w, id)
	}))
}

func TestDistributesAcrossBackends(t *testing.T) {
	var h1, h2, h3 atomic.Int64
	s1, s2, s3 := okServer("1", &h1), okServer("2", &h2), okServer("3", &h3)
	defer s1.Close()
	defer s2.Close()
	defer s3.Close()
	r := newRig(t, []string{s1.URL, s2.URL, s3.URL}, nil)
	for i := 0; i < 30; i++ {
		if w := r.do("GET", "/api/products", nil, ""); w.Code != 201 {
			t.Fatalf("status %d", w.Code)
		}
	}
	if h1.Load() != 10 || h2.Load() != 10 || h3.Load() != 10 {
		t.Fatalf("uneven: %d %d %d", h1.Load(), h2.Load(), h3.Load())
	}
}

func TestMetaHeadersHarvestedAndStripped(t *testing.T) {
	s := okServer("1", nil)
	defer s.Close()
	r := newRig(t, []string{s.URL}, nil)
	tok := auth.Sign(auth.Claims{Sub: 7, Name: "Yash", Exp: time.Now().Add(time.Hour).Unix()}, []byte(secret))
	w := r.do("POST", "/api/orders", map[string]string{"Cookie": "token=" + tok, "Idempotency-Key": "k"}, `{}`)
	if w.Header().Get("X-Meta-Order-Id") != "" || w.Header().Get("X-Request-ID") == "" || w.Header().Get("X-Served-By") != "backend-1" {
		t.Fatalf("bad headers: %v", w.Header())
	}
	e := r.m.Latest(1)[0]
	if e.UserID != 7 || e.UserName != "Yash" || e.OrderID != "ORD-1" || e.Action != "CREATE_ORDER" || !strings.Contains(e.Detail, "product=Laptop") {
		t.Fatalf("bad entry: %+v", e)
	}
}

func TestFailoverWhenBackendDies(t *testing.T) {
	var h1 atomic.Int64
	s1, s2 := okServer("1", &h1), okServer("2", nil)
	defer s1.Close()
	r := newRig(t, []string{s1.URL, s2.URL}, nil)
	s2.Close() // backend-2 crashes
	failed := 0
	for i := 0; i < 20; i++ {
		if w := r.do("GET", "/api/products", nil, ""); w.Code >= 500 {
			failed++
		}
	}
	if failed != 0 {
		t.Fatalf("%d requests failed; retries should have hidden the dead backend", failed)
	}
	if got := r.pool.Get("backend-2").State(); got != routing.Unhealthy {
		t.Fatalf("backend-2 should be UNHEALTHY (passive check), got %s", got)
	}
	if h1.Load() != 20 {
		t.Fatalf("all traffic should reach backend-1, got %d", h1.Load())
	}
}

func TestAllBackendsDownReturns503(t *testing.T) {
	s := okServer("1", nil)
	r := newRig(t, []string{s.URL}, nil)
	s.Close()
	r.do("GET", "/x", nil, "") // discovers the failure
	w := r.do("GET", "/x", nil, "")
	if w.Code != 503 {
		t.Fatalf("expected 503, got %d", w.Code)
	}
	if e := r.m.Latest(1)[0]; e.FailureReason != "NO_HEALTHY_BACKENDS" || e.Success {
		t.Fatalf("failure reason not recorded: %+v", e)
	}
}

func TestDisabledBackendGetsNoTraffic(t *testing.T) {
	var h1, h2 atomic.Int64
	s1, s2 := okServer("1", &h1), okServer("2", &h2)
	defer s1.Close()
	defer s2.Close()
	r := newRig(t, []string{s1.URL, s2.URL}, nil)
	r.pool.Get("backend-2").SetEnabled(false)
	for i := 0; i < 10; i++ {
		r.do("GET", "/x", nil, "")
	}
	if h2.Load() != 0 || h1.Load() != 10 {
		t.Fatalf("disabled backend received traffic: %d / %d", h1.Load(), h2.Load())
	}
}

func slowServer(d time.Duration) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			return
		}
		time.Sleep(d)
	}))
}

func TestTimeoutRetriesIdempotentRequests(t *testing.T) {
	slow, fast := slowServer(1500*time.Millisecond), okServer("fast", nil)
	defer slow.Close()
	defer fast.Close()
	r := newRig(t, []string{slow.URL, fast.URL}, func(s *config.Settings) { s.BackendTimeoutMs = 150 })
	w := r.do("GET", "/x", nil, "")
	if w.Code != 201 {
		t.Fatalf("GET should be retried on the fast backend, got %d", w.Code)
	}
	if e := r.m.Latest(1)[0]; e.Retries != 1 || e.BackendID != "backend-2" {
		t.Fatalf("retry not recorded: %+v", e)
	}
}

func TestNonIdempotentPostNotRetriedAfterTimeout(t *testing.T) {
	slow, fast := slowServer(1500*time.Millisecond), okServer("fast", nil)
	defer slow.Close()
	defer fast.Close()
	r := newRig(t, []string{slow.URL, fast.URL}, func(s *config.Settings) { s.BackendTimeoutMs = 150 })
	w := r.do("POST", "/api/cart", nil, `{"product_id":1}`) // no Idempotency-Key
	if w.Code != 504 {
		t.Fatalf("expected 504 without retry, got %d", w.Code)
	}
	if e := r.m.Latest(1)[0]; e.FailureReason != "BACKEND_TIMEOUT" {
		t.Fatalf("reason: %+v", e)
	}
}

func TestOrderPostWithIdempotencyKeyIsRetried(t *testing.T) {
	slow, fast := slowServer(1500*time.Millisecond), okServer("fast", nil)
	defer slow.Close()
	defer fast.Close()
	r := newRig(t, []string{slow.URL, fast.URL}, func(s *config.Settings) { s.BackendTimeoutMs = 150 })
	w := r.do("POST", "/api/orders", map[string]string{"Idempotency-Key": "abc"}, `{}`)
	if w.Code != 201 {
		t.Fatalf("expected retry to succeed, got %d", w.Code)
	}
}

func TestRateLimiting(t *testing.T) {
	s := okServer("1", nil)
	defer s.Close()
	r := newRig(t, []string{s.URL}, func(s *config.Settings) { s.RateLimit = 5 })
	limited := 0
	for i := 0; i < 20; i++ {
		if r.do("GET", "/x", nil, "").Code == 429 {
			limited++
		}
	}
	if limited < 10 {
		t.Fatalf("expected most requests to be limited, got %d", limited)
	}
	if e := r.m.Latest(1)[0]; e.FailureReason != "RATE_LIMITED" {
		t.Fatalf("429 not recorded as rate limited: %+v", e)
	}
	if r.m.Stats().RateLimited == 0 {
		t.Fatal("rate limited counter not updated")
	}
}

func TestHealthCheckFailureAndRecovery(t *testing.T) {
	var up atomic.Bool
	up.Store(true)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !up.Load() {
			w.WriteHeader(503)
		}
	}))
	defer s.Close()
	r := newRig(t, []string{s.URL}, nil)
	b := r.pool.Get("backend-1")
	if b.State() != routing.Healthy {
		t.Fatalf("expected HEALTHY, got %s", b.State())
	}
	up.Store(false)
	r.mon.CheckAll()
	r.mon.CheckAll() // fall = 2
	if b.State() != routing.Unhealthy {
		t.Fatalf("expected UNHEALTHY, got %s", b.State())
	}
	up.Store(true)
	r.mon.CheckAll()
	if b.State() == routing.Healthy {
		t.Fatal("rise=2: one success must not be enough")
	}
	r.mon.CheckAll()
	if b.State() != routing.Healthy {
		t.Fatalf("expected HEALTHY again, got %s", b.State())
	}
	if n := len(r.mon.Events(10)); n < 2 {
		t.Fatalf("transitions not recorded: %d", n)
	}
}

func TestEnableGoesThroughHealthCheck(t *testing.T) {
	s := okServer("1", nil)
	defer s.Close()
	r := newRig(t, []string{s.URL}, nil)
	b := r.pool.Get("backend-1")
	b.SetEnabled(false)
	if b.Available() {
		t.Fatal("disabled backend must not be available")
	}
	b.SetEnabled(true)
	if b.Available() || b.State() != routing.Recovering {
		t.Fatalf("enabled backend must wait for a health check, state=%s", b.State())
	}
	r.mon.CheckAll()
	if !b.Available() {
		t.Fatal("backend should be HEALTHY after the check")
	}
}

func TestRequestTooLarge(t *testing.T) {
	s := okServer("1", nil)
	defer s.Close()
	r := newRig(t, []string{s.URL}, nil)
	r.px.MaxBody = 10
	if w := r.do("POST", "/api/cart", nil, strings.Repeat("a", 100)); w.Code != 413 {
		t.Fatalf("expected 413, got %d", w.Code)
	}
}

func TestClassify(t *testing.T) {
	cases := map[[2]string]string{
		{"POST", "/api/orders"}: "CREATE_ORDER", {"GET", "/api/orders/ORD-1"}: "VIEW_ORDER", {"POST", "/api/cart"}: "ADD_TO_CART",
		{"GET", "/api/products/3"}: "VIEW_PRODUCT", {"POST", "/api/auth/login"}: "LOGIN", {"DELETE", "/api/cart/items/2"}: "REMOVE_FROM_CART",
	}
	for in, want := range cases {
		if got := Classify(in[0], in[1]); got != want {
			t.Errorf("%v: got %s want %s", in, got, want)
		}
	}
}

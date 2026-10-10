package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRateLimiterBurstThenBlocks(t *testing.T) {
	rl := NewRateLimiter()
	allowed := 0
	for i := 0; i < 50; i++ {
		if rl.Allow("1.2.3.4", 10) {
			allowed++
		}
	}
	if allowed < 10 || allowed > 12 {
		t.Fatalf("burst of ~10 expected, got %d", allowed)
	}
	if !rl.Allow("5.6.7.8", 10) {
		t.Fatal("limit is per IP")
	}
	if !rl.Allow("1.2.3.4", 0) {
		t.Fatal("rate 0 disables limiting")
	}
}

func TestClientIPTrustsForwardedOnlyFromPrivatePeers(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "172.18.0.5:1234"
	r.Header.Set("X-Forwarded-For", "203.0.113.9")
	if got := ClientIP(r); got != "203.0.113.9" {
		t.Fatalf("got %s", got)
	}
	r.RemoteAddr = "198.51.100.1:1234" // public peer: header must be ignored
	if got := ClientIP(r); got != "198.51.100.1" {
		t.Fatalf("spoofed header trusted: %s", got)
	}
}

func TestCORSAllowsOnlyListedOriginsWithCredentials(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	h := CORS([]string{"http://localhost:3000", "http://localhost:3001/"}, next)

	pre := httptest.NewRequest("OPTIONS", "/api/orders", nil)
	pre.Header.Set("Origin", "http://localhost:3000")
	pre.Header.Set("Access-Control-Request-Method", "POST")
	pre.Header.Set("Access-Control-Request-Headers", "idempotency-key,x-requested-with,content-type")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, pre)
	if w.Code != 204 || w.Header().Get("Access-Control-Allow-Origin") != "http://localhost:3000" || w.Header().Get("Access-Control-Allow-Credentials") != "true" {
		t.Fatalf("preflight not allowed: %d %v", w.Code, w.Header())
	}
	if !strings.Contains(strings.ToLower(w.Header().Get("Access-Control-Allow-Headers")), "idempotency-key") {
		t.Fatal("Idempotency-Key must be allowed")
	}

	bad := httptest.NewRequest("GET", "/api/products", nil)
	bad.Header.Set("Origin", "https://evil.example")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, bad)
	if w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("unlisted origin must not get CORS headers")
	}
	badPre := httptest.NewRequest("OPTIONS", "/api/orders", nil)
	badPre.Header.Set("Origin", "https://evil.example")
	badPre.Header.Set("Access-Control-Request-Method", "POST")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, badPre)
	if w.Code != 403 || w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("preflight from unlisted origin must be rejected: %d", w.Code)
	}

	ok := httptest.NewRequest("GET", "/api/products", nil)
	ok.Header.Set("Origin", "http://localhost:3001")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, ok)
	if w.Header().Get("Access-Control-Allow-Origin") != "http://localhost:3001" || w.Header().Get("Access-Control-Allow-Origin") == "*" {
		t.Fatalf("trailing slash origin config / echo failed: %v", w.Header())
	}
	if !strings.Contains(w.Header().Get("Access-Control-Expose-Headers"), "X-Served-By") {
		t.Fatal("X-Served-By must be exposed to the browser")
	}
}

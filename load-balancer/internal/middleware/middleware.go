// Package middleware holds the cross-cutting HTTP pieces of the load balancer.
package middleware

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ---- client IP ----

// ClientIP returns the caller's IP. X-Forwarded-For / X-Real-IP are only trusted when the direct peer is a
// loopback/private address (i.e. our own frontend proxies or Docker network), so internet clients cannot spoof it.
func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip != nil && (ip.IsLoopback() || ip.IsPrivate()) {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			first := strings.TrimSpace(strings.Split(xff, ",")[0])
			if net.ParseIP(first) != nil {
				return first
			}
		}
		if xr := strings.TrimSpace(r.Header.Get("X-Real-IP")); net.ParseIP(xr) != nil {
			return xr
		}
	}
	return host
}

// ---- security headers ----

func SecureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

// ---- CORS ----

// CORS lets the separately hosted shopping website and admin dashboard call the load balancer from the browser.
// Only origins listed in CORS_ORIGINS are allowed; the allowed origin is echoed back (never "*"), which is
// required when credentials (the session cookie) are used. Preflights are answered here and never proxied.
func CORS(origins []string, next http.Handler) http.Handler {
	allowed := map[string]bool{}
	for _, o := range origins {
		allowed[strings.TrimRight(o, "/")] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o := r.Header.Get("Origin")
		if o == "" {
			next.ServeHTTP(w, r)
			return
		}
		h := w.Header()
		h.Add("Vary", "Origin")
		ok := allowed[o]
		if ok {
			h.Set("Access-Control-Allow-Origin", o)
			h.Set("Access-Control-Allow-Credentials", "true")
			h.Set("Access-Control-Expose-Headers", "X-Request-ID, X-Served-By, Idempotent-Replay, Retry-After")
		}
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			if ok {
				h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
				h.Set("Access-Control-Allow-Headers", "Content-Type, Idempotency-Key, X-Requested-With, X-Request-ID, X-LoadTest, X-VU")
				h.Set("Access-Control-Max-Age", "600")
				w.WriteHeader(http.StatusNoContent)
			} else {
				w.WriteHeader(http.StatusForbidden) // origin not allowed: no CORS headers, browser blocks it
			}
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---- per-IP token-bucket rate limiter ----

type bucket struct {
	tokens float64
	last   time.Time
}

type RateLimiter struct {
	mu sync.Mutex
	m  map[string]*bucket
}

func NewRateLimiter() *RateLimiter {
	rl := &RateLimiter{m: map[string]*bucket{}}
	go func() {
		for range time.Tick(time.Minute) {
			rl.mu.Lock()
			for k, b := range rl.m {
				if time.Since(b.last) > time.Minute {
					delete(rl.m, k)
				}
			}
			rl.mu.Unlock()
		}
	}()
	return rl
}

// Allow reports whether ip may make a request now, given a sustained rate (req/s, burst = rate). rate<=0 disables limiting.
func (rl *RateLimiter) Allow(ip string, rate float64) bool {
	if rate <= 0 {
		return true
	}
	burst := rate
	if burst < 1 {
		burst = 1
	}
	now := time.Now()
	rl.mu.Lock()
	defer rl.mu.Unlock()
	b, ok := rl.m[ip]
	if !ok {
		b = &bucket{tokens: burst, last: now}
		rl.m[ip] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * rate
	if b.tokens > burst {
		b.tokens = burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

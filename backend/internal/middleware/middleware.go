package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"
)

type ctxKey int

const RequestIDKey ctxKey = 1

func RequestID(ctx context.Context) string {
	s, _ := ctx.Value(RequestIDKey).(string)
	return s
}

func newID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "REQ-" + strings.ToUpper(hex.EncodeToString(b))
}

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *statusWriter) WriteHeader(c int) { w.status = c; w.ResponseWriter.WriteHeader(c) }
func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

// Chain applies: recover -> request id + access log -> security headers -> body limit -> CSRF guard.
func Chain(h http.Handler, backendID string, maxBody int64, log *slog.Logger) http.Handler {
	return recoverer(log, accessLog(log, backendID, secureHeaders(bodyLimit(maxBody, csrfGuard(h)))))
}

func recoverer(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Error("panic", "panic", rec, "stack", string(debug.Stack()))
				http.Error(w, `{"error":"internal error"}`, 500)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func accessLog(log *slog.Logger, backendID string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		id := r.Header.Get("X-Request-ID")
		if id == "" || len(id) > 64 {
			id = newID()
		}
		w.Header().Set("X-Request-ID", id)
		w.Header().Set("X-Backend-ID", backendID)
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r.WithContext(context.WithValue(r.Context(), RequestIDKey, id)))
		if sw.status == 0 {
			sw.status = 200
		}
		if r.URL.Path == "/health" || r.URL.Path == "/ready" {
			return
		}
		log.Info("request", "request_id", id, "backend", backendID, "method", r.Method, "path", r.URL.Path,
			"status", sw.status, "bytes", sw.bytes, "latency_ms", float64(time.Since(start).Microseconds())/1000)
	})
}

func secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func bodyLimit(max int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, max)
		}
		next.ServeHTTP(w, r)
	})
}

// csrfGuard: state-changing requests must carry a custom header. Browsers cannot add a
// custom header on a cross-site request without a CORS preflight (which we never allow),
// and the session cookie is SameSite=Lax, so cross-site form posts cannot authenticate.
func csrfGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if r.Header.Get("X-Requested-With") == "" && r.Header.Get("Authorization") == "" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":"missing X-Requested-With header (CSRF protection)"}` + "\n"))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

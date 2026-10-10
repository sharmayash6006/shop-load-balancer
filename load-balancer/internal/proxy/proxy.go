// Package proxy is the Layer 7 reverse proxy: it parses the HTTP request, identifies the user, picks a
// backend with the configured algorithm, retries safely, and records everything it saw.
package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"

	"lb/internal/algorithms"
	"lb/internal/auth"
	"lb/internal/config"
	"lb/internal/health"
	"lb/internal/metrics"
	"lb/internal/middleware"
	"lb/internal/routing"
)

type Proxy struct {
	Pool      *routing.Pool
	Algos     *algorithms.Registry
	Cfg       *config.Config
	M         *metrics.Metrics
	Mon       *health.Monitor
	Limiter   *middleware.RateLimiter
	JWTSecret []byte
	MaxBody   int64
	Log       *slog.Logger
	tr        *http.Transport
}

func New(p *routing.Pool, c *config.Config, m *metrics.Metrics, mon *health.Monitor, log *slog.Logger) *Proxy {
	px := &Proxy{Pool: p, Algos: algorithms.NewRegistry(), Cfg: c, M: m, Mon: mon, Limiter: middleware.NewRateLimiter(),
		JWTSecret: []byte(c.JWTSecret), MaxBody: c.MaxBodyBytes, Log: log}
	px.tr = &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			d := net.Dialer{Timeout: time.Duration(c.Get().ConnectTimeoutMs) * time.Millisecond, KeepAlive: 30 * time.Second}
			return d.DialContext(ctx, network, addr)
		},
		MaxIdleConns: 1024, MaxIdleConnsPerHost: 256, IdleConnTimeout: 90 * time.Second, DisableCompression: true,
	}
	return px
}

var hopByHop = map[string]bool{"Connection": true, "Proxy-Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true,
	"Proxy-Authorization": true, "Te": true, "Trailer": true, "Transfer-Encoding": true, "Upgrade": true}

func newReqID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "REQ-" + strings.ToUpper(hex.EncodeToString(b))
}

// Classify maps an HTTP request to a business action shown in the dashboard.
func Classify(method, path string) string {
	switch {
	case path == "/api/auth/register":
		return "REGISTER"
	case path == "/api/auth/login":
		return "LOGIN"
	case path == "/api/auth/logout":
		return "LOGOUT"
	case path == "/api/auth/me" || path == "/api/profile":
		if method == http.MethodPut {
			return "UPDATE_PROFILE"
		}
		return "VIEW_PROFILE"
	case path == "/api/products":
		return "VIEW_PRODUCTS"
	case strings.HasPrefix(path, "/api/products/"):
		return "VIEW_PRODUCT"
	case path == "/api/cart":
		switch method {
		case http.MethodPost:
			return "ADD_TO_CART"
		case http.MethodDelete:
			return "CLEAR_CART"
		}
		return "VIEW_CART"
	case strings.HasPrefix(path, "/api/cart/items/"):
		if method == http.MethodDelete {
			return "REMOVE_FROM_CART"
		}
		return "UPDATE_CART_QTY"
	case path == "/api/checkout":
		return "CHECKOUT"
	case path == "/api/orders":
		if method == http.MethodPost {
			return "CREATE_ORDER"
		}
		return "VIEW_ORDERS"
	case strings.HasPrefix(path, "/api/orders/"):
		return "VIEW_ORDER"
	}
	return "OTHER"
}

func (p *Proxy) identify(r *http.Request) (int64, string) {
	tok := ""
	if c, err := r.Cookie("token"); err == nil {
		tok = c.Value
	} else if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		tok = strings.TrimPrefix(h, "Bearer ")
	}
	if tok == "" {
		return 0, ""
	}
	c, err := auth.Verify(tok, p.JWTSecret)
	if err != nil {
		return 0, ""
	}
	return c.Sub, c.Name
}

// retryable: may this request be sent to another backend after a failure that may have reached the server?
// POST is only retried when it carries an Idempotency-Key (order creation), so retries can never duplicate an order.
func retryable(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPut, http.MethodDelete:
		return true
	}
	return r.Header.Get("Idempotency-Key") != ""
}

type failure struct {
	reason    string
	status    int
	connLevel bool // the request never reached an application: always safe to retry
}

func classifyErr(err error, r *http.Request, ctx context.Context) failure {
	if r.Context().Err() != nil {
		return failure{"CLIENT_CANCELED", 499, false}
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return failure{"REQUEST_TIMEOUT", 504, false}
	}
	var oe *net.OpError
	if errors.As(err, &oe) && oe.Op == "dial" {
		switch {
		case errors.Is(err, syscall.ECONNREFUSED):
			return failure{"CONNECTION_REFUSED", 502, true}
		case oe.Timeout():
			return failure{"CONNECTION_TIMEOUT", 504, true}
		}
		return failure{"BACKEND_UNAVAILABLE", 502, true}
	}
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return failure{"BACKEND_TIMEOUT", 504, false}
	}
	return failure{"BACKEND_ERROR", 502, false}
}

type countWriter struct {
	w http.ResponseWriter
	n int64
}

func (c *countWriter) Write(b []byte) (int, error) {
	n, err := c.w.Write(b)
	c.n += int64(n)
	return n, err
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	p.M.Begin()
	defer p.M.End()

	ip := middleware.ClientIP(r)
	e := metrics.Entry{Time: start, RequestID: newReqID(), IP: ip, Method: r.Method, Path: r.URL.Path,
		Action: Classify(r.Method, r.URL.Path), LoadTest: r.Header.Get("X-LoadTest")}
	if len(e.LoadTest) > 40 {
		e.LoadTest = e.LoadTest[:40]
	}
	e.VU, _ = strconv.Atoi(r.Header.Get("X-VU"))
	e.UserID, e.UserName = p.identify(r)
	w.Header().Set("X-Request-ID", e.RequestID)

	finish := func() {
		e.LatencyMs = float64(time.Since(start).Microseconds()) / 1000
		e.Success = e.Status < 500 && e.Status != 429 && e.FailureReason == ""
		p.M.Record(e)
		p.Log.Info("request", "request_id", e.RequestID, "user_id", e.UserID, "action", e.Action, "method", e.Method, "path", e.Path,
			"backend", e.BackendID, "status", e.Status, "latency_ms", e.LatencyMs, "retries", e.Retries, "failure", e.FailureReason)
	}
	reject := func(status int, reason, msg string) {
		e.Status, e.FailureReason = status, reason
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": msg, "reason": reason, "request_id": e.RequestID})
		finish()
	}

	cfg := p.Cfg.Get()
	if !p.Limiter.Allow(ip, cfg.RateLimit) {
		w.Header().Set("Retry-After", "1")
		reject(http.StatusTooManyRequests, "RATE_LIMITED", "rate limit exceeded")
		return
	}
	var body []byte
	if r.Body != nil && r.Body != http.NoBody {
		var err error
		body, err = io.ReadAll(http.MaxBytesReader(w, r.Body, p.MaxBody))
		if err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				reject(http.StatusRequestEntityTooLarge, "REQUEST_TOO_LARGE", "request body too large")
			} else {
				reject(http.StatusBadRequest, "INVALID_REQUEST", "could not read request body")
			}
			return
		}
	}
	e.ReqSize = int64(len(body))

	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(cfg.RequestTimeoutMs)*time.Millisecond)
	defer cancel()
	canRetry := retryable(r)
	tried := map[string]bool{}
	var last failure
	var lastBackend *routing.Backend

	for attempt := 0; attempt <= cfg.RetryCount; attempt++ {
		b := p.Algos.Select(cfg.Algorithm, p.Pool.All(), ip, tried)
		if b == nil {
			if attempt == 0 {
				e.BackendID = ""
				reject(http.StatusServiceUnavailable, "NO_HEALTHY_BACKENDS", "no healthy backend servers available")
				return
			}
			break
		}
		tried[b.ID] = true
		e.Retries = attempt
		e.BackendID, e.BackendIndex = b.ID, b.Index
		lastBackend = b

		attemptStart := time.Now()
		b.Active.Add(1)
		b.Total.Add(1)
		resp, acancel, err := p.send(ctx, b, r, body, e.RequestID, ip, time.Duration(cfg.BackendTimeoutMs)*time.Millisecond)
		if err != nil {
			b.Active.Add(-1)
			last = classifyErr(err, r, ctx)
			if last.reason != "CLIENT_CANCELED" {
				b.Failed.Add(1)
				b.LatencyUs.Add(time.Since(attemptStart).Microseconds())
				b.LatencyN.Add(1)
			}
			if last.connLevel { // passive health check: take the server out of rotation right away
				from, to := b.MarkFailed(last.reason)
				p.Mon.Transition(b, from, to, last.reason, "passive")
			}
			p.Log.Warn("backend attempt failed", "request_id", e.RequestID, "backend", b.ID, "attempt", attempt, "reason", last.reason, "error", err.Error())
			if last.reason == "CLIENT_CANCELED" || last.reason == "REQUEST_TIMEOUT" || !(last.connLevel || canRetry) {
				break
			}
			continue
		}

		// The backend answered. Retry 502/503/504 elsewhere if that is safe and another server is available.
		if (resp.StatusCode == 502 || resp.StatusCode == 503 || resp.StatusCode == 504) && canRetry &&
			attempt < cfg.RetryCount && p.othersAvailable(tried) {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
			acancel()
			b.Active.Add(-1)
			b.Failed.Add(1)
			b.LatencyUs.Add(time.Since(attemptStart).Microseconds())
			b.LatencyN.Add(1)
			last = failure{reason: failReason(resp), status: resp.StatusCode}
			continue
		}

		p.relay(w, resp, &e)
		acancel()
		b.Active.Add(-1)
		b.LatencyUs.Add(time.Since(attemptStart).Microseconds())
		b.LatencyN.Add(1)
		if resp.StatusCode >= 500 {
			b.Failed.Add(1)
		} else {
			b.Success.Add(1)
		}
		finish()
		return
	}

	// every attempt failed
	if last.status == 0 {
		last = failure{"BACKEND_UNAVAILABLE", 502, true}
	}
	if lastBackend == nil {
		reject(http.StatusServiceUnavailable, "NO_HEALTHY_BACKENDS", "no healthy backend servers available")
		return
	}
	reject(last.status, last.reason, fmt.Sprintf("upstream failure: %s", strings.ToLower(strings.ReplaceAll(last.reason, "_", " "))))
}

func failReason(resp *http.Response) string {
	if m := resp.Header.Get("X-Meta-Error"); m != "" {
		return m
	}
	return "HTTP_" + strconv.Itoa(resp.StatusCode)
}

func (p *Proxy) othersAvailable(tried map[string]bool) bool {
	for _, b := range p.Pool.All() {
		if b.Available() && !tried[b.ID] {
			return true
		}
	}
	return false
}

func (p *Proxy) send(ctx context.Context, b *routing.Backend, r *http.Request, body []byte, reqID, ip string, timeout time.Duration) (*http.Response, context.CancelFunc, error) {
	actx, cancel := context.WithTimeout(ctx, timeout)
	var rd io.Reader
	if len(body) > 0 {
		rd = bytes.NewReader(body)
	}
	out, err := http.NewRequestWithContext(actx, r.Method, b.URL.Scheme+"://"+b.URL.Host+r.URL.RequestURI(), rd)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	for k, vs := range r.Header {
		ck := http.CanonicalHeaderKey(k)
		if hopByHop[ck] || strings.HasPrefix(ck, "X-Meta-") || strings.HasPrefix(ck, "X-Forwarded-") || ck == "X-Real-Ip" || ck == "X-Request-Id" {
			continue
		}
		out.Header[ck] = vs
	}
	out.Header.Set("X-Request-ID", reqID)
	out.Header.Set("X-Forwarded-For", ip)
	out.Header.Set("X-Forwarded-Host", r.Host)
	out.Header.Set("X-Forwarded-Proto", "http")
	out.Header.Set("X-LB-Backend", b.ID)
	resp, err := p.tr.RoundTrip(out)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	return resp, cancel, nil
}

// relay copies the backend response to the client and harvests the X-Meta-* headers (user, product, order...).
func (p *Proxy) relay(w http.ResponseWriter, resp *http.Response, e *metrics.Entry) {
	defer resp.Body.Close()
	h := w.Header()
	for k, vs := range resp.Header {
		ck := http.CanonicalHeaderKey(k)
		if hopByHop[ck] || strings.HasPrefix(ck, "X-Meta-") || ck == "X-Request-Id" {
			continue
		}
		h[ck] = vs
	}
	if e.UserID == 0 { // login / register: the backend tells us who just authenticated
		if id, err := strconv.ParseInt(resp.Header.Get("X-Meta-User-Id"), 10, 64); err == nil {
			e.UserID = id
			e.UserName = resp.Header.Get("X-Meta-User-Name")
		}
	}
	e.OrderID = resp.Header.Get("X-Meta-Order-Id")
	var d []string
	if v := resp.Header.Get("X-Meta-Product"); v != "" {
		d = append(d, "product="+v)
	}
	if v := resp.Header.Get("X-Meta-Quantity"); v != "" {
		d = append(d, "qty="+v)
	}
	if v := resp.Header.Get("X-Meta-Amount"); v != "" {
		d = append(d, "amount="+v)
	}
	e.Detail = strings.Join(d, " ")
	h.Set("X-Request-ID", e.RequestID)
	h.Set("X-Served-By", e.BackendID)
	e.Status = resp.StatusCode
	if resp.StatusCode >= 500 {
		e.FailureReason = failReason(resp)
	}
	w.WriteHeader(resp.StatusCode)
	cw := &countWriter{w: w}
	_, _ = io.Copy(cw, resp.Body)
	e.RespSize = cw.n
}

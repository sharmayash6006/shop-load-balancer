// Package admin serves the dashboard API under /_lb/. Everything except /_lb/login requires an admin session.
package admin

import (
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"lb/internal/algorithms"
	"lb/internal/auth"
	"lb/internal/config"
	"lb/internal/health"
	"lb/internal/metrics"
	"lb/internal/middleware"
	"lb/internal/routing"
	"lb/internal/store"
)

type API struct {
	Cfg  *config.Config
	Pool *routing.Pool
	M    *metrics.Metrics
	Mon  *health.Monitor
	St   *store.Store // may be nil
	Log  *slog.Logger

	secret []byte
	mu     sync.Mutex
	fails  map[string][]time.Time
}

func New(cfg *config.Config, pool *routing.Pool, m *metrics.Metrics, mon *health.Monitor, st *store.Store, log *slog.Logger) *API {
	return &API{Cfg: cfg, Pool: pool, M: m, Mon: mon, St: st, Log: log, secret: []byte("admin:" + cfg.JWTSecret), fails: map[string][]time.Time{}}
}

func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /_lb/login", a.login)
	mux.HandleFunc("POST /_lb/logout", a.logout)
	g := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, a.guard(h)) }
	g("GET /_lb/api/me", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"user": a.Cfg.AdminUser})
	})
	g("GET /_lb/api/stats", a.stats)
	g("GET /_lb/api/backends", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, a.snapshots()) })
	g("POST /_lb/api/backends/{id}/enable", a.setEnabled(true))
	g("POST /_lb/api/backends/{id}/disable", a.setEnabled(false))
	g("POST /_lb/api/backends/{id}/recover", a.recoverBackend)
	g("POST /_lb/api/backends/{id}/weight", a.setWeight)
	g("GET /_lb/api/health-events", a.healthEvents)
	g("GET /_lb/api/config", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, a.configView()) })
	g("POST /_lb/api/config", a.updateConfig)
	g("GET /_lb/api/timeseries", a.timeseries)
	g("GET /_lb/api/requests", a.requests)
	g("GET /_lb/api/requests/live", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, a.M.Latest(100)) })
	g("GET /_lb/api/users", a.users)
	g("GET /_lb/api/users/{id}/activity", a.userActivity)
	g("GET /_lb/api/orders", a.orders)
	g("GET /_lb/api/orders/{id}/trace", a.orderTrace)
	g("GET /_lb/api/loadtest", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, a.M.Load.Snapshot()) })
	g("GET /_lb/api/events", a.events)
	mux.HandleFunc("/_lb/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 404, map[string]string{"error": "not found"})
	})
	return mux
}

// ---------- auth ----------

const cookieName = "lb_admin"

func (a *API) guard(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(cookieName)
		if err != nil {
			writeJSON(w, 401, map[string]string{"error": "admin authentication required"})
			return
		}
		cl, err := auth.Verify(c.Value, a.secret)
		if err != nil || cl.Name != "admin" {
			writeJSON(w, 401, map[string]string{"error": "admin authentication required"})
			return
		}
		if r.Method != http.MethodGet && r.Header.Get("X-Requested-With") == "" { // CSRF
			writeJSON(w, 403, map[string]string{"error": "missing X-Requested-With header"})
			return
		}
		next(w, r)
	})
}

func (a *API) throttled(ip string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	cut := time.Now().Add(-time.Minute)
	keep := a.fails[ip][:0]
	for _, t := range a.fails[ip] {
		if t.After(cut) {
			keep = append(keep, t)
		}
	}
	a.fails[ip] = keep
	return len(keep) >= 5
}

func (a *API) login(w http.ResponseWriter, r *http.Request) {
	ip := middleware.ClientIP(r)
	if r.Header.Get("X-Requested-With") == "" {
		writeJSON(w, 403, map[string]string{"error": "missing X-Requested-With header"})
		return
	}
	if a.throttled(ip) {
		writeJSON(w, 429, map[string]string{"error": "too many failed logins, try again in a minute"})
		return
	}
	var in struct{ Username, Password string }
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid body"})
		return
	}
	u := subtle.ConstantTimeCompare([]byte(in.Username), []byte(a.Cfg.AdminUser))
	p := subtle.ConstantTimeCompare([]byte(in.Password), []byte(a.Cfg.AdminPassword))
	if u&p != 1 || a.Cfg.AdminPassword == "" {
		a.mu.Lock()
		a.fails[ip] = append(a.fails[ip], time.Now())
		a.mu.Unlock()
		writeJSON(w, 401, map[string]string{"error": "invalid credentials"})
		return
	}
	tok := auth.Sign(auth.Claims{Name: "admin", Exp: time.Now().Add(8 * time.Hour).Unix()}, a.secret)
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: tok, Path: "/", HttpOnly: true, Secure: a.Cfg.CookieSecure,
		SameSite: a.sameSite(), MaxAge: 8 * 3600})
	writeJSON(w, 200, map[string]string{"user": a.Cfg.AdminUser})
}

func (a *API) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", HttpOnly: true, Secure: a.Cfg.CookieSecure, SameSite: a.sameSite(), MaxAge: -1})
	writeJSON(w, 200, map[string]string{"status": "logged out"})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (a *API) needDB(w http.ResponseWriter) bool {
	if a.St == nil {
		writeJSON(w, 503, map[string]string{"error": "database unavailable"})
		return false
	}
	return true
}

// ---------- stats, backends, config ----------

func (a *API) snapshots() []routing.Snapshot {
	out := make([]routing.Snapshot, 0, len(a.Pool.All()))
	var total int64
	for _, b := range a.Pool.All() {
		total += b.Total.Load()
	}
	for _, b := range a.Pool.All() {
		out = append(out, b.Snapshot())
	}
	_ = total
	return out
}

func (a *API) stats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"stats": a.M.Stats(), "backends": a.snapshots(), "algorithm": a.Cfg.Get().Algorithm})
}

func (a *API) backend(w http.ResponseWriter, r *http.Request) *routing.Backend {
	b := a.Pool.Get(r.PathValue("id"))
	if b == nil {
		writeJSON(w, 404, map[string]string{"error": "unknown backend"})
	}
	return b
}

func (a *API) persist(b *routing.Backend) {
	if a.St != nil {
		go a.St.UpsertBackend(b)
	}
}

func (a *API) setEnabled(on bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b := a.backend(w, r)
		if b == nil {
			return
		}
		from, to := b.SetEnabled(on)
		reason := "disabled by administrator"
		if on {
			reason = "enabled by administrator, waiting for health check"
		}
		a.Mon.Transition(b, from, to, reason, "admin")
		a.persist(b)
		if on {
			a.Mon.CheckNow()
		}
		a.Log.Warn("admin action", "action", map[bool]string{true: "enable", false: "disable"}[on], "backend", b.ID)
		writeJSON(w, 200, b.Snapshot())
	}
}

func (a *API) recoverBackend(w http.ResponseWriter, r *http.Request) {
	b := a.backend(w, r)
	if b == nil {
		return
	}
	from, to := b.Recover()
	a.Mon.Transition(b, from, to, "recovery requested by administrator", "admin")
	a.Mon.CheckNow()
	writeJSON(w, 200, b.Snapshot())
}

func (a *API) setWeight(w http.ResponseWriter, r *http.Request) {
	b := a.backend(w, r)
	if b == nil {
		return
	}
	var in struct{ Weight int }
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&in); err != nil || in.Weight < 1 || in.Weight > 100 {
		writeJSON(w, 400, map[string]string{"error": "weight must be an integer between 1 and 100"})
		return
	}
	b.SetWeight(in.Weight)
	a.persist(b)
	writeJSON(w, 200, b.Snapshot())
}

func (a *API) healthEvents(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, a.Mon.Events(100))
}

func (a *API) configView() map[string]any {
	return map[string]any{"settings": a.Cfg.Get(), "algorithms": algorithms.Names}
}

func (a *API) updateConfig(w http.ResponseWriter, r *http.Request) {
	var in map[string]json.RawMessage
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid body"})
		return
	}
	var verr error
	num := func(k string, lo, hi float64) (float64, bool) {
		raw, ok := in[k]
		if !ok {
			return 0, false
		}
		var v float64
		if err := json.Unmarshal(raw, &v); err != nil || v < lo || v > hi {
			verr = fmt.Errorf("%s must be a number between %g and %g", k, lo, hi)
			return 0, false
		}
		return v, true
	}
	var alg string
	if raw, ok := in["algorithm"]; ok {
		if err := json.Unmarshal(raw, &alg); err != nil || !algorithms.Valid(alg) {
			verr = errors.New("algorithm must be one of: " + strings.Join(algorithms.Names, ", "))
		}
	}
	ct, hasCT := num("connect_timeout_ms", 50, 60000)
	rt, hasRT := num("request_timeout_ms", 100, 300000)
	bt, hasBT := num("backend_timeout_ms", 50, 300000)
	rc, hasRC := num("retry_count", 0, 5)
	rl, hasRL := num("rate_limit", 0, 1e6)
	hi, hasHI := num("health_interval_ms", 200, 60000)
	if verr != nil {
		writeJSON(w, 400, map[string]string{"error": verr.Error()})
		return
	}
	s := a.Cfg.Update(func(s *config.Settings) {
		if alg != "" {
			s.Algorithm = alg
		}
		if hasCT {
			s.ConnectTimeoutMs = int(ct)
		}
		if hasRT {
			s.RequestTimeoutMs = int(rt)
		}
		if hasBT {
			s.BackendTimeoutMs = int(bt)
		}
		if hasRC {
			s.RetryCount = int(rc)
		}
		if hasRL {
			s.RateLimit = rl
		}
		if hasHI {
			s.HealthIntervalMs = int(hi)
		}
	})
	a.Log.Warn("admin changed configuration", "settings", s)
	writeJSON(w, 200, map[string]any{"settings": s, "algorithms": algorithms.Names})
}

func (a *API) timeseries(w http.ResponseWriter, r *http.Request) {
	rng := r.URL.Query().Get("range")
	writeJSON(w, 200, map[string]any{"range": rng, "backends": backendIDs(a.Pool), "points": a.M.Series.Query(rng, len(a.Pool.All()))})
}

func backendIDs(p *routing.Pool) []string {
	var ids []string
	for _, b := range p.All() {
		ids = append(ids, b.ID)
	}
	return ids
}

// ---------- database-backed queries ----------

func (a *API) requests(w http.ResponseWriter, r *http.Request) {
	if !a.needDB(w) {
		return
	}
	q := r.URL.Query()
	f := store.ReqFilter{Q: q.Get("q"), Status: q.Get("status"), Backend: q.Get("backend"), FailedOnly: q.Get("failed") == "1", LoadTest: q.Get("loadtest")}
	f.Limit, _ = strconv.Atoi(q.Get("limit"))
	f.UserID, _ = strconv.ParseInt(q.Get("user_id"), 10, 64)
	if t, err := time.Parse(time.RFC3339, q.Get("from")); err == nil {
		f.From = t
	}
	if t, err := time.Parse(time.RFC3339, q.Get("to")); err == nil {
		f.To = t
	}
	rows, err := a.St.Requests(r.Context(), f)
	a.respond(w, rows, err)
}

func (a *API) users(w http.ResponseWriter, r *http.Request) {
	if !a.needDB(w) {
		return
	}
	rows, err := a.St.Users(r.Context(), r.URL.Query().Get("q"))
	a.respond(w, rows, err)
}

func (a *API) userActivity(w http.ResponseWriter, r *http.Request) {
	if !a.needDB(w) {
		return
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(strings.ToUpper(r.PathValue("id")), "USR-"), 10, 64)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid user id"})
		return
	}
	rows, err := a.St.UserActivity(r.Context(), id, 300)
	a.respond(w, rows, err)
}

func (a *API) orders(w http.ResponseWriter, r *http.Request) {
	if !a.needDB(w) {
		return
	}
	rows, err := a.St.Orders(r.Context(), r.URL.Query().Get("q"))
	a.respond(w, rows, err)
}

func (a *API) orderTrace(w http.ResponseWriter, r *http.Request) {
	if !a.needDB(w) {
		return
	}
	t, err := a.St.OrderTrace(r.Context(), r.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, 404, map[string]string{"error": "order not found"})
		return
	}
	a.respond(w, t, err)
}

func (a *API) respond(w http.ResponseWriter, v any, err error) {
	if err != nil {
		a.Log.Error("admin query failed", "error", err)
		writeJSON(w, 500, map[string]string{"error": "query failed"})
		return
	}
	writeJSON(w, 200, v)
}

// ---------- real-time stream (Server-Sent Events) ----------

// events pushes one snapshot per second: stats, backend table, load-test metrics, and every request that
// finished since the previous push. The dashboard never polls and never needs a manual refresh.
func (a *API) events(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, 500, map[string]string{"error": "streaming unsupported"})
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	_, last := a.M.Since(1<<62, 0)
	send := func(first bool) bool {
		var recent []metrics.Entry
		if first {
			recent = reverse(a.M.Latest(50))
			_, last = a.M.Since(1<<62, 0)
		} else {
			recent, last = a.M.Since(last, 100)
		}
		payload, _ := json.Marshal(map[string]any{
			"stats": a.M.Stats(), "backends": a.snapshots(), "algorithm": a.Cfg.Get().Algorithm,
			"recent": recent, "load_test": a.M.Load.Snapshot(), "time": time.Now().UTC().Format(time.RFC3339),
		})
		if _, err := fmt.Fprintf(w, "event: snapshot\ndata: %s\n\n", payload); err != nil {
			return false
		}
		fl.Flush()
		return true
	}
	if !send(true) {
		return
	}
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-t.C:
			if !send(false) {
				return
			}
		}
	}
}

func reverse(in []metrics.Entry) []metrics.Entry {
	for i, j := 0, len(in)-1; i < j; i, j = i+1, j-1 {
		in[i], in[j] = in[j], in[i]
	}
	return in
}

func (a *API) sameSite() http.SameSite {
	switch a.Cfg.CookieSameSite {
	case "none":
		return http.SameSiteNoneMode
	case "strict":
		return http.SameSiteStrictMode
	}
	return http.SameSiteLaxMode
}

// Package health runs active health checks and records every backend state transition.
package health

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"lb/internal/config"
	"lb/internal/routing"
	"lb/internal/store"
)

type Event struct {
	Time    time.Time      `json:"time"`
	Backend string         `json:"backend_id"`
	From    routing.Status `json:"from"`
	To      routing.Status `json:"to"`
	Reason  string         `json:"reason"`
	Source  string         `json:"source"` // health-check | passive | admin
}

type Monitor struct {
	Pool *routing.Pool
	Cfg  *config.Config
	Log  *slog.Logger
	St   *store.Store // may be nil (unit tests)

	client  *http.Client
	trigger chan struct{}
	mu      sync.Mutex
	events  []Event
}

func New(p *routing.Pool, c *config.Config, log *slog.Logger, st *store.Store) *Monitor {
	return &Monitor{Pool: p, Cfg: c, Log: log, St: st, trigger: make(chan struct{}, 1),
		client: &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 2, IdleConnTimeout: 30 * time.Second}}}
}

// Transition records a state change (log line, in-memory event feed, database row).
func (m *Monitor) Transition(b *routing.Backend, from, to routing.Status, reason, source string) {
	if from == to {
		return
	}
	ev := Event{Time: time.Now(), Backend: b.ID, From: from, To: to, Reason: reason, Source: source}
	m.mu.Lock()
	m.events = append(m.events, ev)
	if len(m.events) > 500 {
		m.events = m.events[len(m.events)-250:]
	}
	m.mu.Unlock()
	m.Log.Warn("backend state change", "backend", b.ID, "from", from, "to", to, "reason", reason, "source", source)
	if m.St != nil {
		m.St.LogHealth(store.HealthRecord{BackendID: b.ID, Time: ev.Time, Healthy: to == routing.Healthy, State: string(to), Error: reason, Transition: true})
	}
}

// Events returns the newest-first transition feed since startup.
func (m *Monitor) Events(limit int) []Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Event, 0, limit)
	for i := len(m.events) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, m.events[i])
	}
	return out
}

// CheckNow asks the loop to run an immediate round of checks (used after enable/recover).
func (m *Monitor) CheckNow() {
	select {
	case m.trigger <- struct{}{}:
	default:
	}
}

func (m *Monitor) Run(stop <-chan struct{}) {
	m.CheckAll()
	for {
		iv := time.Duration(m.Cfg.Get().HealthIntervalMs) * time.Millisecond
		if iv < 200*time.Millisecond {
			iv = 200 * time.Millisecond
		}
		select {
		case <-stop:
			return
		case <-time.After(iv):
		case <-m.trigger:
		}
		m.CheckAll()
	}
}

func (m *Monitor) CheckAll() {
	var wg sync.WaitGroup
	for _, b := range m.Pool.All() {
		wg.Add(1)
		go func(b *routing.Backend) { defer wg.Done(); m.check(b) }(b)
	}
	wg.Wait()
}

func (m *Monitor) check(b *routing.Backend) {
	s := m.Cfg.Get()
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(s.HealthTimeoutMs)*time.Millisecond)
	defer cancel()
	start := time.Now()
	code, errMsg := 0, ""
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, b.URL.String()+"/health", nil)
	resp, err := m.client.Do(req)
	if err != nil {
		errMsg = shorten(err.Error())
	} else {
		code = resp.StatusCode
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if code != http.StatusOK {
			errMsg = "health endpoint returned HTTP " + http.StatusText(code)
		}
	}
	lat := float64(time.Since(start).Microseconds()) / 1000
	ok := err == nil && code == http.StatusOK
	if !b.Enabled() { // disabled servers are not probed for rotation, but we keep the record fresh
		b.RecordCheck(ok, lat, errMsg, s.HealthRise, s.HealthFall)
		return
	}
	from, to := b.RecordCheck(ok, lat, errMsg, s.HealthRise, s.HealthFall)
	if m.St != nil {
		m.St.LogHealth(store.HealthRecord{BackendID: b.ID, Time: time.Now(), Healthy: ok, StatusCode: code, LatencyMs: lat, Error: errMsg, State: string(to)})
	}
	if from != to {
		reason := "health check passed"
		if !ok {
			reason = errMsg
		}
		m.Transition(b, from, to, reason, "health-check")
	}
}

func shorten(s string) string {
	if len(s) > 200 {
		return s[:200]
	}
	return s
}

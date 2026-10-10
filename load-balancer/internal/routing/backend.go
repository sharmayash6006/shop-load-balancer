// Package routing holds the backend server model and the pool of servers.
package routing

import (
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

type Status string

const (
	Healthy    Status = "HEALTHY"
	Unhealthy  Status = "UNHEALTHY"
	Disabled   Status = "DISABLED"
	Recovering Status = "RECOVERING" // enabled (or just started) and waiting for a passing health check
)

type Backend struct {
	ID    string
	Name  string
	Host  string
	Port  int
	URL   *url.URL
	Index int // position in the pool, used for per-backend time series

	Active    atomic.Int64
	Total     atomic.Int64 // attempts sent to this backend
	Success   atomic.Int64
	Failed    atomic.Int64 // 5xx responses or connection errors
	LatencyUs atomic.Int64 // sum of attempt latency (microseconds)
	LatencyN  atomic.Int64

	mu          sync.RWMutex
	weight      int
	enabled     bool
	status      Status
	lastCheck   time.Time
	lastError   string
	lastLatMs   float64
	consecOK    int
	consecFail  int
	seenHealthy bool // has been HEALTHY at least once: a server that never was admits on the first passing check
}

func New(index int, rawURL string, weight int) (*Backend, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	port, _ := strconv.Atoi(u.Port())
	if weight < 1 {
		weight = 1
	}
	n := strconv.Itoa(index + 1)
	return &Backend{ID: "backend-" + n, Name: "Backend " + n, Host: u.Hostname(), Port: port, URL: u, Index: index,
		weight: weight, enabled: true, status: Recovering}, nil
}

// Available reports whether the backend may receive new requests.
func (b *Backend) Available() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.enabled && b.status == Healthy
}

func (b *Backend) Weight() int     { b.mu.RLock(); defer b.mu.RUnlock(); return b.weight }
func (b *Backend) Enabled() bool   { b.mu.RLock(); defer b.mu.RUnlock(); return b.enabled }
func (b *Backend) State() Status   { b.mu.RLock(); defer b.mu.RUnlock(); return b.status }
func (b *Backend) SetWeight(w int) { b.mu.Lock(); b.weight = w; b.mu.Unlock() }

// SetEnabled implements the manual enable/disable control. Disabling takes effect immediately for new
// requests (in-flight requests finish normally). Enabling moves the server to RECOVERING: it only receives
// traffic again after a health check passes.
func (b *Backend) SetEnabled(on bool) (from, to Status) {
	b.mu.Lock()
	defer b.mu.Unlock()
	from = b.status
	b.enabled = on
	if on {
		b.status = Recovering
		b.consecOK, b.consecFail = 0, 0
	} else {
		b.status = Disabled
	}
	return from, b.status
}

// Recover forces an UNHEALTHY server back to RECOVERING so the next check can re-admit it quickly.
func (b *Backend) Recover() (from, to Status) {
	b.mu.Lock()
	defer b.mu.Unlock()
	from = b.status
	if b.enabled && b.status == Unhealthy {
		b.status = Recovering
		b.consecOK, b.consecFail = 0, 0
	}
	return from, b.status
}

// RecordCheck applies the result of an active health check and returns the state transition (if any).
func (b *Backend) RecordCheck(ok bool, latMs float64, errMsg string, rise, fall int) (from, to Status) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lastCheck = time.Now()
	b.lastLatMs = latMs
	b.lastError = errMsg
	from = b.status
	if !b.enabled {
		b.status = Disabled
		return from, b.status
	}
	if ok {
		b.consecOK++
		b.consecFail = 0
		need := rise
		if b.status == Recovering || !b.seenHealthy {
			need = 1
		}
		if b.status != Healthy && b.consecOK >= need {
			b.status = Healthy
			b.seenHealthy = true
		}
	} else {
		b.consecFail++
		b.consecOK = 0
		if b.status == Healthy && b.consecFail >= fall {
			b.status = Unhealthy
		} else if b.status == Recovering {
			b.status = Unhealthy
		}
	}
	return from, b.status
}

// MarkFailed is passive health checking: a refused/unreachable connection during real traffic removes the
// server from rotation immediately instead of waiting for the next active check.
func (b *Backend) MarkFailed(reason string) (from, to Status) {
	b.mu.Lock()
	defer b.mu.Unlock()
	from = b.status
	b.lastError = reason
	if b.enabled && b.status == Healthy {
		b.status = Unhealthy
		b.consecOK = 0
		b.consecFail = 1
	}
	return from, b.status
}

type Snapshot struct {
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	Host          string  `json:"host"`
	Port          int     `json:"port"`
	Status        Status  `json:"status"`
	Enabled       bool    `json:"enabled"`
	Weight        int     `json:"weight"`
	Active        int64   `json:"active_connections"`
	Total         int64   `json:"total_requests"`
	Success       int64   `json:"successful_requests"`
	Failed        int64   `json:"failed_requests"`
	AvgResponseMs float64 `json:"avg_response_ms"`
	LastCheck     string  `json:"last_health_check"`
	LastError     string  `json:"last_error"`
	LastCheckMs   float64 `json:"last_check_latency_ms"`
}

func (b *Backend) Snapshot() Snapshot {
	b.mu.RLock()
	defer b.mu.RUnlock()
	s := Snapshot{ID: b.ID, Name: b.Name, Host: b.Host, Port: b.Port, Status: b.status, Enabled: b.enabled, Weight: b.weight,
		Active: b.Active.Load(), Total: b.Total.Load(), Success: b.Success.Load(), Failed: b.Failed.Load(),
		LastError: b.lastError, LastCheckMs: b.lastLatMs}
	if n := b.LatencyN.Load(); n > 0 {
		s.AvgResponseMs = float64(b.LatencyUs.Load()) / float64(n) / 1000
	}
	if !b.lastCheck.IsZero() {
		s.LastCheck = b.lastCheck.UTC().Format(time.RFC3339)
	}
	return s
}

// Pool is the ordered, fixed-size list of backends.
type Pool struct{ backends []*Backend }

func NewPool(urls []string, weights []int) (*Pool, error) {
	p := &Pool{}
	for i, u := range urls {
		w := 1
		if i < len(weights) {
			w = weights[i]
		}
		b, err := New(i, u, w)
		if err != nil {
			return nil, err
		}
		p.backends = append(p.backends, b)
	}
	return p, nil
}

func (p *Pool) All() []*Backend { return p.backends }

func (p *Pool) Get(id string) *Backend {
	for _, b := range p.backends {
		if b.ID == id {
			return b
		}
	}
	return nil
}

func (p *Pool) HealthyCount() int {
	n := 0
	for _, b := range p.backends {
		if b.Available() {
			n++
		}
	}
	return n
}

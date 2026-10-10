package metrics

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"lb/internal/routing"
)

const recentCap = 1000

type Metrics struct {
	Started time.Time

	Total, OK, Failed          atomic.Int64
	S2, S3, S4, S5             atomic.Int64
	RateLimited                atomic.Int64
	Timeouts, ConnErrors       atomic.Int64
	NoBackend, DBErrors        atomic.Int64
	Retried                    atomic.Int64
	Active                     atomic.Int64 // in-flight requests at the LB
	PeakActive                 atomic.Int64
	AvailSeconds, TotalSeconds atomic.Int64
	DroppedLogs                atomic.Int64

	Hist   Hist
	Series Series
	Load   *LoadTest

	// Sink receives every finished request (the database writer). May be nil.
	Sink func(Entry)

	seq     atomic.Int64
	mu      sync.Mutex
	recent  []Entry
	reasons map[string]int64
	pool    *routing.Pool
}

func New(pool *routing.Pool) *Metrics {
	return &Metrics{Started: time.Now(), pool: pool, reasons: map[string]int64{}, Load: newLoadTest()}
}

func (m *Metrics) Begin() {
	n := m.Active.Add(1)
	for {
		p := m.PeakActive.Load()
		if n <= p || m.PeakActive.CompareAndSwap(p, n) {
			break
		}
	}
}
func (m *Metrics) End() { m.Active.Add(-1) }

// Record stores a finished request everywhere: counters, histogram, series, live feed, load test, database.
func (m *Metrics) Record(e Entry) {
	e.Seq = m.seq.Add(1)
	m.Total.Add(1)
	if e.Success {
		m.OK.Add(1)
	} else {
		m.Failed.Add(1)
	}
	switch {
	case e.Status >= 500:
		m.S5.Add(1)
	case e.Status >= 400:
		m.S4.Add(1)
	case e.Status >= 300:
		m.S3.Add(1)
	default:
		m.S2.Add(1)
	}
	if e.Retries > 0 {
		m.Retried.Add(1)
	}
	switch e.FailureReason {
	case "":
	case "RATE_LIMITED":
		m.RateLimited.Add(1)
	case "CONNECTION_TIMEOUT", "BACKEND_TIMEOUT", "REQUEST_TIMEOUT":
		m.Timeouts.Add(1)
	case "CONNECTION_REFUSED", "BACKEND_ERROR":
		m.ConnErrors.Add(1)
	case "NO_HEALTHY_BACKENDS":
		m.NoBackend.Add(1)
	case "DATABASE_UNAVAILABLE":
		m.DBErrors.Add(1)
	}
	us := int64(e.LatencyMs * 1000)
	m.Hist.Observe(us)
	m.Series.Record(e.Time, &e)
	m.mu.Lock()
	if e.FailureReason != "" {
		m.reasons[e.FailureReason]++
	}
	if len(m.recent) >= recentCap {
		copy(m.recent, m.recent[len(m.recent)-recentCap/2:])
		m.recent = m.recent[:recentCap/2]
	}
	m.recent = append(m.recent, e)
	m.mu.Unlock()
	m.Load.Observe(&e, m.Active.Load())
	if m.Sink != nil {
		m.Sink(e)
	}
}

// Since returns up to limit entries newer than seq (oldest first) and the latest sequence number.
func (m *Metrics) Since(seq int64, limit int) ([]Entry, int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	last := m.seq.Load()
	i := sort.Search(len(m.recent), func(i int) bool { return m.recent[i].Seq > seq })
	out := m.recent[i:]
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return append([]Entry(nil), out...), last
}

// Latest returns the newest n entries, newest first.
func (m *Metrics) Latest(n int) []Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	if n > len(m.recent) {
		n = len(m.recent)
	}
	out := make([]Entry, 0, n)
	for i := len(m.recent) - 1; i >= len(m.recent)-n; i-- {
		out = append(out, m.recent[i])
	}
	return out
}

// RunSampler records gauges once per second and tracks availability (>=1 healthy backend).
func (m *Metrics) RunSampler(stop <-chan struct{}) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case now := <-t.C:
			h := m.pool.HealthyCount()
			m.Series.Sample(now, m.Active.Load(), h)
			m.TotalSeconds.Add(1)
			if h > 0 {
				m.AvailSeconds.Add(1)
			}
		}
	}
}

type Stats struct {
	TotalRequests   int64            `json:"total_requests"`
	Successful      int64            `json:"successful"`
	Failed          int64            `json:"failed"`
	SuccessRate     float64          `json:"success_rate"`
	AvgMs           float64          `json:"avg_ms"`
	P50             float64          `json:"p50_ms"`
	P95             float64          `json:"p95_ms"`
	P99             float64          `json:"p99_ms"`
	MaxMs           float64          `json:"max_ms"`
	RPS             float64          `json:"requests_per_sec"`
	RPM             int64            `json:"requests_per_min"`
	Active          int64            `json:"active_connections"`
	PeakActive      int64            `json:"peak_active_connections"`
	UptimeSeconds   int64            `json:"uptime_seconds"`
	AvailabilityPct float64          `json:"availability_pct"`
	S2              int64            `json:"s2xx"`
	S3              int64            `json:"s3xx"`
	S4              int64            `json:"s4xx"`
	S5              int64            `json:"s5xx"`
	RateLimited     int64            `json:"rate_limited"`
	Timeouts        int64            `json:"timeouts"`
	ConnErrors      int64            `json:"connection_errors"`
	NoBackend       int64            `json:"no_backend_errors"`
	DBErrors        int64            `json:"database_errors"`
	Retried         int64            `json:"retried_requests"`
	DroppedLogs     int64            `json:"dropped_log_rows"`
	Reasons         map[string]int64 `json:"failure_reasons"`
	HealthyBackends int              `json:"healthy_backends"`
	TotalBackends   int              `json:"total_backends"`
	NoHealthy       bool             `json:"no_healthy_backends"`
}

func (m *Metrics) Stats() Stats {
	total := m.Total.Load()
	s := Stats{TotalRequests: total, Successful: m.OK.Load(), Failed: m.Failed.Load(),
		AvgMs: m.Hist.AvgMs(), P50: m.Hist.Percentile(.50), P95: m.Hist.Percentile(.95), P99: m.Hist.Percentile(.99), MaxMs: m.Hist.MaxMs(),
		RPS: float64(m.Series.RecentRequests(5)) / 5, RPM: m.Series.RecentRequests(60),
		Active: m.Active.Load(), PeakActive: m.PeakActive.Load(), UptimeSeconds: int64(time.Since(m.Started).Seconds()),
		S2: m.S2.Load(), S3: m.S3.Load(), S4: m.S4.Load(), S5: m.S5.Load(),
		RateLimited: m.RateLimited.Load(), Timeouts: m.Timeouts.Load(), ConnErrors: m.ConnErrors.Load(),
		NoBackend: m.NoBackend.Load(), DBErrors: m.DBErrors.Load(), Retried: m.Retried.Load(), DroppedLogs: m.DroppedLogs.Load(),
		HealthyBackends: m.pool.HealthyCount(), TotalBackends: len(m.pool.All())}
	s.NoHealthy = s.HealthyBackends == 0
	if total > 0 {
		s.SuccessRate = float64(s.Successful) / float64(total) * 100
	} else {
		s.SuccessRate = 100
	}
	if ts := m.TotalSeconds.Load(); ts > 0 {
		s.AvailabilityPct = float64(m.AvailSeconds.Load()) / float64(ts) * 100
	} else {
		s.AvailabilityPct = 100
	}
	m.mu.Lock()
	s.Reasons = make(map[string]int64, len(m.reasons))
	for k, v := range m.reasons {
		s.Reasons[k] = v
	}
	m.mu.Unlock()
	return s
}

// WritePrometheus renders Prometheus text-format metrics.
func (m *Metrics) WritePrometheus(w io.Writer) {
	p := func(f string, a ...any) { fmt.Fprintf(w, f, a...) }
	p("# HELP lb_requests_total Total requests handled by the load balancer.\n# TYPE lb_requests_total counter\nlb_requests_total %d\n", m.Total.Load())
	p("# HELP lb_requests_failed_total Requests that failed (5xx, timeouts, rate-limited, no backend).\n# TYPE lb_requests_failed_total counter\nlb_requests_failed_total %d\n", m.Failed.Load())
	p("# HELP lb_http_responses_total Responses by status class.\n# TYPE lb_http_responses_total counter\n")
	p("lb_http_responses_total{class=\"2xx\"} %d\nlb_http_responses_total{class=\"3xx\"} %d\nlb_http_responses_total{class=\"4xx\"} %d\nlb_http_responses_total{class=\"5xx\"} %d\n",
		m.S2.Load(), m.S3.Load(), m.S4.Load(), m.S5.Load())
	p("# HELP lb_rate_limited_total Requests rejected with 429.\n# TYPE lb_rate_limited_total counter\nlb_rate_limited_total %d\n", m.RateLimited.Load())
	p("# HELP lb_retried_requests_total Requests that needed at least one retry.\n# TYPE lb_retried_requests_total counter\nlb_retried_requests_total %d\n", m.Retried.Load())
	p("# HELP lb_active_connections In-flight requests.\n# TYPE lb_active_connections gauge\nlb_active_connections %d\n", m.Active.Load())
	p("# HELP lb_request_duration_seconds Request latency.\n# TYPE lb_request_duration_seconds histogram\n")
	for _, le := range []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10} {
		p("lb_request_duration_seconds_bucket{le=\"%g\"} %d\n", le, m.Hist.CumulativeLE(le))
	}
	p("lb_request_duration_seconds_bucket{le=\"+Inf\"} %d\nlb_request_duration_seconds_sum %f\nlb_request_duration_seconds_count %d\n",
		m.Hist.Count(), m.Hist.SumSeconds(), m.Hist.Count())
	var sb strings.Builder
	sb.WriteString("# HELP lb_backend_health 1 when the backend is HEALTHY and enabled.\n# TYPE lb_backend_health gauge\n")
	var req, act, fail strings.Builder
	req.WriteString("# HELP lb_backend_requests_total Requests sent to each backend.\n# TYPE lb_backend_requests_total counter\n")
	act.WriteString("# HELP lb_backend_active_connections In-flight requests per backend.\n# TYPE lb_backend_active_connections gauge\n")
	fail.WriteString("# HELP lb_backend_failures_total Failed attempts per backend.\n# TYPE lb_backend_failures_total counter\n")
	for _, b := range m.pool.All() {
		v := 0
		if b.Available() {
			v = 1
		}
		fmt.Fprintf(&sb, "lb_backend_health{backend=%q} %d\n", b.ID, v)
		fmt.Fprintf(&req, "lb_backend_requests_total{backend=%q} %d\n", b.ID, b.Total.Load())
		fmt.Fprintf(&act, "lb_backend_active_connections{backend=%q} %d\n", b.ID, b.Active.Load())
		fmt.Fprintf(&fail, "lb_backend_failures_total{backend=%q} %d\n", b.ID, b.Failed.Load())
	}
	io.WriteString(w, sb.String()+req.String()+act.String()+fail.String())
}

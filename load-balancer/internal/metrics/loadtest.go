package metrics

import (
	"sort"
	"sync"
	"time"
)

// LoadTest aggregates the real traffic that carries an X-LoadTest header (sent by the k6 scripts).
// A new test starts automatically when traffic resumes after 15 seconds of silence.
type LoadTest struct {
	mu         sync.Mutex
	name       string
	started    time.Time
	last       time.Time
	req, ok    int64
	fail       int64
	s2, s4, s5 int64
	hist       Hist
	perBackend map[string]int64
	vus        map[int]time.Time
	maxVU      int
	series     []ltPoint
}

type ltPoint struct {
	T      int64   `json:"t"`
	Req    int64   `json:"requests"`
	Fail   int64   `json:"failed"`
	VUs    int     `json:"vus"`
	Active int64   `json:"active"`
	AvgMs  float64 `json:"avg_ms"`
	latUs  int64
}

func newLoadTest() *LoadTest {
	return &LoadTest{perBackend: map[string]int64{}, vus: map[int]time.Time{}}
}

func (l *LoadTest) reset(name string, now time.Time) {
	l.name, l.started = name, now
	l.req, l.ok, l.fail, l.s2, l.s4, l.s5 = 0, 0, 0, 0, 0, 0
	l.hist = Hist{}
	l.perBackend = map[string]int64{}
	l.vus = map[int]time.Time{}
	l.maxVU = 0
	l.series = nil
}

func (l *LoadTest) activeVUs(now time.Time) int {
	n := 0
	for id, t := range l.vus {
		if now.Sub(t) > 30*time.Second {
			delete(l.vus, id)
		} else if now.Sub(t) <= 5*time.Second {
			n++
		}
	}
	return n
}

func (l *LoadTest) Observe(e *Entry, active int64) {
	if e.LoadTest == "" {
		return
	}
	now := e.Time
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.last.IsZero() || now.Sub(l.last) > 15*time.Second || l.name != e.LoadTest && now.Sub(l.last) > 2*time.Second {
		l.reset(e.LoadTest, now)
	}
	l.last = now
	l.req++
	if e.Success {
		l.ok++
	} else {
		l.fail++
	}
	switch {
	case e.Status >= 500:
		l.s5++
	case e.Status >= 400:
		l.s4++
	default:
		l.s2++
	}
	l.hist.Observe(int64(e.LatencyMs * 1000))
	if e.BackendID != "" {
		l.perBackend[e.BackendID]++
	}
	if e.VU > 0 {
		l.vus[e.VU] = now
	}
	vu := l.activeVUs(now)
	if vu > l.maxVU {
		l.maxVU = vu
	}
	sec := now.Unix()
	if len(l.series) == 0 || l.series[len(l.series)-1].T != sec {
		l.series = append(l.series, ltPoint{T: sec})
		if len(l.series) > 900 {
			l.series = l.series[len(l.series)-900:]
		}
	}
	p := &l.series[len(l.series)-1]
	p.Req++
	if !e.Success {
		p.Fail++
	}
	p.VUs = vu
	if active > p.Active {
		p.Active = active
	}
	p.latUs += int64(e.LatencyMs * 1000)
	p.AvgMs = float64(p.latUs) / float64(p.Req) / 1000
}

type LoadSnapshot struct {
	Name        string           `json:"name"`
	Running     bool             `json:"running"`
	Started     string           `json:"started"`
	ElapsedSec  int64            `json:"elapsed_sec"`
	VUs         int              `json:"virtual_users"`
	MaxVUs      int              `json:"max_virtual_users"`
	Requests    int64            `json:"requests"`
	RPS         float64          `json:"requests_per_sec"`
	SuccessRate float64          `json:"success_rate"`
	FailureRate float64          `json:"failure_rate"`
	AvgMs       float64          `json:"avg_ms"`
	P95         float64          `json:"p95_ms"`
	P99         float64          `json:"p99_ms"`
	S2          int64            `json:"s2xx"`
	S4          int64            `json:"s4xx"`
	S5          int64            `json:"s5xx"`
	Backends    []LoadBackendPct `json:"traffic_distribution"`
	Series      []ltPoint        `json:"series"`
}

type LoadBackendPct struct {
	Backend  string  `json:"backend_id"`
	Requests int64   `json:"requests"`
	Pct      float64 `json:"pct"`
}

func (l *LoadTest) Snapshot() LoadSnapshot {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	s := LoadSnapshot{Name: l.name, Requests: l.req, S2: l.s2, S4: l.s4, S5: l.s5, MaxVUs: l.maxVU, Series: append([]ltPoint(nil), l.series...)}
	if l.last.IsZero() {
		s.Backends = []LoadBackendPct{}
		s.Series = []ltPoint{}
		return s
	}
	s.Running = now.Sub(l.last) < 5*time.Second
	s.Started = l.started.UTC().Format(time.RFC3339)
	end := l.last
	s.ElapsedSec = int64(end.Sub(l.started).Seconds())
	if s.Running {
		s.VUs = l.activeVUs(now)
		s.ElapsedSec = int64(now.Sub(l.started).Seconds())
	}
	if l.req > 0 {
		s.SuccessRate = float64(l.ok) / float64(l.req) * 100
		s.FailureRate = float64(l.fail) / float64(l.req) * 100
	}
	// RPS over the last 5 completed seconds of the test (or whole-test average when finished)
	var sum int64
	cut := now.Unix() - 5
	if !s.Running {
		cut = l.last.Unix() - 5
	}
	for _, p := range l.series {
		if p.T >= cut && p.T < now.Unix() {
			sum += p.Req
		}
	}
	s.RPS = float64(sum) / 5
	s.AvgMs, s.P95, s.P99 = l.hist.AvgMs(), l.hist.Percentile(.95), l.hist.Percentile(.99)
	s.Backends = []LoadBackendPct{}
	for id, n := range l.perBackend {
		s.Backends = append(s.Backends, LoadBackendPct{id, n, float64(n) / float64(l.req) * 100})
	}
	sort.Slice(s.Backends, func(i, j int) bool { return s.Backends[i].Backend < s.Backends[j].Backend })
	return s
}

package metrics

import (
	"sync"
	"time"
)

const MaxBackends = 8

type bucket struct {
	Ts                 int64
	Req, OK, Fail      int64
	S2, S3, S4, S5     int64
	LatSumUs, LatMaxUs int64
	PB                 [MaxBackends]int64
	Active             int64
	Healthy            int
}

// Point is one chart sample returned to the dashboard.
type Point struct {
	T          int64   `json:"t"`
	Req        int64   `json:"requests"`
	RPS        float64 `json:"rps"`
	OK         int64   `json:"ok"`
	Fail       int64   `json:"failed"`
	S2         int64   `json:"s2xx"`
	S3         int64   `json:"s3xx"`
	S4         int64   `json:"s4xx"`
	S5         int64   `json:"s5xx"`
	AvgMs      float64 `json:"avg_ms"`
	MaxMs      float64 `json:"max_ms"`
	PerBackend []int64 `json:"per_backend"`
	Active     int64   `json:"active"`
	Healthy    int     `json:"healthy"`
}

// Series stores 1-hour of per-second buckets and 24-hours of per-minute buckets.
type Series struct {
	mu   sync.Mutex
	secs [3600]bucket
	mins [1440]bucket
}

func (s *Series) at(sec int64) (*bucket, *bucket) {
	b := &s.secs[sec%3600]
	if b.Ts != sec {
		*b = bucket{Ts: sec}
	}
	m := sec / 60 * 60
	mb := &s.mins[(m/60)%1440]
	if mb.Ts != m {
		*mb = bucket{Ts: m}
	}
	return b, mb
}

func add(b *bucket, e *Entry) {
	b.Req++
	if e.Success {
		b.OK++
	} else {
		b.Fail++
	}
	switch {
	case e.Status >= 500:
		b.S5++
	case e.Status >= 400:
		b.S4++
	case e.Status >= 300:
		b.S3++
	default:
		b.S2++
	}
	us := int64(e.LatencyMs * 1000)
	b.LatSumUs += us
	if us > b.LatMaxUs {
		b.LatMaxUs = us
	}
	if e.BackendID != "" && e.BackendIndex >= 0 && e.BackendIndex < MaxBackends {
		b.PB[e.BackendIndex]++
	}
}

func (s *Series) Record(t time.Time, e *Entry) {
	s.mu.Lock()
	b, mb := s.at(t.Unix())
	add(b, e)
	add(mb, e)
	s.mu.Unlock()
}

// Sample stores gauge values (active connections, healthy backend count) once per second.
func (s *Series) Sample(t time.Time, active int64, healthy int) {
	s.mu.Lock()
	b, mb := s.at(t.Unix())
	if active > b.Active {
		b.Active = active
	}
	if active > mb.Active {
		mb.Active = active
	}
	b.Healthy = healthy
	if mb.Healthy == 0 || healthy < mb.Healthy {
		mb.Healthy = healthy
	}
	s.mu.Unlock()
}

// RecentRequests sums requests over the last n COMPLETED seconds.
func (s *Series) RecentRequests(n int) int64 {
	now := time.Now().Unix()
	s.mu.Lock()
	defer s.mu.Unlock()
	var sum int64
	for t := now - int64(n); t < now; t++ {
		if b := &s.secs[t%3600]; b.Ts == t {
			sum += b.Req
		}
	}
	return sum
}

type rangeDef struct {
	span, step int64
	minutes    bool
}

var ranges = map[string]rangeDef{
	"1m": {60, 1, false}, "5m": {300, 5, false}, "15m": {900, 15, false}, "1h": {3600, 60, false}, "24h": {86400, 900, true},
}

func (s *Series) Query(name string, nBackends int) []Point {
	rd, ok := ranges[name]
	if !ok {
		rd = ranges["5m"]
	}
	now := time.Now().Unix()
	end := now/rd.step*rd.step + rd.step
	n := int(rd.span / rd.step)
	out := make([]Point, 0, n)
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := 0; i < n; i++ {
		start := end - int64(n-i)*rd.step
		p := Point{T: start, PerBackend: make([]int64, nBackends)}
		var latSum, latMax int64
		unit := int64(1)
		if rd.minutes {
			unit = 60
		}
		for t := start; t < start+rd.step; t += unit {
			var b *bucket
			if rd.minutes {
				b = &s.mins[(t/60)%1440]
			} else {
				b = &s.secs[t%3600]
			}
			if b.Ts != t {
				continue
			}
			p.Req += b.Req
			p.OK += b.OK
			p.Fail += b.Fail
			p.S2 += b.S2
			p.S3 += b.S3
			p.S4 += b.S4
			p.S5 += b.S5
			latSum += b.LatSumUs
			if b.LatMaxUs > latMax {
				latMax = b.LatMaxUs
			}
			for k := 0; k < nBackends && k < MaxBackends; k++ {
				p.PerBackend[k] += b.PB[k]
			}
			if b.Active > p.Active {
				p.Active = b.Active
			}
			if p.Healthy == 0 || (b.Healthy > 0 && b.Healthy < p.Healthy) {
				p.Healthy = b.Healthy
			}
		}
		p.RPS = float64(p.Req) / float64(rd.step)
		if p.Req > 0 {
			p.AvgMs = float64(latSum) / float64(p.Req) / 1000
		}
		p.MaxMs = float64(latMax) / 1000
		out = append(out, p)
	}
	return out
}

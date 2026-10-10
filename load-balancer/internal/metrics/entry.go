// Package metrics collects real request metrics: counters, latency histograms, time series,
// the live request feed and load-test statistics.
package metrics

import (
	"math"
	"sort"
	"sync/atomic"
	"time"
)

// Entry is one proxied request as seen by the load balancer. It is the single source of truth for the
// dashboard, the request log table and the database request_logs table.
type Entry struct {
	Seq           int64     `json:"seq"`
	Time          time.Time `json:"time"`
	RequestID     string    `json:"request_id"`
	UserID        int64     `json:"user_id,omitempty"`
	UserName      string    `json:"user_name"`
	IP            string    `json:"ip"`
	Method        string    `json:"method"`
	Path          string    `json:"path"`
	Action        string    `json:"action"`
	Detail        string    `json:"detail"`
	OrderID       string    `json:"order_id"`
	BackendID     string    `json:"backend_id"`
	BackendIndex  int       `json:"-"`
	Status        int       `json:"status"`
	LatencyMs     float64   `json:"latency_ms"`
	ReqSize       int64     `json:"request_size"`
	RespSize      int64     `json:"response_size"`
	Success       bool      `json:"success"`
	FailureReason string    `json:"failure_reason"`
	Retries       int       `json:"retries"`
	LoadTest      string    `json:"load_test"`
	VU            int       `json:"-"`
}

// Hist is a lock-free log-scale latency histogram (100µs .. ~2min, 15% bucket width).
type Hist struct {
	counts [112]atomic.Int64
	total  atomic.Int64
	sumUs  atomic.Int64
	maxUs  atomic.Int64
}

var bounds = func() []float64 {
	b := make([]float64, 112)
	v := 100.0
	for i := range b {
		b[i] = v
		v *= 1.15
	}
	return b
}()

func (h *Hist) Observe(us int64) {
	i := sort.SearchFloat64s(bounds, float64(us))
	if i >= len(bounds) {
		i = len(bounds) - 1
	}
	h.counts[i].Add(1)
	h.total.Add(1)
	h.sumUs.Add(us)
	for {
		m := h.maxUs.Load()
		if us <= m || h.maxUs.CompareAndSwap(m, us) {
			break
		}
	}
}

// Percentile returns the latency (ms) at quantile q in [0,1].
func (h *Hist) Percentile(q float64) float64 {
	n := h.total.Load()
	if n == 0 {
		return 0
	}
	target := int64(math.Ceil(q * float64(n)))
	if target < 1 {
		target = 1
	}
	var cum int64
	for i := range h.counts {
		cum += h.counts[i].Load()
		if cum >= target {
			v := bounds[i]
			if mx := float64(h.maxUs.Load()); v > mx {
				v = mx
			}
			return v / 1000
		}
	}
	return float64(h.maxUs.Load()) / 1000
}

func (h *Hist) AvgMs() float64 {
	n := h.total.Load()
	if n == 0 {
		return 0
	}
	return float64(h.sumUs.Load()) / float64(n) / 1000
}

func (h *Hist) MaxMs() float64      { return float64(h.maxUs.Load()) / 1000 }
func (h *Hist) Count() int64        { return h.total.Load() }
func (h *Hist) SumSeconds() float64 { return float64(h.sumUs.Load()) / 1e6 }

// CumulativeLE returns how many observations are <= le seconds (bucket-resolution approximation).
func (h *Hist) CumulativeLE(le float64) int64 {
	var c int64
	for i := range h.counts {
		if bounds[i]/1e6 <= le {
			c += h.counts[i].Load()
		}
	}
	return c
}

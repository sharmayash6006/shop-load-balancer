// Package algorithms implements the four load-balancing strategies.
package algorithms

import (
	"hash/fnv"
	"sync"
	"sync/atomic"

	"lb/internal/routing"
)

const (
	RoundRobin         = "round_robin"
	WeightedRoundRobin = "weighted_round_robin"
	LeastConnections   = "least_connections"
	IPHash             = "ip_hash"
)

var Names = []string{RoundRobin, WeightedRoundRobin, LeastConnections, IPHash}

func Valid(name string) bool {
	for _, n := range Names {
		if n == name {
			return true
		}
	}
	return false
}

// Selector picks one backend. `all` is the full ordered pool; `excl` lists IDs already tried for this request.
// Only backends that are Available() and not excluded are eligible.
type Selector interface {
	Select(all []*routing.Backend, clientIP string, excl map[string]bool) *routing.Backend
}

func eligible(all []*routing.Backend, excl map[string]bool) []*routing.Backend {
	out := make([]*routing.Backend, 0, len(all))
	for _, b := range all {
		if b.Available() && !excl[b.ID] {
			out = append(out, b)
		}
	}
	return out
}

// Registry maps an algorithm name to its (stateful) selector.
type Registry struct{ m map[string]Selector }

func NewRegistry() *Registry {
	return &Registry{m: map[string]Selector{
		RoundRobin:         &roundRobin{},
		WeightedRoundRobin: &weightedRR{current: map[string]int{}},
		LeastConnections:   &leastConn{},
		IPHash:             ipHash{},
	}}
}

func (r *Registry) Select(name string, all []*routing.Backend, ip string, excl map[string]bool) *routing.Backend {
	s, ok := r.m[name]
	if !ok {
		s = r.m[RoundRobin]
	}
	return s.Select(all, ip, excl)
}

// ---- Round Robin ----

type roundRobin struct{ n atomic.Uint64 }

func (r *roundRobin) Select(all []*routing.Backend, _ string, excl map[string]bool) *routing.Backend {
	el := eligible(all, excl)
	if len(el) == 0 {
		return nil
	}
	return el[(r.n.Add(1)-1)%uint64(len(el))]
}

// ---- Weighted Round Robin (smooth weighted, as used by nginx) ----
// Weights 5/3/1 produce a 5:3:1 split, interleaved rather than in bursts.

type weightedRR struct {
	mu      sync.Mutex
	current map[string]int
}

func (w *weightedRR) Select(all []*routing.Backend, _ string, excl map[string]bool) *routing.Backend {
	el := eligible(all, excl)
	if len(el) == 0 {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	total := 0
	var best *routing.Backend
	for _, b := range el {
		wt := b.Weight()
		w.current[b.ID] += wt
		total += wt
		if best == nil || w.current[b.ID] > w.current[best.ID] {
			best = b
		}
	}
	w.current[best.ID] -= total
	return best
}

// ---- Least Connections ----

type leastConn struct{ tie atomic.Uint64 }

func (l *leastConn) Select(all []*routing.Backend, _ string, excl map[string]bool) *routing.Backend {
	el := eligible(all, excl)
	if len(el) == 0 {
		return nil
	}
	start := int(l.tie.Add(1) % uint64(len(el))) // rotate the starting point so ties are spread out
	best := el[start]
	for i := 1; i < len(el); i++ {
		b := el[(start+i)%len(el)]
		if b.Active.Load() < best.Active.Load() {
			best = b
		}
	}
	return best
}

// ---- IP Hash ----
// The hash is taken over the FULL pool so a client's primary server never changes just because another server
// is down. If the primary is unavailable we probe forward to the next available one, and the client returns to
// its primary as soon as it is healthy again.

type ipHash struct{}

func (ipHash) Select(all []*routing.Backend, ip string, excl map[string]bool) *routing.Backend {
	if len(all) == 0 {
		return nil
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(ip))
	start := int(h.Sum32() % uint32(len(all)))
	for i := 0; i < len(all); i++ {
		b := all[(start+i)%len(all)]
		if b.Available() && !excl[b.ID] {
			return b
		}
	}
	return nil
}

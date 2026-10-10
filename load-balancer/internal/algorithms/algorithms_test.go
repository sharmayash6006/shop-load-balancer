package algorithms

import (
	"fmt"
	"testing"

	"lb/internal/routing"
)

func pool(t *testing.T, weights ...int) []*routing.Backend {
	t.Helper()
	var out []*routing.Backend
	for i, w := range weights {
		b, err := routing.New(i, fmt.Sprintf("http://backend-%d:800%d", i+1, i+1), w)
		if err != nil {
			t.Fatal(err)
		}
		b.RecordCheck(true, 1, "", 2, 2) // RECOVERING -> HEALTHY
		out = append(out, b)
	}
	return out
}

func count(sel func() *routing.Backend, n int) map[string]int {
	m := map[string]int{}
	for i := 0; i < n; i++ {
		if b := sel(); b != nil {
			m[b.ID]++
		}
	}
	return m
}

func TestRoundRobinIsEven(t *testing.T) {
	bs := pool(t, 1, 1, 1)
	r := NewRegistry()
	got := count(func() *routing.Backend { return r.Select(RoundRobin, bs, "1.1.1.1", nil) }, 300)
	for _, b := range bs {
		if got[b.ID] != 100 {
			t.Fatalf("expected 100 each, got %v", got)
		}
	}
}

func TestRoundRobinSkipsUnavailable(t *testing.T) {
	bs := pool(t, 1, 1, 1)
	bs[2].SetEnabled(false)
	r := NewRegistry()
	got := count(func() *routing.Backend { return r.Select(RoundRobin, bs, "x", nil) }, 100)
	if got["backend-3"] != 0 || got["backend-1"] != 50 || got["backend-2"] != 50 {
		t.Fatalf("unexpected %v", got)
	}
}

func TestWeightedRoundRobinRatio(t *testing.T) {
	bs := pool(t, 5, 3, 1)
	r := NewRegistry()
	got := count(func() *routing.Backend { return r.Select(WeightedRoundRobin, bs, "x", nil) }, 900)
	if got["backend-1"] != 500 || got["backend-2"] != 300 || got["backend-3"] != 100 {
		t.Fatalf("expected 500/300/100, got %v", got)
	}
}

func TestLeastConnectionsPicksLowest(t *testing.T) {
	bs := pool(t, 1, 1, 1)
	bs[0].Active.Store(12)
	bs[1].Active.Store(5)
	bs[2].Active.Store(8)
	r := NewRegistry()
	for i := 0; i < 20; i++ {
		if b := r.Select(LeastConnections, bs, "x", nil); b.ID != "backend-2" {
			t.Fatalf("expected backend-2, got %s", b.ID)
		}
	}
}

func TestIPHashStickyAndRemaps(t *testing.T) {
	bs := pool(t, 1, 1, 1)
	r := NewRegistry()
	ip := "203.0.113.7"
	first := r.Select(IPHash, bs, ip, nil)
	for i := 0; i < 50; i++ {
		if r.Select(IPHash, bs, ip, nil).ID != first.ID {
			t.Fatal("same IP must map to the same backend")
		}
	}
	first.SetEnabled(false) // primary goes away -> remapped to a healthy one
	second := r.Select(IPHash, bs, ip, nil)
	if second == nil || second.ID == first.ID {
		t.Fatalf("expected remap, got %v", second)
	}
	for i := 0; i < 20; i++ {
		if r.Select(IPHash, bs, ip, nil).ID != second.ID {
			t.Fatal("remap must be stable too")
		}
	}
	first.SetEnabled(true)
	first.RecordCheck(true, 1, "", 2, 2) // healthy again -> client returns to its primary
	if r.Select(IPHash, bs, ip, nil).ID != first.ID {
		t.Fatal("client should return to its original backend")
	}
}

func TestNoBackendsReturnsNil(t *testing.T) {
	bs := pool(t, 1, 1)
	for _, b := range bs {
		b.SetEnabled(false)
	}
	r := NewRegistry()
	for _, n := range Names {
		if r.Select(n, bs, "x", nil) != nil {
			t.Fatalf("%s should return nil when nothing is available", n)
		}
	}
}

func TestExclusion(t *testing.T) {
	bs := pool(t, 1, 1)
	r := NewRegistry()
	b := r.Select(RoundRobin, bs, "x", map[string]bool{"backend-1": true})
	if b == nil || b.ID != "backend-2" {
		t.Fatalf("expected backend-2, got %v", b)
	}
}

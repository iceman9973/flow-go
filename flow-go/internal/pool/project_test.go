package pool

import (
	"context"
	"testing"
)

// A worker carries the project it generates into, so the result of a routed job
// can name the project the job actually ran in.
//
// Empty is a real state and not the same as "never asked": the engine's project
// resolution consults a listing and a browser and can come back with nothing, so
// a worker with no project has to be distinguishable from one that has not been
// given a chance yet.
func TestWorkerProjectIDRoundTrips(t *testing.T) {
	w := newTestWorker("acct-a")

	if got := w.ProjectID(); got != "" {
		t.Errorf("ProjectID() = %q before any SetProjectID, want empty", got)
	}

	w.SetProjectID("project-a")
	if got := w.ProjectID(); got != "project-a" {
		t.Errorf("ProjectID() = %q, want project-a", got)
	}

	// Re-pointing a worker is how a re-bootstrap that resolved a different
	// project lands. The second value has to win.
	w.SetProjectID("project-a2")
	if got := w.ProjectID(); got != "project-a2" {
		t.Errorf("ProjectID() = %q after a second Set, want project-a2", got)
	}
}

// The project belongs to the worker and not to the pool. Two accounts signed into
// one browser have different project lists, and one worker's id must never be
// read back off another — which is exactly what a pool-level field would do.
func TestProjectsArePerWorker(t *testing.T) {
	p := New()
	a := newTestWorker("acct-a")
	b := newTestWorker("acct-b")
	a.SetProjectID("project-a")
	b.SetProjectID("project-b")
	p.Register(a)
	p.Register(b)

	workers := p.Workers()
	if len(workers) != 2 {
		t.Fatalf("Workers = %d, want 2", len(workers))
	}
	for _, tc := range []struct{ id, want string }{
		{"acct-a", "project-a"},
		{"acct-b", "project-b"},
	} {
		var found bool
		for _, w := range workers {
			if w.ID != tc.id {
				continue
			}
			found = true
			if got := w.ProjectID(); got != tc.want {
				t.Errorf("worker %s ProjectID() = %q, want %q", tc.id, got, tc.want)
			}
		}
		if !found {
			t.Errorf("worker %s is not registered", tc.id)
		}
	}
}

// /status reports one row per worker, and the project has to be on the row. With
// two accounts registered a single pool-level project would be a guess at which
// one a given worker routes to.
func TestStatsReportEachWorkersProject(t *testing.T) {
	p := New()
	a := newTestWorker("acct-a")
	a.SetProjectID("project-a")
	b := newTestWorker("acct-b")
	p.Register(a)
	p.Register(b)

	stats := p.Stats()
	if len(stats.Workers) != 2 {
		t.Fatalf("Workers = %d, want 2", len(stats.Workers))
	}

	byID := map[string]string{}
	for _, ws := range stats.Workers {
		byID[ws.ID] = ws.ProjectID
	}
	if byID["acct-a"] != "project-a" {
		t.Errorf("acct-a project = %q, want project-a", byID["acct-a"])
	}
	// Registered with no project set: reported empty rather than borrowing a
	// peer's, which would name a project the worker cannot generate into.
	if byID["acct-b"] != "" {
		t.Errorf("acct-b project = %q, want empty — a worker must not inherit another's project",
			byID["acct-b"])
	}
}

// Registration is additive, and this is the behaviour that replaced the engine's
// Retain call: Bootstrap now registers every account it discovers and keeps the
// rest so the pool can route between them. If Register went back to collapsing
// the pool, discovering a second account would silently disable the first.
func TestRegisteringASecondAccountKeepsTheFirst(t *testing.T) {
	p := New()
	p.Register(newTestWorker("acct-a"))
	p.Register(newTestWorker("acct-b"))

	if p.Size() != 2 {
		t.Fatalf("Size = %d, want 2 — registering a second account must not drop the first", p.Size())
	}

	// And both are reachable, which is the whole point of keeping them.
	seen := map[string]bool{}
	for i := 0; i < p.Size(); i++ {
		w, err := p.Acquire(context.Background(), 0)
		if err != nil {
			t.Fatalf("Acquire %d failed: %v", i, err)
		}
		seen[w.ID] = true
		p.Release(w, 0, nil)
	}
	if !seen["acct-a"] || !seen["acct-b"] {
		t.Errorf("routing only reached %v; both registered accounts should be usable", seen)
	}
}

// Re-registering an account replaces that worker in place rather than adding a
// second row for it. Bootstrap runs on every cookie sync, so appending would
// grow the pool by one entry per sync and inflate the balance /status reports.
func TestReRegisteringAnAccountReplacesInPlace(t *testing.T) {
	p := New()
	p.Register(newTestWorker("acct-a"))
	p.Register(newTestWorker("acct-b"))

	replacement := newTestWorker("acct-a")
	replacement.SetProjectID("project-a2")
	p.Register(replacement)

	if p.Size() != 2 {
		t.Fatalf("Size = %d, want 2 — a re-register must replace, not append", p.Size())
	}
	var found bool
	for _, w := range p.Workers() {
		if w.ID != "acct-a" {
			continue
		}
		found = true
		if got := w.ProjectID(); got != "project-a2" {
			t.Errorf("acct-a project = %q, want project-a2 — the replacement did not take", got)
		}
	}
	if !found {
		t.Error("acct-a is missing after being re-registered")
	}
}

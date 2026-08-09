package ingress

import (
	"path/filepath"
	"testing"
	"time"
)

func TestSeedDoesNotOverwriteObservation(t *testing.T) {
	s := NewStats(100)
	s.Ingest(line("a", "10.0.0.1:80", 200, 1), t0)
	s.Seed("a", "10.0.0.1:80", t0.Add(time.Hour)) // config sweep after real traffic
	if !s.UpstreamLastSeen[upstreamKey{"a", "10.0.0.1:80"}].Equal(t0) {
		t.Fatal("seed overwrote real observation")
	}
	// Never-seen pair gets the watching-since clock.
	s.Seed("b", "10.0.0.2:80", t0)
	if !s.UpstreamLastSeen[upstreamKey{"b", "10.0.0.2:80"}].Equal(t0) {
		t.Fatal("seed did not register configured pair")
	}
	if !s.VhostLastSeen[vhostKey{"b"}].Equal(t0) {
		t.Fatal("seed did not register vhost")
	}
}

func TestStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s := NewStats(100)
	s.Ingest(line("a", "10.0.0.1:80", 200, 1), t0)
	s.Seed("b", "10.0.0.2:80", t0.Add(time.Minute))
	if err := s.SaveState(path); err != nil {
		t.Fatal(err)
	}

	fresh := NewStats(100)
	if err := fresh.LoadState(path); err != nil {
		t.Fatal(err)
	}
	if !fresh.UpstreamLastSeen[upstreamKey{"a", "10.0.0.1:80"}].Equal(time.Unix(t0.Unix(), 0)) {
		t.Fatalf("upstream clock lost: %v", fresh.UpstreamLastSeen)
	}
	if !fresh.VhostLastSeen[vhostKey{"b"}].Equal(time.Unix(t0.Add(time.Minute).Unix(), 0)) {
		t.Fatalf("seeded vhost clock lost: %v", fresh.VhostLastSeen)
	}

	// Loading an older state must not rewind a fresher in-memory clock.
	later := t0.Add(time.Hour)
	fresh.Ingest(line("a", "10.0.0.1:80", 200, 1), later)
	if err := fresh.LoadState(path); err != nil {
		t.Fatal(err)
	}
	if !fresh.UpstreamLastSeen[upstreamKey{"a", "10.0.0.1:80"}].Equal(later) {
		t.Fatal("stale state rewound a fresher clock")
	}
}

func TestLoadStateMissingFile(t *testing.T) {
	s := NewStats(100)
	if err := s.LoadState(filepath.Join(t.TempDir(), "absent.json")); err != nil {
		t.Fatalf("missing state file should not error: %v", err)
	}
}

func TestPruneSeeds(t *testing.T) {
	s := NewStats(100)
	s.Seed("a", "stale.host:80", t0)               // seeded under old identity, never observed
	s.Seed("a", "10.0.0.1:80", t0)                 // current seed, never observed
	s.Ingest(line("a", "10.0.0.2:80", 200, 1), t0) // real traffic, not in seed set

	s.PruneSeeds(map[UpstreamKey]bool{UpstreamSeedKey("a", "10.0.0.1:80"): true})

	if _, ok := s.UpstreamLastSeen[UpstreamSeedKey("a", "stale.host:80")]; ok {
		t.Fatal("stale never-observed seed not pruned")
	}
	if _, ok := s.UpstreamLastSeen[UpstreamSeedKey("a", "10.0.0.1:80")]; !ok {
		t.Fatal("current seed wrongly pruned")
	}
	if _, ok := s.UpstreamLastSeen[UpstreamSeedKey("a", "10.0.0.2:80")]; !ok {
		t.Fatal("observed-traffic entry wrongly pruned — that IS the decommission signal")
	}
}

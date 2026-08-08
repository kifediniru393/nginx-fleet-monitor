package ingress

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// persistedState is the on-disk form of the idle-clock timestamps. Only
// last-seen times are persisted: they are the decommission evidence and must
// survive restarts, or a weekly restart would reset every 5-day idle clock.
// Counters are not persisted — Prometheus handles counter resets natively.
type persistedState struct {
	Vhosts    map[string]int64 `json:"vhosts"`    // vhost -> unix seconds
	Upstreams map[string]int64 `json:"upstreams"` // "vhost\x00addr" -> unix seconds
}

// Seed registers a configured vhost/upstream pair that has not been observed
// serving traffic yet, starting its idle clock at now ("no traffic since we
// began watching"). Already-observed entries are untouched, so real traffic
// evidence always wins over seeding.
func (s *Stats) Seed(vhost, addr string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	vh := vhostKey{vhost}
	if _, ok := s.VhostLastSeen[vh]; !ok {
		s.VhostLastSeen[vh] = now
	}
	if addr == "" {
		return
	}
	key := upstreamKey{vhost, addr}
	if _, ok := s.UpstreamLastSeen[key]; !ok {
		s.UpstreamLastSeen[key] = now
	}
}

// SaveState atomically writes the idle-clock timestamps to path.
func (s *Stats) SaveState(path string) error {
	s.mu.Lock()
	st := persistedState{Vhosts: map[string]int64{}, Upstreams: map[string]int64{}}
	for vh, t := range s.VhostLastSeen {
		st.Vhosts[vh.vhost] = t.Unix()
	}
	for k, t := range s.UpstreamLastSeen {
		st.Upstreams[k.vhost+"\x00"+k.addr] = t.Unix()
	}
	s.mu.Unlock()

	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadState restores idle clocks saved by SaveState. Loaded times never
// overwrite fresher in-memory observations. A missing file is not an error.
func (s *Stats) LoadState(path string) error {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var st persistedState
	if err := json.Unmarshal(b, &st); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for vhost, unix := range st.Vhosts {
		vh := vhostKey{vhost}
		t := time.Unix(unix, 0)
		if s.VhostLastSeen[vh].Before(t) {
			s.VhostLastSeen[vh] = t
		}
	}
	for k, unix := range st.Upstreams {
		var vhost, addr string
		for i := 0; i < len(k); i++ {
			if k[i] == 0 {
				vhost, addr = k[:i], k[i+1:]
				break
			}
		}
		if vhost == "" && addr == "" {
			continue
		}
		key := upstreamKey{vhost, addr}
		t := time.Unix(unix, 0)
		if s.UpstreamLastSeen[key].Before(t) {
			s.UpstreamLastSeen[key] = t
		}
	}
	return nil
}

// EnsureStateDir creates the state file's directory if needed.
func EnsureStateDir(path string) error {
	return os.MkdirAll(filepath.Dir(path), 0o755)
}

package vrrp

import (
	"net/netip"
	"sync"
	"time"
)

// VRIDState is the tracked state for one virtual router.
type VRIDState struct {
	Master   netip.Addr
	Priority uint8
	Interval time.Duration
	VIPs     []netip.Addr
	Version  int
	LastSeen time.Time
	Stepdown bool // last advert from the master had priority 0
}

// MasterDownInterval per RFC 5798: 3*interval + skew, skew = (256-prio)/256 * interval.
func (s VRIDState) MasterDownInterval() time.Duration {
	skew := time.Duration(256-int(s.Priority)) * s.Interval / 256
	return 3*s.Interval + skew
}

// MasterAlive reports whether the master has been heard within its
// master-down interval as of now.
func (s VRIDState) MasterAlive(now time.Time) bool {
	return now.Sub(s.LastSeen) <= s.MasterDownInterval()
}

// Transition is a master change on one VRID.
type Transition struct {
	VRID     uint8
	From, To netip.Addr
}

// Tracker folds adverts into per-VRID state and counts master transitions.
type Tracker struct {
	mu          sync.Mutex
	vrids       map[uint8]*VRIDState
	transitions map[Transition]uint64
}

func NewTracker() *Tracker {
	return &Tracker{
		vrids:       make(map[uint8]*VRIDState),
		transitions: make(map[Transition]uint64),
	}
}

// Observe folds one advert into the tracker at time now.
func (t *Tracker) Observe(a *Advert, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.vrids[a.VRID]
	if !ok {
		s = &VRIDState{}
		t.vrids[a.VRID] = s
		t.transitions[Transition{VRID: a.VRID, To: a.Src}]++
	} else if s.Master != a.Src {
		// A different node advertising is a takeover — count it once.
		t.transitions[Transition{VRID: a.VRID, From: s.Master, To: a.Src}]++
	}
	s.Master = a.Src
	s.Priority = a.Priority
	s.Version = a.Version
	s.VIPs = a.VIPs
	s.LastSeen = now
	s.Stepdown = a.Priority == 0
	if a.Interval > 0 {
		s.Interval = a.Interval
	}
}

// Snapshot returns copies of the current per-VRID state and transition counts.
func (t *Tracker) Snapshot() (map[uint8]VRIDState, map[Transition]uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	states := make(map[uint8]VRIDState, len(t.vrids))
	for id, s := range t.vrids {
		states[id] = *s
	}
	trans := make(map[Transition]uint64, len(t.transitions))
	for k, v := range t.transitions {
		trans[k] = v
	}
	return states, trans
}

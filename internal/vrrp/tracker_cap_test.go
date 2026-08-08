package vrrp

import (
	"net/netip"
	"testing"
	"time"
)

// A spoofed-advert flood must not grow the transitions map without bound.
func TestTransitionCardinalityCap(t *testing.T) {
	tr := NewTracker()
	now := time.Now()
	for i := 0; i < maxTransitionKeys+500; i++ {
		src := netip.AddrFrom4([4]byte{10, byte(i >> 16), byte(i >> 8), byte(i)})
		tr.Observe(&Advert{Version: 2, VRID: 51, Priority: 200, Src: src}, now)
	}
	_, trans := tr.Snapshot()
	if len(trans) > maxTransitionKeys {
		t.Fatalf("transitions map grew to %d, cap is %d", len(trans), maxTransitionKeys)
	}
	if tr.Dropped() == 0 {
		t.Fatal("dropped counter did not record the overflow")
	}
	// Existing keys still count through the cap.
	states, _ := tr.Snapshot()
	if states[51].Priority != 200 {
		t.Fatal("state tracking broke under flood")
	}
}

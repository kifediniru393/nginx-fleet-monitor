package vrrp

import (
	"errors"
	"net/netip"
	"testing"
	"time"
)

var (
	srcA = netip.MustParseAddr("10.1.1.1")
	srcB = netip.MustParseAddr("10.1.1.2")
	dst  = netip.MustParseAddr("224.0.0.18")
	vip  = netip.MustParseAddr("10.1.1.100")
)

// buildV2 constructs a valid VRRPv2 advert: interval in whole seconds,
// RFC 3768 checksum over the entire message including the 8 auth bytes.
func buildV2(vrid, prio uint8, intervalSec byte, vips ...netip.Addr) []byte {
	p := []byte{0x21, vrid, prio, byte(len(vips)), 1 /* authtype simple */, intervalSec, 0, 0}
	for _, v := range vips {
		a := v.As4()
		p = append(p, a[:]...)
	}
	p = append(p, []byte("password")...) // auth data
	ck := inetChecksum(p)
	p[6], p[7] = byte(ck>>8), byte(ck)
	return p
}

// buildV3 constructs a valid VRRPv3 advert: 12-bit centisecond interval,
// pseudo-header checksum, no auth fields.
func buildV3(vrid, prio uint8, centis uint16, src netip.Addr, vips ...netip.Addr) []byte {
	p := []byte{0x31, vrid, prio, byte(len(vips)), byte(centis >> 8 & 0x0f), byte(centis), 0, 0}
	for _, v := range vips {
		a := v.As4()
		p = append(p, a[:]...)
	}
	ck := inetChecksum(append(pseudoHeader(src, dst, len(p)), p...))
	p[6], p[7] = byte(ck>>8), byte(ck)
	return p
}

func TestParseV2(t *testing.T) {
	a, err := Parse(buildV2(51, 150, 1, vip), srcA, dst)
	if err != nil {
		t.Fatal(err)
	}
	if a.Version != 2 || a.VRID != 51 || a.Priority != 150 {
		t.Fatalf("got %+v", a)
	}
	if a.Interval != time.Second {
		t.Fatalf("interval = %v, want 1s", a.Interval)
	}
	if len(a.VIPs) != 1 || a.VIPs[0] != vip {
		t.Fatalf("vips = %v", a.VIPs)
	}
}

func TestParseV3CentisecondInterval(t *testing.T) {
	a, err := Parse(buildV3(51, 150, 100, srcA, vip), srcA, dst)
	if err != nil {
		t.Fatal(err)
	}
	if a.Version != 3 {
		t.Fatalf("version = %d", a.Version)
	}
	// 100 centiseconds = 1s. Parsing v3 with v2 offsets would read the low
	// interval byte (100) as 100 *seconds* — assert the branch fired.
	if a.Interval != time.Second {
		t.Fatalf("interval = %v, want 1s (v3 centiseconds misparsed?)", a.Interval)
	}
}

func TestParseV3WithV2OffsetsRejected(t *testing.T) {
	// A v3 packet must not validate under v2 rules. Corrupting the version
	// nibble to 2 on a bare v3 packet is rejected as truncated (v2 expects 8
	// trailing auth bytes); with padding present, the v2 checksum (no
	// pseudo-header) must fail rather than yield plausible garbage.
	p := buildV3(51, 150, 100, srcA, vip)
	p[0] = 0x21
	if _, err := Parse(p, srcA, dst); !errors.Is(err, ErrTruncated) {
		t.Fatalf("bare v3-as-v2: want truncation rejection, got %v", err)
	}
	padded := append(p, make([]byte, 8)...)
	if _, err := Parse(padded, srcA, dst); !errors.Is(err, ErrBadChecksum) {
		t.Fatalf("padded v3-as-v2: want checksum rejection, got %v", err)
	}
}

func TestParseBadChecksum(t *testing.T) {
	for name, p := range map[string][]byte{
		"v2": buildV2(51, 150, 1, vip),
		"v3": buildV3(51, 150, 100, srcA, vip),
	} {
		p[2] ^= 0xff // corrupt priority
		if _, err := Parse(p, srcA, dst); !errors.Is(err, ErrBadChecksum) {
			t.Errorf("%s: want ErrBadChecksum, got %v", name, err)
		}
	}
}

func TestParseTruncated(t *testing.T) {
	p := buildV2(51, 150, 1, vip)
	for _, n := range []int{0, 4, 7, 9} {
		if _, err := Parse(p[:n], srcA, dst); !errors.Is(err, ErrTruncated) {
			t.Errorf("len %d: want ErrTruncated, got %v", n, err)
		}
	}
}

func TestParsePriorityZeroStepdown(t *testing.T) {
	a, err := Parse(buildV2(51, 0, 1, vip), srcA, dst)
	if err != nil {
		t.Fatal(err)
	}
	if a.Priority != 0 {
		t.Fatalf("priority = %d", a.Priority)
	}
}

func TestParseMultiVIP(t *testing.T) {
	vip2 := netip.MustParseAddr("10.1.1.101")
	a, err := Parse(buildV2(51, 150, 1, vip, vip2), srcA, dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.VIPs) != 2 || a.VIPs[1] != vip2 {
		t.Fatalf("vips = %v", a.VIPs)
	}
}

func TestTrackerTransitions(t *testing.T) {
	tr := NewTracker()
	now := time.Now()
	advA, _ := Parse(buildV2(51, 150, 1, vip), srcA, dst)
	advB, _ := Parse(buildV2(51, 200, 1, vip), srcB, dst)

	tr.Observe(advA, now)
	tr.Observe(advA, now.Add(time.Second)) // same master: no new transition
	tr.Observe(advB, now.Add(2*time.Second))

	states, trans := tr.Snapshot()
	if s := states[51]; s.Master != srcB || s.Priority != 200 {
		t.Fatalf("state = %+v", s)
	}
	if n := trans[Transition{VRID: 51, From: srcA, To: srcB}]; n != 1 {
		t.Fatalf("takeover counted %d times, want 1", n)
	}
	if n := trans[Transition{VRID: 51, To: srcA}]; n != 1 {
		t.Fatalf("initial election counted %d times, want 1", n)
	}
}

func TestTrackerMasterDown(t *testing.T) {
	tr := NewTracker()
	now := time.Now()
	adv, _ := Parse(buildV2(51, 150, 1, vip), srcA, dst)
	tr.Observe(adv, now)
	states, _ := tr.Snapshot()
	s := states[51]
	if !s.MasterAlive(now.Add(2 * time.Second)) {
		t.Fatal("master should be alive within master-down interval")
	}
	// 3*1s + (256-150)/256*1s ≈ 3.41s
	if s.MasterAlive(now.Add(4 * time.Second)) {
		t.Fatal("master should be down past master-down interval")
	}
}

func TestTrackerVRIDsIndependent(t *testing.T) {
	tr := NewTracker()
	now := time.Now()
	adv51, _ := Parse(buildV2(51, 150, 1, vip), srcA, dst)
	adv52, _ := Parse(buildV2(52, 100, 1, netip.MustParseAddr("10.1.1.101")), srcB, dst)
	tr.Observe(adv51, now)
	tr.Observe(adv52, now)
	states, trans := tr.Snapshot()
	if len(states) != 2 {
		t.Fatalf("want 2 VRIDs, got %d", len(states))
	}
	// nginx-3 on VRID 52 must not count as a takeover on VRID 51.
	if n := trans[Transition{VRID: 51, From: srcA, To: srcB}]; n != 0 {
		t.Fatal("cross-VRID takeover recorded")
	}
}

func TestTrackerStepdownCounter(t *testing.T) {
	tr := NewTracker()
	now := time.Now()
	normal, _ := Parse(buildV2(51, 150, 1, vip), srcA, dst)
	prio0, _ := Parse(buildV2(51, 0, 1, vip), srcA, dst)

	tr.Observe(normal, now)
	tr.Observe(prio0, now.Add(time.Second))
	tr.Observe(prio0, now.Add(2*time.Second)) // same stepdown burst: one edge
	tr.Observe(normal, now.Add(3*time.Second))
	tr.Observe(prio0, now.Add(4*time.Second)) // second stepdown event

	if n := tr.Stepdowns()[51]; n != 2 {
		t.Fatalf("stepdowns = %d, want 2 (edge-counted)", n)
	}
}

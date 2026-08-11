package vrrp

import (
	"testing"
)

// FuzzParse hammers the advert parser with arbitrary payloads. The parser
// consumes raw wire bytes from an unauthenticated protocol, so it must never
// panic, and anything it accepts must be internally consistent.
func FuzzParse(f *testing.F) {
	// Seeds: valid v2, priority-0 stepdown, multi-VIP, and truncations.
	f.Add(buildV2(51, 200, 1, vip))
	f.Add(buildV2(51, 0, 1, vip))
	f.Add(buildV2(7, 100, 5, vip, srcA, srcB))
	f.Add(buildV2(51, 200, 1, vip)[:8])
	f.Add([]byte{0x31, 51, 200, 1, 0, 100, 0, 0}) // v3-shaped header
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, payload []byte) {
		a, err := Parse(payload, srcA, dst)
		if err != nil {
			return
		}
		if a == nil {
			t.Fatal("nil advert without error")
		}
		if a.Version != 2 && a.Version != 3 {
			t.Fatalf("accepted unsupported version %d", a.Version)
		}
		if len(a.VIPs) != int(payload[3]) {
			t.Fatalf("VIP count %d != advertised %d", len(a.VIPs), payload[3])
		}
	})
}

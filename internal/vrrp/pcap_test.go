package vrrp

import (
	"encoding/binary"
	"net/netip"
	"os"
	"testing"
)

// TestGoldenPcap replays real keepalived adverts captured on the test fleet
// (lb-svr1, VRID 51, authtype simple) through the parser — the Tier 0 gate.
// This is the fixture that caught the v2 checksum-scope bug: RFC 3768
// checksums the whole message including auth data.
func TestGoldenPcap(t *testing.T) {
	pkts := readPcap(t, "testdata/vrrp-v2-keepalived.pcap")
	if len(pkts) == 0 {
		t.Fatal("no packets in fixture")
	}
	parsed := 0
	for _, frame := range pkts {
		if len(frame) < 34 || binary.BigEndian.Uint16(frame[12:14]) != 0x0800 {
			continue
		}
		ip := frame[14:]
		if ip[9] != IPProtoVRRP {
			continue
		}
		ihl := int(ip[0]&0x0f) * 4
		totLen := int(binary.BigEndian.Uint16(ip[2:4]))
		if totLen > len(ip) {
			totLen = len(ip)
		}
		src, _ := netip.AddrFromSlice(ip[12:16])
		dst, _ := netip.AddrFromSlice(ip[16:20])
		a, err := Parse(ip[ihl:totLen], src, dst)
		if err != nil {
			t.Fatalf("real advert rejected: %v", err)
		}
		if a.Version != 2 || a.VRID != 51 || a.Priority != 255 {
			t.Fatalf("parsed %+v", a)
		}
		if len(a.VIPs) != 1 || a.VIPs[0] != netip.MustParseAddr("192.168.2.154") {
			t.Fatalf("vips = %v", a.VIPs)
		}
		parsed++
	}
	if parsed == 0 {
		t.Fatal("no VRRP packets parsed from fixture")
	}
}

// readPcap is a minimal classic-pcap reader (24B global header, 16B per
// packet) — enough for fixtures, no dependency.
func readPcap(t *testing.T, path string) [][]byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) < 24 {
		t.Fatal("short pcap")
	}
	var order binary.ByteOrder
	switch binary.LittleEndian.Uint32(b[0:4]) {
	case 0xa1b2c3d4, 0xa1b23c4d:
		order = binary.LittleEndian
	case 0xd4c3b2a1, 0x4d3cb2a1:
		order = binary.BigEndian
	default:
		t.Fatal("not a classic pcap")
	}
	var pkts [][]byte
	off := 24
	for off+16 <= len(b) {
		capLen := int(order.Uint32(b[off+8 : off+12]))
		off += 16
		if off+capLen > len(b) {
			break
		}
		pkts = append(pkts, b[off:off+capLen])
		off += capLen
	}
	return pkts
}

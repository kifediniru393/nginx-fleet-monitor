// Package vrrp implements a passive VRRP (protocol 112) advert parser and
// cluster state tracker. The parser is pure and platform-independent; the
// listener (AF_PACKET) is Linux-only.
package vrrp

import (
	"errors"
	"fmt"
	"net/netip"
	"time"
)

const (
	IPProtoVRRP    = 112
	typeAdvert     = 1
	multicastGroup = "224.0.0.18"
)

var (
	ErrTruncated   = errors.New("vrrp: packet truncated")
	ErrBadVersion  = errors.New("vrrp: unsupported version")
	ErrBadType     = errors.New("vrrp: not an advertisement")
	ErrBadChecksum = errors.New("vrrp: checksum mismatch")
)

// Advert is one parsed VRRP advertisement.
type Advert struct {
	Version  int
	VRID     uint8
	Priority uint8
	Interval time.Duration
	VIPs     []netip.Addr
	Src      netip.Addr
}

// Parse decodes a VRRP payload (the bytes after the IP header). src and dst
// are taken from the IP header; dst is needed for the v3 pseudo-header
// checksum. The version nibble is branched on before any offset is trusted:
// v2 carries its advert interval as whole seconds at offset 5, v3 as a 12-bit
// centisecond field at offsets 4-5, and their checksums differ (v3 includes an
// IPv4 pseudo-header).
func Parse(payload []byte, src, dst netip.Addr) (*Advert, error) {
	if len(payload) < 8 {
		return nil, ErrTruncated
	}
	version := int(payload[0] >> 4)
	if t := payload[0] & 0x0f; t != typeAdvert {
		return nil, fmt.Errorf("%w: type %d", ErrBadType, t)
	}
	a := &Advert{
		Version:  version,
		VRID:     payload[1],
		Priority: payload[2],
		Src:      src,
	}
	count := int(payload[3])
	need := 8 + 4*count
	switch version {
	case 2:
		a.Interval = time.Duration(payload[5]) * time.Second
		need += 8 // trailing auth data
		if len(payload) < need {
			return nil, ErrTruncated
		}
		// RFC 3768: checksum covers the entire VRRP message, auth data
		// included. Callers must trim payload to the IP total length first —
		// Ethernet padding after the message would break this.
		if inetChecksum(payload[:need]) != 0 {
			return nil, ErrBadChecksum
		}
	case 3:
		centis := (uint16(payload[4]&0x0f) << 8) | uint16(payload[5])
		a.Interval = time.Duration(centis) * 10 * time.Millisecond
		if len(payload) < need {
			return nil, ErrTruncated
		}
		ph := pseudoHeader(src, dst, len(payload))
		if inetChecksum(append(ph, payload...)) != 0 {
			return nil, ErrBadChecksum
		}
	default:
		return nil, fmt.Errorf("%w: %d", ErrBadVersion, version)
	}
	for i := 0; i < count; i++ {
		off := 8 + 4*i
		ip, _ := netip.AddrFromSlice(payload[off : off+4])
		a.VIPs = append(a.VIPs, ip)
	}
	return a, nil
}

func pseudoHeader(src, dst netip.Addr, vrrpLen int) []byte {
	ph := make([]byte, 0, 12)
	s, d := src.As4(), dst.As4()
	ph = append(ph, s[:]...)
	ph = append(ph, d[:]...)
	ph = append(ph, 0, IPProtoVRRP, byte(vrrpLen>>8), byte(vrrpLen))
	return ph
}

func inetChecksum(b []byte) uint16 {
	sum := 0
	for i := 0; i+1 < len(b); i += 2 {
		sum += int(b[i])<<8 | int(b[i+1])
	}
	if len(b)%2 == 1 {
		sum += int(b[len(b)-1]) << 8
	}
	for sum>>16 > 0 {
		sum = sum&0xffff + sum>>16
	}
	return ^uint16(sum)
}

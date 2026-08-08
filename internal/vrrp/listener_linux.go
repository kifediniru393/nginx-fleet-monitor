//go:build linux

package vrrp

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"time"

	"golang.org/x/sys/unix"
)

// Listen captures IPv4 protocol-112 packets via AF_PACKET and feeds them into
// the tracker. AF_PACKET (rather than a multicast-group raw socket) sees both
// multicast adverts and unicast_peer adverts addressed to this node.
// ALLMULTI membership is set so the NIC doesn't filter 224.0.0.18 frames.
// Requires CAP_NET_RAW. Blocks until ctx is cancelled.
func Listen(ctx context.Context, tr *Tracker) error {
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_DGRAM, int(htons(unix.ETH_P_IP)))
	if err != nil {
		return fmt.Errorf("vrrp: AF_PACKET socket (need CAP_NET_RAW): %w", err)
	}
	defer unix.Close(fd)

	setAllMulti(fd)

	go func() {
		<-ctx.Done()
		unix.Shutdown(fd, unix.SHUT_RD)
	}()

	buf := make([]byte, 65536)
	for {
		n, _, err := unix.Recvfrom(fd, buf, 0)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			return fmt.Errorf("vrrp: recvfrom: %w", err)
		}
		pkt := buf[:n]
		if len(pkt) < 20 || pkt[0]>>4 != 4 || pkt[9] != IPProtoVRRP {
			continue
		}
		ihl := int(pkt[0]&0x0f) * 4
		if len(pkt) < ihl {
			continue
		}
		src, _ := netip.AddrFromSlice(pkt[12:16])
		dst, _ := netip.AddrFromSlice(pkt[16:20])
		if a, err := Parse(pkt[ihl:], src, dst); err == nil {
			tr.Observe(a, time.Now())
		}
	}
}

func setAllMulti(fd int) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return
	}
	for _, ifc := range ifaces {
		mreq := unix.PacketMreq{Ifindex: int32(ifc.Index), Type: unix.PACKET_MR_ALLMULTI}
		unix.SetsockoptPacketMreq(fd, unix.SOL_PACKET, unix.PACKET_ADD_MEMBERSHIP, &mreq)
	}
}

func htons(v uint16) uint16 { return v<<8 | v>>8 }

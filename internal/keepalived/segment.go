package keepalived

import "net"

// SegmentForInterface returns the L2 segment identity for an interface as its
// first IPv4 network in CIDR form (e.g. "192.168.2.0/24"). VRIDs are only
// unique per segment — VRRP has 255 of them, so a 200-VM estate spanning
// VLANs can legitimately reuse a VRID on different subnets; this label keeps
// such clusters distinct when grouping by (segment, vrid). Empty if the
// interface is missing or has no IPv4 address.
func SegmentForInterface(name string) string {
	ifc, err := net.InterfaceByName(name)
	if err != nil {
		return ""
	}
	addrs, err := ifc.Addrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
			masked := &net.IPNet{IP: ipn.IP.Mask(ipn.Mask), Mask: ipn.Mask}
			return masked.String()
		}
	}
	return ""
}

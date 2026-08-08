// Package keepalived parses the local keepalived.conf for cluster membership.
// Passive VRRP observation reveals only masters (backups never advertise), so
// "which VRIDs is this node a member of" must come from the node's own
// config — the wire then cross-checks who currently masters them.
package keepalived

import (
	"os"
	"strconv"
	"strings"
)

// Instance is one vrrp_instance block.
type Instance struct {
	Name     string
	VRID     int
	Priority int
	VIPs     []string
	Unicast  bool // unicast_peer present: multicast listening won't hear peers
}

// ParseFile reads keepalived.conf. A missing file returns (nil, nil): not
// running keepalived is a normal state, not an error.
func ParseFile(path string) ([]Instance, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return parse(string(b)), nil
}

func parse(text string) []Instance {
	var out []Instance
	var cur *Instance
	depth := 0
	instDepth := 0
	inVIPs := false

	for _, raw := range strings.Split(text, "\n") {
		line := raw
		if i := strings.IndexAny(line, "#!"); i >= 0 {
			line = line[:i]
		}
		f := strings.Fields(line)
		open := strings.Contains(line, "{")
		closeB := strings.Contains(line, "}")

		switch {
		case len(f) >= 2 && f[0] == "vrrp_instance":
			out = append(out, Instance{Name: f[1]})
			cur = &out[len(out)-1]
			instDepth = depth
		case cur != nil && len(f) >= 1 && f[0] == "virtual_ipaddress":
			inVIPs = true
		case cur != nil && inVIPs && len(f) >= 1 && !closeB:
			// first token is "ip" or "ip/prefix"
			cur.VIPs = append(cur.VIPs, strings.SplitN(f[0], "/", 2)[0])
		case cur != nil && len(f) >= 2 && f[0] == "virtual_router_id":
			cur.VRID, _ = strconv.Atoi(f[1])
		case cur != nil && len(f) >= 2 && f[0] == "priority":
			cur.Priority, _ = strconv.Atoi(f[1])
		case cur != nil && len(f) >= 1 && strings.HasPrefix(f[0], "unicast_peer"):
			cur.Unicast = true
		}

		if open {
			depth++
		}
		if closeB {
			depth--
			if inVIPs {
				inVIPs = false
			} else if cur != nil && depth <= instDepth {
				cur = nil
			}
		}
	}
	return out
}

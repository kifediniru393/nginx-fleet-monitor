package collectors

import (
	"net"
	"net/netip"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/techmoose/nginx-fleet-exporter/internal/vrrp"
)

var (
	vrrpEnabledDesc = prometheus.NewDesc("nginx_fleet_vrrp_enabled",
		"1 if the VRRP module is running.", nil, nil)
	vrrpMasterDesc = prometheus.NewDesc("nginx_fleet_vrrp_master",
		"1 if node last advertised as master for the VRID and is within its master-down interval.",
		[]string{"vrid", "node", "vip", "observer"}, nil)
	vrrpPriorityDesc = prometheus.NewDesc("nginx_fleet_vrrp_priority",
		"Advertised priority of the current master.", []string{"vrid", "node", "observer"}, nil)
	vrrpIntervalDesc = prometheus.NewDesc("nginx_fleet_vrrp_advert_interval_seconds",
		"Advertised interval.", []string{"vrid", "observer"}, nil)
	vrrpAgeDesc = prometheus.NewDesc("nginx_fleet_vrrp_last_advert_age_seconds",
		"Seconds since the last advert from the current master.", []string{"vrid", "node", "observer"}, nil)
	vrrpVersionDesc = prometheus.NewDesc("nginx_fleet_vrrp_advert_version",
		"VRRP protocol version observed.", []string{"vrid", "observer"}, nil)
	vrrpTransitionsDesc = prometheus.NewDesc("nginx_fleet_vrrp_transitions_total",
		"Master transitions per VRID, including initial election (empty from_node).",
		[]string{"vrid", "from_node", "to_node", "observer"}, nil)
	vrrpStepdownDesc = prometheus.NewDesc("nginx_fleet_vrrp_stepdown",
		"1 if the last advert was a priority-0 graceful stepdown (VIP about to move).",
		[]string{"vrid", "node", "observer"}, nil)
	activeDesc = prometheus.NewDesc("nginx_fleet_active",
		"1 if this node is currently the active/serving node.", []string{"node", "method"}, nil)
)

// VRRPCollector exposes the passive listener's tracker state. When the
// listener is not running (module off, non-Linux, or socket failure) it emits
// only vrrp_enabled=0 plus a static active=1 — the rest of the exporter is
// unaffected.
type VRRPCollector struct {
	Tracker  *vrrp.Tracker
	Enabled  func() bool
	Hostname string
	localIPs map[netip.Addr]bool
}

func NewVRRPCollector(tr *vrrp.Tracker, enabled func() bool, hostname string) *VRRPCollector {
	c := &VRRPCollector{Tracker: tr, Enabled: enabled, Hostname: hostname, localIPs: map[netip.Addr]bool{}}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok {
				if ip, ok := netip.AddrFromSlice(ipn.IP.To4()); ok {
					c.localIPs[ip] = true
				}
			}
		}
	}
	return c
}

func (c *VRRPCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- vrrpEnabledDesc
	ch <- vrrpMasterDesc
	ch <- vrrpPriorityDesc
	ch <- vrrpIntervalDesc
	ch <- vrrpAgeDesc
	ch <- vrrpVersionDesc
	ch <- vrrpTransitionsDesc
	ch <- vrrpStepdownDesc
	ch <- activeDesc
}

func (c *VRRPCollector) Collect(ch chan<- prometheus.Metric) {
	if !c.Enabled() {
		ch <- prometheus.MustNewConstMetric(vrrpEnabledDesc, prometheus.GaugeValue, 0)
		// Without VRRP there is no VIP: this node is always serving.
		ch <- prometheus.MustNewConstMetric(activeDesc, prometheus.GaugeValue, 1, c.Hostname, "static")
		return
	}
	ch <- prometheus.MustNewConstMetric(vrrpEnabledDesc, prometheus.GaugeValue, 1)

	now := time.Now()
	states, trans := c.Tracker.Snapshot()
	obs := c.Hostname
	active := 0.0

	for vrid, s := range states {
		v := strconv.Itoa(int(vrid))
		node := s.Master.String()
		masterVal := 0.0
		if s.MasterAlive(now) && !s.Stepdown {
			masterVal = 1
		}
		if masterVal == 1 && c.localIPs[s.Master] {
			active = 1
		}
		for _, vip := range s.VIPs {
			ch <- prometheus.MustNewConstMetric(vrrpMasterDesc, prometheus.GaugeValue, masterVal, v, node, vip.String(), obs)
		}
		ch <- prometheus.MustNewConstMetric(vrrpPriorityDesc, prometheus.GaugeValue, float64(s.Priority), v, node, obs)
		ch <- prometheus.MustNewConstMetric(vrrpIntervalDesc, prometheus.GaugeValue, s.Interval.Seconds(), v, obs)
		ch <- prometheus.MustNewConstMetric(vrrpAgeDesc, prometheus.GaugeValue, now.Sub(s.LastSeen).Seconds(), v, node, obs)
		ch <- prometheus.MustNewConstMetric(vrrpVersionDesc, prometheus.GaugeValue, float64(s.Version), v, obs)
		stepdown := 0.0
		if s.Stepdown {
			stepdown = 1
		}
		ch <- prometheus.MustNewConstMetric(vrrpStepdownDesc, prometheus.GaugeValue, stepdown, v, node, obs)
	}
	for tr, n := range trans {
		from := ""
		if tr.From.IsValid() {
			from = tr.From.String()
		}
		ch <- prometheus.MustNewConstMetric(vrrpTransitionsDesc, prometheus.CounterValue, float64(n),
			strconv.Itoa(int(tr.VRID)), from, tr.To.String(), obs)
	}
	ch <- prometheus.MustNewConstMetric(activeDesc, prometheus.GaugeValue, active, c.Hostname, "vrrp")
}

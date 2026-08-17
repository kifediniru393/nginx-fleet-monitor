package collectors

import (
	"net"
	"net/netip"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/techmoose/nginx-fleet-exporter/internal/keepalived"
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
	vrrpDroppedDesc = prometheus.NewDesc("nginx_fleet_vrrp_transitions_dropped_total",
		"Transition observations discarded by the cardinality cap (spoofed-advert flood guard).",
		[]string{"observer"}, nil)
	vrrpStepdownsTotalDesc = prometheus.NewDesc("nginx_fleet_vrrp_stepdowns_total",
		"Graceful (priority-0) stepdowns observed per VRID — the durable record; the gauge below is often too brief to scrape.",
		[]string{"vrid", "observer"}, nil)
	vrrpStepdownDesc = prometheus.NewDesc("nginx_fleet_vrrp_stepdown",
		"1 if the last advert was a priority-0 graceful stepdown (VIP about to move).",
		[]string{"vrid", "node", "observer"}, nil)
	clusterInfoDesc = prometheus.NewDesc("nginx_fleet_cluster_info",
		"Cluster membership from local keepalived.conf: this node participates in the VRID. "+
			"Passive VRRP cannot see silent backups; membership comes from config, mastership from the wire. "+
			"Group clusters by (segment, vrid): VRIDs are only unique per L2 segment.",
		[]string{"vrid", "vip", "member_node", "vrrp_instance", "segment"}, nil)
	unicastDesc = prometheus.NewDesc("nginx_fleet_vrrp_unicast_configured",
		"1 if the instance uses unicast_peer: peer adverts may not be visible to multicast observers.",
		[]string{"vrid", "vrrp_instance"}, nil)
	activeDesc = prometheus.NewDesc("nginx_fleet_active",
		"1 if this node is currently the active/serving node.", []string{"node", "method"}, nil)
)

// VRRPCollector exposes the passive listener's tracker state. When the
// listener is not running (module off, non-Linux, or socket failure) it emits
// only vrrp_enabled=0 plus a static active=1 — the rest of the exporter is
// unaffected.
type VRRPCollector struct {
	Tracker   *vrrp.Tracker
	Enabled   func() bool
	Hostname  string
	Instances []keepalived.Instance // local keepalived.conf membership
	localIPs  map[netip.Addr]bool
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
	ch <- clusterInfoDesc
	ch <- unicastDesc
	ch <- vrrpEnabledDesc
	ch <- vrrpMasterDesc
	ch <- vrrpPriorityDesc
	ch <- vrrpIntervalDesc
	ch <- vrrpAgeDesc
	ch <- vrrpVersionDesc
	ch <- vrrpTransitionsDesc
	ch <- vrrpStepdownDesc
	ch <- vrrpStepdownsTotalDesc
	ch <- vrrpDroppedDesc
	ch <- activeDesc
}

func (c *VRRPCollector) Collect(ch chan<- prometheus.Metric) {
	// Membership is config-derived: valid even when the wire listener is off.
	for _, inst := range c.Instances {
		v := strconv.Itoa(inst.VRID)
		segment := keepalived.SegmentForInterface(inst.Interface)
		for _, vip := range inst.VIPs {
			ch <- prometheus.MustNewConstMetric(clusterInfoDesc, prometheus.GaugeValue, 1, v, vip, c.Hostname, inst.Name, segment)
		}
		if inst.Unicast {
			ch <- prometheus.MustNewConstMetric(unicastDesc, prometheus.GaugeValue, 1, v, inst.Name)
		}
	}
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
	for vrid, n := range c.Tracker.Stepdowns() {
		ch <- prometheus.MustNewConstMetric(vrrpStepdownsTotalDesc, prometheus.CounterValue, float64(n),
			strconv.Itoa(int(vrid)), obs)
	}
	for tr, n := range trans {
		from := ""
		if tr.From.IsValid() {
			from = tr.From.String()
		}
		ch <- prometheus.MustNewConstMetric(vrrpTransitionsDesc, prometheus.CounterValue, float64(n),
			strconv.Itoa(int(tr.VRID)), from, tr.To.String(), obs)
	}
	ch <- prometheus.MustNewConstMetric(vrrpDroppedDesc, prometheus.CounterValue, float64(c.Tracker.Dropped()), obs)
	ch <- prometheus.MustNewConstMetric(activeDesc, prometheus.GaugeValue, active, c.Hostname, "vrrp")
}

package ingress

import (
	"github.com/prometheus/client_golang/prometheus"
)

var (
	requestsDesc = prometheus.NewDesc("nginx_fleet_ingress_requests_total",
		"Requests per vhost by status class.", []string{"vhost", "status_class"}, nil)
	bytesDesc = prometheus.NewDesc("nginx_fleet_ingress_bytes_total",
		"L7 bytes per vhost ($bytes_sent / $request_length).", []string{"vhost", "direction"}, nil)
	vhostLastDesc = prometheus.NewDesc("nginx_fleet_vhost_last_traffic_timestamp_seconds",
		"Unix time of the last observed request for the vhost.", []string{"vhost"}, nil)
	upReqDesc = prometheus.NewDesc("nginx_fleet_upstream_requests_total",
		"Requests (including retried attempts) per upstream member.", []string{"vhost", "upstream_addr"}, nil)
	upFailDesc = prometheus.NewDesc("nginx_fleet_upstream_failures_total",
		"Empirical upstream failures: next-upstream retries and 502-504 finals.",
		[]string{"vhost", "upstream_addr", "reason"}, nil)
	upLastDesc = prometheus.NewDesc("nginx_fleet_upstream_last_traffic_timestamp_seconds",
		"Unix time of the last observed request to the upstream member.", []string{"vhost", "upstream_addr"}, nil)
	upTimeDesc = prometheus.NewDesc("nginx_fleet_upstream_response_seconds",
		"Histogram of $upstream_response_time per member (buckets give per-member percentiles).",
		[]string{"vhost", "upstream_addr"}, nil)
	upUpDesc = prometheus.NewDesc("nginx_fleet_upstream_up",
		"1 if the most recent evidence for this member is a success. Evidence-based, not a live probe: "+
			"with no recent traffic the verdict is stale — gate alerts on the last_ok/last_fail timestamps.",
		[]string{"vhost", "upstream_addr"}, nil)
	upLastOKDesc = prometheus.NewDesc("nginx_fleet_upstream_last_ok_timestamp_seconds",
		"Unix time of the last observed successful response from the member. Absent until a success is seen; "+
			"now() minus this is the age of the up verdict.", []string{"vhost", "upstream_addr"}, nil)
	upLastFailDesc = prometheus.NewDesc("nginx_fleet_upstream_last_fail_timestamp_seconds",
		"Unix time of the last observed failure (next-upstream retry or 502-504 final) for the member. "+
			"Absent until a failure is seen.", []string{"vhost", "upstream_addr"}, nil)
	unattribDesc = prometheus.NewDesc("nginx_fleet_ingress_unattributed_total",
		"Log lines that could not be attributed.", []string{"reason"}, nil)
	ingressEnabledDesc = prometheus.NewDesc("nginx_fleet_ingress_enabled",
		"1 if the ingress (log-tailing) collector is running.", nil, nil)
)

// Collector exposes the tailer's aggregated stats.
type Collector struct {
	Stats   *Stats
	Enabled func() bool
}

func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- requestsDesc
	ch <- bytesDesc
	ch <- vhostLastDesc
	ch <- upReqDesc
	ch <- upFailDesc
	ch <- upLastDesc
	ch <- upTimeDesc
	ch <- upUpDesc
	ch <- upLastOKDesc
	ch <- upLastFailDesc
	ch <- unattribDesc
	ch <- ingressEnabledDesc
}

func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	if !c.Enabled() {
		ch <- prometheus.MustNewConstMetric(ingressEnabledDesc, prometheus.GaugeValue, 0)
		return
	}
	ch <- prometheus.MustNewConstMetric(ingressEnabledDesc, prometheus.GaugeValue, 1)

	s := c.Stats
	s.mu.Lock()
	defer s.mu.Unlock()

	for vh, classes := range s.Requests {
		for class, n := range classes {
			ch <- prometheus.MustNewConstMetric(requestsDesc, prometheus.CounterValue, float64(n), vh.vhost, class)
		}
	}
	for vh, n := range s.BytesSent {
		ch <- prometheus.MustNewConstMetric(bytesDesc, prometheus.CounterValue, float64(n), vh.vhost, "sent")
	}
	for vh, n := range s.BytesReceived {
		ch <- prometheus.MustNewConstMetric(bytesDesc, prometheus.CounterValue, float64(n), vh.vhost, "received")
	}
	for vh, t := range s.VhostLastSeen {
		ch <- prometheus.MustNewConstMetric(vhostLastDesc, prometheus.GaugeValue, float64(t.Unix()), vh.vhost)
	}
	for k, n := range s.UpstreamRequests {
		ch <- prometheus.MustNewConstMetric(upReqDesc, prometheus.CounterValue, float64(n), k.vhost, k.addr)
	}
	for k, reasons := range s.UpstreamFailures {
		for reason, n := range reasons {
			ch <- prometheus.MustNewConstMetric(upFailDesc, prometheus.CounterValue, float64(n), k.vhost, k.addr, reason)
		}
	}
	for k, t := range s.UpstreamLastSeen {
		ch <- prometheus.MustNewConstMetric(upLastDesc, prometheus.GaugeValue, float64(t.Unix()), k.vhost, k.addr)
		up := 0.0
		if s.Up(k) {
			up = 1
		}
		ch <- prometheus.MustNewConstMetric(upUpDesc, prometheus.GaugeValue, up, k.vhost, k.addr)
	}
	for k, t := range s.UpstreamLastOK {
		ch <- prometheus.MustNewConstMetric(upLastOKDesc, prometheus.GaugeValue, float64(t.Unix()), k.vhost, k.addr)
	}
	for k, t := range s.UpstreamLastFail {
		ch <- prometheus.MustNewConstMetric(upLastFailDesc, prometheus.GaugeValue, float64(t.Unix()), k.vhost, k.addr)
	}
	for k, h := range s.UpstreamTimeHist {
		buckets := map[float64]uint64{}
		var cum uint64
		for i, le := range TimeBuckets {
			cum += h[i]
			buckets[le] = cum
		}
		ch <- prometheus.MustNewConstHistogram(upTimeDesc, s.UpstreamTimeCount[k], s.UpstreamTimeSum[k], buckets, k.vhost, k.addr)
	}
	for reason, n := range s.Unattributed {
		ch <- prometheus.MustNewConstMetric(unattribDesc, prometheus.CounterValue, float64(n), reason)
	}
}

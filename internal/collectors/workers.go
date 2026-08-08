package collectors

import (
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
)

type workerStats struct {
	pid        int
	fdsOpen    int
	fdsLimit   int
	cpuSeconds float64
	rssBytes   float64
}

var (
	fdsOpenDesc = prometheus.NewDesc("nginx_fleet_worker_fds_open",
		"Open file descriptors per nginx worker.", []string{"pid"}, nil)
	fdsLimitDesc = prometheus.NewDesc("nginx_fleet_worker_fds_limit",
		"Soft fd limit per nginx worker.", []string{"pid"}, nil)
	cpuDesc = prometheus.NewDesc("nginx_fleet_worker_cpu_seconds_total",
		"CPU time consumed per nginx worker.", []string{"pid"}, nil)
	rssDesc = prometheus.NewDesc("nginx_fleet_worker_rss_bytes",
		"Resident memory per nginx worker.", []string{"pid"}, nil)
)

// WorkersCollector reads nginx worker capacity signals from /proc at scrape time.
type WorkersCollector struct{}

func (WorkersCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- fdsOpenDesc
	ch <- fdsLimitDesc
	ch <- cpuDesc
	ch <- rssDesc
}

func (WorkersCollector) Collect(ch chan<- prometheus.Metric) {
	for _, w := range listWorkers() {
		pid := strconv.Itoa(w.pid)
		ch <- prometheus.MustNewConstMetric(fdsOpenDesc, prometheus.GaugeValue, float64(w.fdsOpen), pid)
		ch <- prometheus.MustNewConstMetric(fdsLimitDesc, prometheus.GaugeValue, float64(w.fdsLimit), pid)
		ch <- prometheus.MustNewConstMetric(cpuDesc, prometheus.CounterValue, w.cpuSeconds, pid)
		ch <- prometheus.MustNewConstMetric(rssDesc, prometheus.GaugeValue, w.rssBytes, pid)
	}
}

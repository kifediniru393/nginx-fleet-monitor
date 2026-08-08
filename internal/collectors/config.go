// Package collectors implements the exporter's modular Prometheus collectors:
// config (nginx -T topology), workers (/proc capacity), vrrp (optional
// passive cluster identity).
package collectors

import (
	"log/slog"
	"os/exec"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/techmoose/nginx-fleet-exporter/internal/nginxconf"
)

var (
	vhostInfoDesc = prometheus.NewDesc("nginx_fleet_vhost_info",
		"Intended routing topology from the running nginx config (nginx -T).",
		[]string{"vhost", "listen_addr", "listen_port", "tls", "upstream_addr", "config_file"}, nil)
	workerProcsDesc = prometheus.NewDesc("nginx_fleet_worker_processes",
		"Configured worker_processes (0 = auto/unknown).", nil, nil)
	workerConnsDesc = prometheus.NewDesc("nginx_fleet_worker_connections_limit",
		"Configured worker_connections.", nil, nil)
	configScrapeErrDesc = prometheus.NewDesc("nginx_fleet_config_parse_errors_total",
		"Failures running or parsing nginx -T.", nil, nil)
)

// ConfigCollector runs `nginx -T` at most once per interval and serves the
// parsed topology from memory at scrape time. If nginx -T fails (typically:
// unprivileged user cannot read TLS keys that -T validates), it falls back to
// parsing the config from disk with include resolution.
type ConfigCollector struct {
	cmd          []string
	fallbackPath string
	interval     time.Duration

	mu       sync.Mutex
	cfg      *nginxconf.Config
	fetched  time.Time
	failures float64
}

func NewConfigCollector(cmd []string, fallbackPath string, interval time.Duration) *ConfigCollector {
	return &ConfigCollector{cmd: cmd, fallbackPath: fallbackPath, interval: interval}
}

// Config returns the current parsed config (may be nil before first success).
func (c *ConfigCollector) Config() *nginxconf.Config {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refreshLocked()
	return c.cfg
}

func (c *ConfigCollector) refreshLocked() {
	if time.Since(c.fetched) < c.interval && c.cfg != nil {
		return
	}
	c.fetched = time.Now()
	out, err := exec.Command(c.cmd[0], c.cmd[1:]...).Output()
	text := string(out)
	if err != nil {
		if c.fallbackPath != "" {
			text, err = nginxconf.LoadFromDisk(c.fallbackPath)
		}
		if err != nil {
			c.failures++
			slog.Warn("config unavailable via nginx -T and disk fallback", "cmd", c.cmd, "fallback", c.fallbackPath, "err", err)
			return
		}
	}
	c.cfg = nginxconf.Parse(text)
}

func (c *ConfigCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- vhostInfoDesc
	ch <- workerProcsDesc
	ch <- workerConnsDesc
	ch <- configScrapeErrDesc
}

func (c *ConfigCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.Lock()
	c.refreshLocked()
	cfg, failures := c.cfg, c.failures
	c.mu.Unlock()

	ch <- prometheus.MustNewConstMetric(configScrapeErrDesc, prometheus.CounterValue, failures)
	if cfg == nil {
		return
	}
	ch <- prometheus.MustNewConstMetric(workerProcsDesc, prometheus.GaugeValue, float64(cfg.WorkerProcesses))
	ch <- prometheus.MustNewConstMetric(workerConnsDesc, prometheus.GaugeValue, float64(cfg.WorkerConnections))

	seen := map[string]bool{}
	for _, s := range cfg.Servers {
		backends := cfg.Backends(s)
		if backends == nil {
			backends = []string{""}
		}
		for _, name := range s.Names {
			for _, l := range s.Listens {
				tls := "false"
				if l.TLS {
					tls = "true"
				}
				for _, b := range backends {
					key := name + "|" + l.Addr + "|" + l.Port + "|" + tls + "|" + b
					if seen[key] {
						continue
					}
					seen[key] = true
					ch <- prometheus.MustNewConstMetric(vhostInfoDesc, prometheus.GaugeValue, 1,
						name, l.Addr, l.Port, tls, b, s.File)
				}
			}
		}
	}
}

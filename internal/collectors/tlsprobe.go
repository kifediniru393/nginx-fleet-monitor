package collectors

import (
	"context"
	"crypto/tls"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// TLSProbeCollector reports certificate expiry for every TLS vhost in the
// parsed topology by handshaking against the local listener with the vhost as
// SNI and reading the certificate nginx actually serves. Observing the wire
// rather than reading cert files means: no key/cert file permissions needed,
// SNI selection bugs are caught (the vhost serving the wrong cert entirely),
// and what's measured is what clients get.
type TLSProbeCollector struct {
	cfg *ConfigCollector

	mu      sync.Mutex
	results map[probeKey]probeResult
}

type probeKey struct{ vhost, port string }

type probeResult struct {
	expiry   time.Time
	sanMatch bool
	ok       bool // handshake succeeded
}

var (
	certExpiryDesc = prometheus.NewDesc("nginx_fleet_vhost_cert_expiry_timestamp_seconds",
		"NotAfter of the certificate actually served for the vhost (probed via SNI against the local listener).",
		[]string{"vhost", "listen_port"}, nil)
	certSanMatchDesc = prometheus.NewDesc("nginx_fleet_vhost_cert_san_match",
		"1 if the served certificate is valid for the vhost name — 0 means the wrong cert is being served (SNI/config bug).",
		[]string{"vhost", "listen_port"}, nil)
	certProbeErrDesc = prometheus.NewDesc("nginx_fleet_vhost_cert_probe_failed",
		"1 if the TLS handshake for this vhost failed entirely.",
		[]string{"vhost", "listen_port"}, nil)
)

func NewTLSProbeCollector(cfg *ConfigCollector) *TLSProbeCollector {
	return &TLSProbeCollector{cfg: cfg, results: map[probeKey]probeResult{}}
}

// Start probes shortly after startup (once the config parse has settled) and
// hourly thereafter — certificates don't change faster, and each probe is a
// full local handshake per TLS vhost.
func (c *TLSProbeCollector) Start(ctx context.Context) {
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(15 * time.Second):
		}
		for {
			c.probeAll()
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Hour):
			}
		}
	}()
}

func (c *TLSProbeCollector) probeAll() {
	cfg := c.cfg.Config()
	if cfg == nil {
		return
	}
	local := localIPv4s()
	fresh := map[probeKey]probeResult{}
	for _, s := range cfg.Servers {
		for _, l := range s.Listens {
			if !l.TLS {
				continue
			}
			// Listeners bound to a specific address (commonly the VIP) are not
			// reachable on loopback; dial the configured address — but only if
			// this node currently holds it. Dialing a VIP from the standby
			// would traverse the network to the master and report the
			// *master's* certificate under this node's identity, silently
			// defeating drift detection. Not held -> skip, no series.
			addr := "127.0.0.1"
			if l.Addr != "" && l.Addr != "0.0.0.0" && l.Addr != "[::]" && l.Addr != "*" {
				if !local[l.Addr] {
					continue
				}
				addr = l.Addr
			}
			for _, name := range s.Names {
				// SNI needs a literal DNS name: skip catch-alls and wildcards.
				if name == "_" || strings.HasPrefix(name, "*") {
					continue
				}
				key := probeKey{name, l.Port}
				if _, done := fresh[key]; done {
					continue
				}
				fresh[key] = probeOne(net.JoinHostPort(addr, l.Port), name)
			}
		}
	}
	c.mu.Lock()
	c.results = fresh
	c.mu.Unlock()
}

// localIPv4s returns the IPv4 addresses currently held by this node,
// including VIPs while this node is master.
func localIPv4s() map[string]bool {
	out := map[string]bool{}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return out
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
			out[ipn.IP.String()] = true
		}
	}
	return out
}

// probeOne handshakes addr with serverName as SNI and inspects the leaf.
func probeOne(addr, serverName string) probeResult {
	d := &net.Dialer{Timeout: 5 * time.Second}
	conn, err := tls.DialWithDialer(d, "tcp", addr, &tls.Config{
		ServerName:         serverName,
		InsecureSkipVerify: true, // self-signed is fine; we verify name match ourselves
	})
	if err != nil {
		return probeResult{}
	}
	defer conn.Close()
	certs := conn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return probeResult{}
	}
	leaf := certs[0]
	return probeResult{
		expiry:   leaf.NotAfter,
		sanMatch: leaf.VerifyHostname(serverName) == nil,
		ok:       true,
	}
}

func (c *TLSProbeCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- certExpiryDesc
	ch <- certSanMatchDesc
	ch <- certProbeErrDesc
}

func (c *TLSProbeCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, r := range c.results {
		if !r.ok {
			ch <- prometheus.MustNewConstMetric(certProbeErrDesc, prometheus.GaugeValue, 1, k.vhost, k.port)
			continue
		}
		ch <- prometheus.MustNewConstMetric(certExpiryDesc, prometheus.GaugeValue, float64(r.expiry.Unix()), k.vhost, k.port)
		match := 0.0
		if r.sanMatch {
			match = 1
		}
		ch <- prometheus.MustNewConstMetric(certSanMatchDesc, prometheus.GaugeValue, match, k.vhost, k.port)
	}
}

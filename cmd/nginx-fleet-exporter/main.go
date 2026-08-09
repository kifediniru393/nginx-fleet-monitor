// nginx-fleet-exporter: a standard Prometheus exporter for nginx fleets.
// Phase 0 collectors: config topology (nginx -T), worker capacity (/proc),
// and optional passive VRRP cluster identity.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/techmoose/nginx-fleet-exporter/internal/collectors"
	"github.com/techmoose/nginx-fleet-exporter/internal/ingress"
	"github.com/techmoose/nginx-fleet-exporter/internal/keepalived"
	"github.com/techmoose/nginx-fleet-exporter/internal/vrrp"
)

func main() {
	listenAddr := flag.String("web.listen-address", ":9942", "address for /metrics")
	nginxTCmd := flag.String("nginx.t-command", "nginx -T", "command producing the assembled nginx config")
	nginxConf := flag.String("nginx.config", "/etc/nginx/nginx.conf", "on-disk config parsed (with include resolution) when nginx -T fails; empty disables fallback")
	configInterval := flag.Duration("nginx.config-interval", 60*time.Second, "minimum interval between config re-parses")
	vrrpMode := flag.String("vrrp", "auto", "VRRP module: on, off, or auto (enable when adverts are heard)")
	keepalivedConf := flag.String("keepalived.config", "/etc/keepalived/keepalived.conf", "local keepalived config for cluster membership; missing file is fine")
	accessLog := flag.String("ingress.access-log", "", "path to the 'fleet' JSON access log; empty disables the ingress collector")
	maxVhosts := flag.Int("ingress.max-vhosts", 500, "distinct vhost labels before folding into _other")
	stateFile := flag.String("ingress.state-file", "/var/lib/nginx-fleet-exporter/state.json", "idle-clock persistence; empty disables")
	decommissionWindow := flag.Duration("decommission-window", 120*time.Hour, "idle window after which an upstream is a decommission candidate (informational; consumed by recording rules)")
	flag.Parse()

	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "unknown"
	}

	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(prometheus.NewGoCollector())

	cfgCollector := collectors.NewConfigCollector(strings.Fields(*nginxTCmd), *nginxConf, *configInterval)
	reg.MustRegister(cfgCollector)
	reg.MustRegister(collectors.WorkersCollector{})

	reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "nginx_fleet_decommission_window_seconds",
		Help: "Configured idle window after which an upstream is flagged for decommissioning.",
	}, func() float64 { return decommissionWindow.Seconds() }))

	// VRRP module: optional by design. Any failure here degrades to
	// vrrp_enabled=0 and never takes down the other collectors.
	tracker := vrrp.NewTracker()
	var vrrpRunning atomic.Bool
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfgCollector.StartResolver(ctx)

	if *vrrpMode != "off" {
		go func() {
			vrrpRunning.Store(true)
			err := vrrp.Listen(ctx, tracker)
			vrrpRunning.Store(false)
			if err != nil && ctx.Err() == nil {
				slog.Warn("vrrp listener stopped; continuing without VRRP", "err", err)
			}
		}()
	}
	enabled := func() bool {
		if !vrrpRunning.Load() {
			return false
		}
		if *vrrpMode == "on" {
			return true
		}
		// auto: enabled once adverts have actually been heard.
		states, _ := tracker.Snapshot()
		return len(states) > 0
	}
	vc := collectors.NewVRRPCollector(tracker, enabled, hostname)
	if inst, err := keepalived.ParseFile(*keepalivedConf); err != nil {
		slog.Warn("keepalived.conf unreadable; cluster_info unavailable", "path", *keepalivedConf, "err", err)
	} else {
		vc.Instances = inst
	}
	reg.MustRegister(vc)

	// Ingress collector (log-tailing mechanism). Same degradation contract as
	// vrrp: tailer failure -> ingress_enabled 0, everything else keeps serving.
	stats := ingress.NewStats(*maxVhosts)
	var ingressRunning atomic.Bool
	if *accessLog != "" && *stateFile != "" {
		// Idle clocks survive restarts, or the 5-day decommission window
		// would reset on every deploy.
		if err := ingress.EnsureStateDir(*stateFile); err != nil {
			slog.Warn("state dir unavailable; idle clocks won't persist", "err", err)
		} else {
			if err := stats.LoadState(*stateFile); err != nil {
				slog.Warn("state load failed", "err", err)
			}
			go func() {
				t := time.NewTicker(time.Minute)
				defer t.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-t.C:
						stats.SaveState(*stateFile)
					}
				}
			}()
		}
	}
	if *accessLog != "" {
		// Seed configured-but-never-seen vhosts/upstreams from the topology so
		// "zero traffic since we began watching" also reaches the 5-day rule.
		go func() {
			t := time.NewTicker(time.Minute)
			defer t.Stop()
			for {
				if cfg := cfgCollector.Config(); cfg != nil {
					now := time.Now()
					valid := map[ingress.UpstreamKey]bool{}
					for _, srv := range cfg.Servers {
						for _, name := range srv.Names {
							backends := cfg.Backends(srv)
							if len(backends) == 0 {
								stats.Seed(name, "", now)
							}
							for _, b := range backends {
								// Seed under the resolved identity: logs record
								// IPs, and a hostname-keyed seed would never
								// match traffic, false-alarming decommission.
								addr := cfgCollector.ResolveBackend(b)
								stats.Seed(name, addr, now)
								valid[ingress.UpstreamSeedKey(name, addr)] = true
							}
						}
					}
					stats.PruneSeeds(valid)
				}
				select {
				case <-ctx.Done():
					return
				case <-t.C:
				}
			}
		}()
	}
	if *accessLog != "" {
		go func() {
			for ctx.Err() == nil {
				ingressRunning.Store(true)
				err := ingress.Tail(ctx, *accessLog, stats)
				ingressRunning.Store(false)
				if ctx.Err() == nil {
					slog.Warn("ingress tailer stopped; retrying in 30s", "path", *accessLog, "err", err)
					select {
					case <-time.After(30 * time.Second):
					case <-ctx.Done():
					}
				}
			}
		}()
	}
	reg.MustRegister(&ingress.Collector{Stats: stats, Enabled: ingressRunning.Load})

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html><body><h1>nginx-fleet-exporter</h1><p><a href="/metrics">/metrics</a></p></body></html>`))
	})

	srv := &http.Server{
		Addr:    *listenAddr,
		Handler: mux,
		// Slowloris guard: the endpoint is typically reachable from the whole
		// scrape network and unauthenticated.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	slog.Info("nginx-fleet-exporter listening", "addr", *listenAddr, "vrrp", *vrrpMode, "node", hostname)
	err := srv.ListenAndServe()
	// Final synchronous save: the periodic saver races process exit.
	if *accessLog != "" && *stateFile != "" {
		if serr := stats.SaveState(*stateFile); serr != nil {
			slog.Warn("final state save failed", "err", serr)
		}
	}
	if err != nil && err != http.ErrServerClosed {
		slog.Error("server failed", "err", err)
		os.Exit(1)
	}
}

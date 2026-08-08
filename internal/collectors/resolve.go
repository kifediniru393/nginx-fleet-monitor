package collectors

import (
	"context"
	"net"
	"strings"
	"sync"
	"time"
)

// resolverCache maps upstream hostnames to IPs so config-side identity
// (hostnames) can join ingress-side identity (log-observed IPs). Lookups run
// only in the background refresh — never on the scrape path — and results
// stick until replaced, so a DNS outage degrades to stale-but-usable mappings
// rather than churning series. (Deliberate: nginx itself behaves the same
// way, serving on resolutions cached at reload.)
type resolverCache struct {
	mu     sync.Mutex
	byHost map[string]string
	lookup func(ctx context.Context, host string) ([]string, error)
}

func newResolverCache() *resolverCache {
	return &resolverCache{
		byHost: map[string]string{},
		lookup: net.DefaultResolver.LookupHost,
	}
}

// Refresh resolves every hostname in addrs ("host:port" or bare host),
// bounded to 2s per name. IP literals are skipped.
func (r *resolverCache) Refresh(addrs []string) {
	for _, addr := range addrs {
		host := addr
		if h, _, err := net.SplitHostPort(addr); err == nil {
			host = h
		}
		if host == "" || net.ParseIP(host) != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		ips, err := r.lookup(ctx, host)
		cancel()
		if err != nil || len(ips) == 0 {
			continue // keep any previous resolution
		}
		ip := firstIPv4(ips)
		if ip == "" {
			continue
		}
		r.mu.Lock()
		r.byHost[host] = ip
		r.mu.Unlock()
	}
}

// Resolve maps "host:port" to "ip:port" using the cache; unresolved or
// literal addresses come back unchanged.
func (r *resolverCache) Resolve(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		host, port = addr, ""
	}
	r.mu.Lock()
	ip, ok := r.byHost[host]
	r.mu.Unlock()
	if !ok {
		return addr
	}
	if port == "" {
		return ip
	}
	return net.JoinHostPort(ip, port)
}

func firstIPv4(ips []string) string {
	for _, s := range ips {
		if ip := net.ParseIP(s); ip != nil && ip.To4() != nil {
			return s
		}
	}
	return ""
}

// hostOf returns the host part of "host:port", or the string itself.
func hostOf(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return strings.TrimSpace(addr)
}

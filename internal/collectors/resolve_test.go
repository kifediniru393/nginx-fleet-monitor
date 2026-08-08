package collectors

import (
	"context"
	"errors"
	"testing"
)

func TestResolverCache(t *testing.T) {
	r := newResolverCache()
	calls := 0
	r.lookup = func(_ context.Context, host string) ([]string, error) {
		calls++
		switch host {
		case "bee.kniru.local":
			return []string{"fe80::1", "192.168.2.91"}, nil // IPv4 preferred
		default:
			return nil, errors.New("NXDOMAIN")
		}
	}

	r.Refresh([]string{"bee.kniru.local:8006", "rodimus.kniru.local:8006", "10.0.0.5:80", "bare-host"})
	if got := r.Resolve("bee.kniru.local:8006"); got != "192.168.2.91:8006" {
		t.Fatalf("resolved = %q", got)
	}
	// Unresolvable and literal addresses pass through unchanged.
	if got := r.Resolve("rodimus.kniru.local:8006"); got != "rodimus.kniru.local:8006" {
		t.Fatalf("unresolved = %q", got)
	}
	if got := r.Resolve("10.0.0.5:80"); got != "10.0.0.5:80" {
		t.Fatalf("literal = %q", got)
	}

	// A later DNS outage keeps the last known mapping.
	r.lookup = func(_ context.Context, _ string) ([]string, error) { return nil, errors.New("timeout") }
	r.Refresh([]string{"bee.kniru.local:8006"})
	if got := r.Resolve("bee.kniru.local:8006"); got != "192.168.2.91:8006" {
		t.Fatalf("outage dropped cached mapping: %q", got)
	}
}

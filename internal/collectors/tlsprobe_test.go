package collectors

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestProbeOne(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "https://")

	// httptest's cert is for example.com and 127.0.0.1.
	r := probeOne(addr, "example.com")
	if !r.ok {
		t.Fatal("handshake failed")
	}
	if !r.sanMatch {
		t.Fatal("expected SAN match for example.com against httptest cert")
	}
	if r.expiry.Before(time.Now()) {
		t.Fatalf("httptest cert reported expired: %v", r.expiry)
	}

	// A name the cert does NOT cover: handshake still succeeds (we skip
	// verification), but the mismatch must be reported — that's the
	// wrong-cert-served signal.
	r = probeOne(addr, "other.example.org")
	if !r.ok {
		t.Fatal("handshake failed for mismatched SNI")
	}
	if r.sanMatch {
		t.Fatal("SAN mismatch not detected")
	}

	// Nothing listening: probe failure, not a panic.
	if r := probeOne("127.0.0.1:1", "example.com"); r.ok {
		t.Fatal("probe against closed port reported ok")
	}
}

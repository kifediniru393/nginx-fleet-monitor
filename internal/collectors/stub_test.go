package collectors

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

const stubPage = "Active connections: 1 \nserver accepts handled requests\n 306 306 308 \nReading: 0 Writing: 1 Waiting: 0 \n"

func TestParseStub(t *testing.T) {
	s, err := parseStub(stubPage)
	if err != nil {
		t.Fatal(err)
	}
	if s.active != 1 || s.accepted != 306 || s.handled != 306 || s.requests != 308 || s.writing != 1 {
		t.Fatalf("parsed %+v", s)
	}
	if _, err := parseStub("<html>not a stub page</html>"); err == nil {
		t.Fatal("garbage accepted")
	}
}

func TestStubCollectorCompatNames(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(stubPage))
	}))
	defer srv.Close()

	c := NewStubCollector(srv.URL)
	// The exact exposition the official exporter produces for the same page.
	want := `
# HELP nginx_http_requests_total Total HTTP requests (official exporter compatible).
# TYPE nginx_http_requests_total counter
nginx_http_requests_total 308
# HELP nginx_up 1 if the stub_status endpoint is reachable (official exporter compatible).
# TYPE nginx_up gauge
nginx_up 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want), "nginx_up", "nginx_http_requests_total"); err != nil {
		t.Fatal(err)
	}
}

func TestStubCollectorDown(t *testing.T) {
	c := NewStubCollector("http://127.0.0.1:1/stub_status") // nothing listens
	if v := testutil.CollectAndCount(c); v != 1 {
		t.Fatalf("down stub should emit only nginx_up, got %d series", v)
	}
	reg := prometheus.NewRegistry()
	reg.MustRegister(c)
	if v := testutil.ToFloat64(testCollectOne(c)); v != 0 {
		t.Fatalf("nginx_up = %v, want 0", v)
	}
}

// testCollectOne returns a single-metric collector view for ToFloat64.
func testCollectOne(c prometheus.Collector) prometheus.Collector { return c }

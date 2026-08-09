package collectors

import (
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// StubCollector scrapes nginx's stub_status endpoint and emits the exact
// metric names of the official nginx-prometheus-exporter, making this
// exporter a drop-in replacement: existing dashboards and alerts written
// against nginx_up / nginx_connections_* / nginx_http_requests_total keep
// working unchanged. It also supplies the connection-state gauges
// (active/reading/writing/waiting) that no other collector here covers —
// worker-internal state only stub_status exposes.
type StubCollector struct {
	URI    string
	client *http.Client
}

func NewStubCollector(uri string) *StubCollector {
	return &StubCollector{URI: uri, client: &http.Client{Timeout: 3 * time.Second}}
}

var (
	stubUpDesc = prometheus.NewDesc("nginx_up",
		"1 if the stub_status endpoint is reachable (official exporter compatible).", nil, nil)
	stubAcceptedDesc = prometheus.NewDesc("nginx_connections_accepted",
		"Accepted client connections (official exporter compatible).", nil, nil)
	stubHandledDesc = prometheus.NewDesc("nginx_connections_handled",
		"Handled client connections (official exporter compatible).", nil, nil)
	stubRequestsDesc = prometheus.NewDesc("nginx_http_requests_total",
		"Total HTTP requests (official exporter compatible).", nil, nil)
	stubActiveDesc = prometheus.NewDesc("nginx_connections_active",
		"Active client connections (official exporter compatible).", nil, nil)
	stubReadingDesc = prometheus.NewDesc("nginx_connections_reading",
		"Connections reading the request (official exporter compatible).", nil, nil)
	stubWritingDesc = prometheus.NewDesc("nginx_connections_writing",
		"Connections writing the response (official exporter compatible).", nil, nil)
	stubWaitingDesc = prometheus.NewDesc("nginx_connections_waiting",
		"Idle keep-alive connections (official exporter compatible).", nil, nil)
)

// stubStatus holds one parsed stub_status page.
type stubStatus struct {
	active, reading, writing, waiting  float64
	accepted, handled, requests        float64
}

// parseStub decodes the fixed stub_status format:
//
//	Active connections: 1
//	server accepts handled requests
//	 306 306 308
//	Reading: 0 Writing: 1 Waiting: 0
func parseStub(body string) (*stubStatus, error) {
	var s stubStatus
	_, err := fmt.Sscanf(body,
		"Active connections: %f\nserver accepts handled requests\n %f %f %f\nReading: %f Writing: %f Waiting: %f",
		&s.active, &s.accepted, &s.handled, &s.requests, &s.reading, &s.writing, &s.waiting)
	if err != nil {
		return nil, fmt.Errorf("stub_status parse: %w", err)
	}
	return &s, nil
}

func (c *StubCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- stubUpDesc
	ch <- stubAcceptedDesc
	ch <- stubHandledDesc
	ch <- stubRequestsDesc
	ch <- stubActiveDesc
	ch <- stubReadingDesc
	ch <- stubWritingDesc
	ch <- stubWaitingDesc
}

func (c *StubCollector) Collect(ch chan<- prometheus.Metric) {
	s, err := c.fetch()
	if err != nil {
		ch <- prometheus.MustNewConstMetric(stubUpDesc, prometheus.GaugeValue, 0)
		return
	}
	ch <- prometheus.MustNewConstMetric(stubUpDesc, prometheus.GaugeValue, 1)
	ch <- prometheus.MustNewConstMetric(stubAcceptedDesc, prometheus.CounterValue, s.accepted)
	ch <- prometheus.MustNewConstMetric(stubHandledDesc, prometheus.CounterValue, s.handled)
	ch <- prometheus.MustNewConstMetric(stubRequestsDesc, prometheus.CounterValue, s.requests)
	ch <- prometheus.MustNewConstMetric(stubActiveDesc, prometheus.GaugeValue, s.active)
	ch <- prometheus.MustNewConstMetric(stubReadingDesc, prometheus.GaugeValue, s.reading)
	ch <- prometheus.MustNewConstMetric(stubWritingDesc, prometheus.GaugeValue, s.writing)
	ch <- prometheus.MustNewConstMetric(stubWaitingDesc, prometheus.GaugeValue, s.waiting)
}

func (c *StubCollector) fetch() (*stubStatus, error) {
	resp, err := c.client.Get(c.URI)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("stub_status: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return nil, err
	}
	return parseStub(string(body))
}

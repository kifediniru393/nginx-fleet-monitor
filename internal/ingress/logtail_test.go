package ingress

import (
	"reflect"
	"testing"
	"time"
)

var t0 = time.Unix(1754000000, 0)

func line(host, upstream string, status int, bytesSent int) string {
	return `{"host":"` + host + `","upstream":"` + upstream + `","bytes_sent":` +
		itoa(bytesSent) + `,"request_length":100,"status":` + itoa(status) + `,"upstream_time":"0.05"}`
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestIngestBasics(t *testing.T) {
	s := NewStats(100)
	s.Ingest(line("a.example.com", "10.1.1.100:8080", 200, 5000), t0)
	s.Ingest(line("a.example.com", "10.1.1.100:8080", 404, 300), t0.Add(time.Second))

	vh := vhostKey{"a.example.com"}
	if s.Requests[vh]["2xx"] != 1 || s.Requests[vh]["4xx"] != 1 {
		t.Fatalf("requests = %v", s.Requests[vh])
	}
	if s.BytesSent[vh] != 5300 || s.BytesReceived[vh] != 200 {
		t.Fatalf("bytes = %d/%d", s.BytesSent[vh], s.BytesReceived[vh])
	}
	if !s.VhostLastSeen[vh].Equal(t0.Add(time.Second)) {
		t.Fatal("last seen not updated")
	}
	key := upstreamKey{"a.example.com", "10.1.1.100:8080"}
	if s.UpstreamRequests[key] != 2 || !s.Up(key) {
		t.Fatalf("upstream = %d up=%v", s.UpstreamRequests[key], s.Up(key))
	}
}

func TestIngestRetryAttemptsAreFailures(t *testing.T) {
	s := NewStats(100)
	// proxy_next_upstream: first member failed, second served the request.
	s.Ingest(`{"host":"a","upstream":"10.1.1.100:8080, 10.1.1.101:8080","bytes_sent":1,"request_length":1,"status":200,"upstream_time":"3.0, 0.02"}`, t0)

	failed := upstreamKey{"a", "10.1.1.100:8080"}
	served := upstreamKey{"a", "10.1.1.101:8080"}
	if s.UpstreamFailures[failed]["next_upstream"] != 1 || s.Up(failed) {
		t.Fatalf("failed member: %v up=%v", s.UpstreamFailures[failed], s.Up(failed))
	}
	if len(s.UpstreamFailures[served]) != 0 || !s.Up(served) {
		t.Fatalf("serving member wrongly marked failed: %v", s.UpstreamFailures[served])
	}
	// The bare colon in addr:port must not split the attempt list.
	want := []string{"10.1.1.100:8080", "10.1.1.101:8080"}
	if got := splitAttempts("10.1.1.100:8080, 10.1.1.101:8080"); !reflect.DeepEqual(got, want) {
		t.Fatalf("split = %v", got)
	}
	if got := splitAttempts("10.1.1.100:8080 : 10.1.1.101:8080"); !reflect.DeepEqual(got, want) {
		t.Fatalf("group split = %v", got)
	}
}

func TestIngestBadGatewayFinal(t *testing.T) {
	s := NewStats(100)
	s.Ingest(line("a", "10.1.1.100:8080", 502, 150), t0)
	key := upstreamKey{"a", "10.1.1.100:8080"}
	if s.UpstreamFailures[key]["http_502"] != 1 || s.Up(key) {
		t.Fatalf("502 not recorded as failure: %v up=%v", s.UpstreamFailures[key], s.Up(key))
	}
	// A later success flips it back up.
	s.Ingest(line("a", "10.1.1.100:8080", 200, 150), t0.Add(time.Minute))
	if !s.Up(key) {
		t.Fatal("recovery not reflected")
	}
}

func TestIngestUnattributed(t *testing.T) {
	s := NewStats(100)
	s.Ingest("not json", t0)
	s.Ingest(`{"host":"","upstream":"-","bytes_sent":0,"request_length":0,"status":400,"upstream_time":"-"}`, t0)
	if s.Unattributed["parse_fail"] != 1 || s.Unattributed["no_host"] != 1 {
		t.Fatalf("unattributed = %v", s.Unattributed)
	}
}

func TestIngestStaticVhostNoUpstream(t *testing.T) {
	s := NewStats(100)
	s.Ingest(line("static.example.com", "-", 200, 1000), t0)
	if len(s.UpstreamRequests) != 0 {
		t.Fatalf("upstream recorded for '-': %v", s.UpstreamRequests)
	}
	if s.Requests[vhostKey{"static.example.com"}]["2xx"] != 1 {
		t.Fatal("vhost request not counted")
	}
}

func TestCardinalityCap(t *testing.T) {
	s := NewStats(2)
	s.Ingest(line("a", "-", 200, 1), t0)
	s.Ingest(line("b", "-", 200, 1), t0)
	s.Ingest(line("c", "-", 200, 1), t0) // over cap -> _other
	s.Ingest(line("a", "-", 200, 1), t0) // existing vhost still tracked
	if _, ok := s.VhostLastSeen[vhostKey{"c"}]; ok {
		t.Fatal("cap not enforced")
	}
	if s.Requests[vhostKey{"_other"}]["2xx"] != 1 || s.Requests[vhostKey{"a"}]["2xx"] != 2 {
		t.Fatalf("cap accounting wrong: %v", s.Requests)
	}
}

func TestIngestNegativeBytesIgnored(t *testing.T) {
	s := NewStats(100)
	s.Ingest(`{"host":"a","upstream":"-","bytes_sent":-5,"request_length":-1,"status":200,"upstream_time":"-"}`, t0)
	if s.BytesSent[vhostKey{"a"}] != 0 || s.BytesReceived[vhostKey{"a"}] != 0 {
		t.Fatalf("negative bytes corrupted counters: %d/%d", s.BytesSent[vhostKey{"a"}], s.BytesReceived[vhostKey{"a"}])
	}
}

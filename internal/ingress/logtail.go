// Package ingress implements the log-tailing runtime attribution mechanism:
// per-vhost and per-upstream traffic, last-traffic timestamps (decommission
// signal), and empirical upstream failure evidence — all from one documented
// access_log format.
//
// Required nginx config (the one permitted piece of config coupling):
//
//	log_format fleet escape=json '{"host":"$host","upstream":"$upstream_addr",'
//	    '"bytes_sent":$bytes_sent,"request_length":$request_length,'
//	    '"status":$status,"upstream_time":"$upstream_response_time"}';
//	access_log /var/log/nginx/fleet.log fleet;
package ingress

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type logLine struct {
	Host          string `json:"host"`
	Upstream      string `json:"upstream"`
	BytesSent     int64  `json:"bytes_sent"`
	RequestLength int64  `json:"request_length"`
	Status        int    `json:"status"`
	UpstreamTime  string `json:"upstream_time"`
}

type vhostKey struct{ vhost string }
type upstreamKey struct{ vhost, addr string }

// Stats is the in-memory aggregation the collector reads at scrape time.
type Stats struct {
	mu sync.Mutex

	Requests      map[vhostKey]map[string]uint64 // vhost -> status class ("2xx"...) -> count
	BytesSent     map[vhostKey]uint64
	BytesReceived map[vhostKey]uint64
	VhostLastSeen map[vhostKey]time.Time

	UpstreamRequests map[upstreamKey]uint64
	UpstreamFailures map[upstreamKey]map[string]uint64 // reason -> count
	UpstreamLastSeen map[upstreamKey]time.Time
	UpstreamLastOK   map[upstreamKey]time.Time
	UpstreamLastFail map[upstreamKey]time.Time
	UpstreamTimeSum  map[upstreamKey]float64
	// Histogram of $upstream_response_time per member: non-cumulative counts
	// per bucket (cumulated at collect time), plus an observation count.
	// Buckets give per-member percentiles — means hide the saturating tail.
	UpstreamTimeHist  map[upstreamKey]*[len(TimeBuckets) + 1]uint64 // last slot = +Inf
	UpstreamTimeCount map[upstreamKey]uint64

	Unattributed map[string]uint64 // reason -> count

	maxVhosts int
}

func NewStats(maxVhosts int) *Stats {
	return &Stats{
		Requests:         map[vhostKey]map[string]uint64{},
		BytesSent:        map[vhostKey]uint64{},
		BytesReceived:    map[vhostKey]uint64{},
		VhostLastSeen:    map[vhostKey]time.Time{},
		UpstreamRequests: map[upstreamKey]uint64{},
		UpstreamFailures: map[upstreamKey]map[string]uint64{},
		UpstreamLastSeen: map[upstreamKey]time.Time{},
		UpstreamLastOK:   map[upstreamKey]time.Time{},
		UpstreamLastFail: map[upstreamKey]time.Time{},
		UpstreamTimeSum:   map[upstreamKey]float64{},
		UpstreamTimeHist:  map[upstreamKey]*[len(TimeBuckets) + 1]uint64{},
		UpstreamTimeCount: map[upstreamKey]uint64{},
		Unattributed:     map[string]uint64{},
		maxVhosts:        maxVhosts,
	}
}

// Ingest folds one access-log line into the stats at time now.
func (s *Stats) Ingest(raw string, now time.Time) {
	var l logLine
	if err := json.Unmarshal([]byte(raw), &l); err != nil {
		s.mu.Lock()
		s.Unattributed["parse_fail"]++
		s.mu.Unlock()
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if l.Host == "" {
		s.Unattributed["no_host"]++
		return
	}
	vh := vhostKey{s.capVhost(l.Host)}

	if s.Requests[vh] == nil {
		s.Requests[vh] = map[string]uint64{}
	}
	s.Requests[vh][statusClass(l.Status)]++
	// Guard negatives: uint64(-1) would jump the counter by 2^64-1. Log lines
	// are semi-trusted (nginx writes them, but fields echo client input).
	if l.BytesSent > 0 {
		s.BytesSent[vh] += uint64(l.BytesSent)
	}
	if l.RequestLength > 0 {
		s.BytesReceived[vh] += uint64(l.RequestLength)
	}
	s.VhostLastSeen[vh] = now

	// $upstream_addr is a comma/colon separated attempt list when
	// proxy_next_upstream retries: every address before the last is a failed
	// attempt — empirical failure evidence for that member.
	attempts := splitAttempts(l.Upstream)
	times := splitAttempts(l.UpstreamTime)
	for i, addr := range attempts {
		if addr == "" || addr == "-" {
			continue
		}
		key := upstreamKey{vh.vhost, addr}
		s.UpstreamRequests[key]++
		s.UpstreamLastSeen[key] = now
		if i < len(times) {
			if t, err := strconv.ParseFloat(times[i], 64); err == nil && t >= 0 {
				s.UpstreamTimeSum[key] += t
				h := s.UpstreamTimeHist[key]
				if h == nil {
					h = &[len(TimeBuckets) + 1]uint64{}
					s.UpstreamTimeHist[key] = h
				}
				h[bucketIndex(t)]++
				s.UpstreamTimeCount[key]++
			}
		}
		final := i == len(attempts)-1
		switch {
		case !final:
			s.fail(key, "next_upstream", now)
		case l.Status >= 502 && l.Status <= 504:
			s.fail(key, "http_"+strconv.Itoa(l.Status), now)
		default:
			s.UpstreamLastOK[key] = now
		}
	}
}

func (s *Stats) fail(key upstreamKey, reason string, now time.Time) {
	if s.UpstreamFailures[key] == nil {
		s.UpstreamFailures[key] = map[string]uint64{}
	}
	s.UpstreamFailures[key][reason]++
	s.UpstreamLastFail[key] = now
}

// capVhost bounds label cardinality: once maxVhosts distinct vhosts are
// tracked, new ones fold into "_other" (wildcard server_name protection).
func (s *Stats) capVhost(host string) string {
	if _, seen := s.VhostLastSeen[vhostKey{host}]; seen || len(s.VhostLastSeen) < s.maxVhosts {
		return host
	}
	return "_other"
}

// Up reports the empirical up/down verdict for one upstream member:
// down if its most recent evidence is a failure.
func (s *Stats) Up(key upstreamKey) bool {
	return !s.UpstreamLastOK[key].Before(s.UpstreamLastFail[key])
}

// TimeBuckets are the upstream latency histogram bounds (Prometheus defaults).
var TimeBuckets = [...]float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}

func bucketIndex(t float64) int {
	for i, le := range TimeBuckets {
		if t <= le {
			return i
		}
	}
	return len(TimeBuckets) // +Inf
}

func statusClass(code int) string {
	if code < 100 || code > 599 {
		return "other"
	}
	return strconv.Itoa(code/100) + "xx"
}

// splitAttempts splits $upstream_addr / $upstream_response_time attempt
// lists. nginx separates retry attempts with ", " and server-group switches
// with " : " — a bare ':' is part of addr:port and must not split.
func splitAttempts(v string) []string {
	if v == "" || v == "-" {
		return nil
	}
	v = strings.ReplaceAll(v, " : ", ", ")
	var out []string
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Tail follows the access log, surviving rotation and truncation, feeding
// each line into stats. Polls rather than inotify: one dependency fewer, and
// a 1s delay is irrelevant at scrape resolution. Blocks until ctx ends.
func Tail(ctx context.Context, path string, s *Stats) error {
	var f *os.File
	var reader *bufio.Reader
	var size int64

	open := func(seekEnd bool) error {
		if f != nil {
			f.Close()
		}
		var err error
		f, err = os.Open(path)
		if err != nil {
			return err
		}
		if seekEnd {
			f.Seek(0, io.SeekEnd)
		}
		fi, _ := f.Stat()
		if fi != nil {
			size = fi.Size()
		}
		reader = bufio.NewReader(f)
		return nil
	}

	// Start at the end: history is unknowable anyway, and replaying a huge
	// log on restart would double-count into the counters.
	if err := open(true); err != nil {
		return err
	}
	defer f.Close()

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
		for {
			line, err := reader.ReadString('\n')
			if line != "" && err == nil {
				s.Ingest(strings.TrimRight(line, "\n"), time.Now())
				continue
			}
			break
		}
		// Rotation (new inode) or truncation (shrunk file): reopen from start.
		fi, err := os.Stat(path)
		if err != nil {
			continue // rotated away and not recreated yet
		}
		cur, _ := f.Stat()
		if cur == nil || !os.SameFile(fi, cur) || fi.Size() < size {
			open(false)
		}
		if fi != nil {
			size = fi.Size()
		}
	}
}

# Contributing

Issues and pull requests welcome.

## Development

```sh
go test ./...     # full suite, runs anywhere, no privileges needed
go vet ./...
GOOS=linux GOARCH=amd64 go build ./cmd/nginx-fleet-exporter
```

The VRRP parser tests replay a checked-in pcap of real keepalived adverts
(`internal/vrrp/testdata/`). If you have a keepalived environment speaking a
configuration we don't have fixtures for (VRRPv3, unicast_peer, IPv6), a
5-second capture is a very welcome contribution:

```sh
tcpdump -i any proto 112 -c 5 -w vrrp-<description>.pcap
```

## Ground rules

- No runtime dependencies on other exporters or agents — self-contained is a
  design constraint, not an accident.
- Collectors must degrade independently: a failure in one collector may never
  take down another. New collectors follow the same contract (an `_enabled`
  gauge, no panics across the boundary).
- Nothing blocking on the scrape path: scrapes read in-memory state only.
- Wire and log inputs are untrusted or semi-trusted — bound your cardinality
  and validate your parsing. Tests for the hostile case are expected.

## Reporting bugs

Include the exporter version, `uname -a`, and if it's a VRRP issue, a packet
capture (`tcpdump -i any proto 112 -c 5 -w bug.pcap`) — the wire format is
the ground truth we debug against.

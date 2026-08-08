# nginx-fleet-exporter

A standard Prometheus exporter for nginx fleets. Single Go binary, one
`/metrics` endpoint (default `:9942`), modular collectors that degrade
independently — any collector failing leaves the rest serving:

| Collector | Status | What it gives you |
|---|---|---|
| config | always on | Intended topology from the nginx config: `vhost_info` (vhost × listener × upstream member), `worker_processes`, `worker_connections_limit` |
| workers | always on (Linux) | Per-worker capacity from `/proc`: open/limit fds, CPU, RSS |
| ingress | optional (`--ingress.access-log`) | Observed traffic from a dedicated access log: per-vhost requests/bytes, per-upstream traffic/failures/latency, `upstream_up`, last-traffic idle clocks |
| vrrp | optional (`--vrrp`) | Passive VRRP cluster identity from the wire: who is master per VRID, failover transitions, stepdown early-warning, `nginx_fleet_active` |
| keepalived membership | automatic | `cluster_info` from the local keepalived.conf: which VRIDs this node belongs to, on which L2 segment |

Design rationale: `nginx-fleet-exporter-plan.md` (revision 3).
Verified on a live keepalived pair: failover detected < 2 s, transitions
counted exactly once per direction on every observer.

## Requirements

- **Linux** on the target hosts (VRRP listener and /proc metrics are
  Linux-only; the binary runs elsewhere with those collectors disabled).
- **CAP_NET_RAW** for the VRRP listener (packet socket, same mechanism as
  tcpdump). Granted by the shipped systemd unit; without it the exporter
  still runs with `vrrp_enabled 0`.
- **Readable nginx config.** The collector tries `nginx -T` first (the
  *running* config); if that fails — it validates TLS keys an unprivileged
  user can't read — it falls back to parsing `/etc/nginx/nginx.conf` from
  disk with full `include` resolution. No sudo, no cert access needed.
- **Readable access log** for the ingress collector (the unit adds the
  exporter user to group `adm`; the drop-in's log is root-owned 0644).
- Go 1.26+ to build. No runtime dependencies on any other exporter/agent.

## Install (per host, all steps additive — no existing file is edited)

```sh
# 1. Build (on your workstation)
GOOS=linux GOARCH=amd64 go build -o nginx-fleet-exporter ./cmd/nginx-fleet-exporter

# 2. Binary + systemd unit
scp nginx-fleet-exporter root@HOST:/usr/local/bin/
scp deploy/nginx-fleet-exporter.service root@HOST:/etc/systemd/system/
ssh root@HOST 'useradd -r -s /usr/sbin/nologin nginx-exporter; usermod -aG adm nginx-exporter'

# 3. Ingress log drop-in (skip if you don't want traffic attribution)
scp deploy/zz-fleet-logging.conf root@HOST:/etc/nginx/conf.d/
ssh root@HOST 'nginx -t && nginx -s reload'

# 4. Log rotation for the new log
scp deploy/fleet-logrotate root@HOST:/etc/logrotate.d/nginx-fleet

# 5. Start
ssh root@HOST 'systemctl daemon-reload && systemctl enable --now nginx-fleet-exporter'
curl -s http://HOST:9942/metrics | grep nginx_fleet_
```

The logging drop-in works because stock nginx setups `include
/etc/nginx/conf.d/*.conf` inside `http {}`; nginx writes to *every*
configured `access_log`, so existing logs continue untouched and deleting
the drop-in fully reverts. **Boundary:** a `server` block that declares its
own `access_log` overrides the http-level one — such vhosts won't appear in
the fleet log (their requests count only in their own logs).

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `--web.listen-address` | `:9942` | metrics endpoint |
| `--nginx.t-command` | `nginx -T` | command producing the assembled running config |
| `--nginx.config` | `/etc/nginx/nginx.conf` | disk fallback when nginx -T fails; empty disables |
| `--nginx.config-interval` | `60s` | minimum interval between config re-parses (never per-scrape) |
| `--vrrp` | `auto` | `on` / `off` / `auto` (enabled once protocol-112 adverts are heard) |
| `--keepalived.config` | `/etc/keepalived/keepalived.conf` | membership source; missing file is normal |
| `--ingress.access-log` | *(empty = off)* | path to the fleet JSON log |
| `--ingress.max-vhosts` | `500` | distinct vhost labels before folding into `_other` (wildcard `server_name` protection) |
| `--ingress.state-file` | `/var/lib/nginx-fleet-exporter/state.json` | idle-clock persistence across restarts; empty disables |
| `--decommission-window` | `120h` | idle window (5 days) exported for the hygiene rules |

## Cluster identity at fleet scale

Each node independently reports **membership** (from its own keepalived.conf
— catches standbys, which never advertise) and **mastership** (from the wire
— ground truth, not self-report). VRIDs are only unique per L2 segment, so
`cluster_info` carries a `segment` label (the vrrp_instance interface's IPv4
network in CIDR):

```promql
count by (segment, vrid) (nginx_fleet_cluster_info)                 # every cluster + size
max by (segment, vrid, node) (nginx_fleet_vrrp_master) == 1         # current master per cluster
count by (segment, vrid) (nginx_fleet_cluster_info) < 2             # clusters missing a standby
nginx_fleet_active{method="vrrp"}                                   # is this node serving?
```

Nodes without keepalived report `vrrp_enabled 0` and
`active{method="static"} 1` — they coexist in the same dashboards.

## Hygiene / decommissioning

`rules/hygiene.yml` (VictoriaMetrics/Prometheus rule file) flags:

- **Decommission candidates**: vhosts/upstream members idle longer than the
  5-day window. Configured-but-never-used entries are seeded with a
  "watching since" clock, so dead config alerts too — it doesn't need to
  have ever served a request. Idle clocks persist across exporter restarts.
- **Down upstreams**: `upstream_up == 0` from empirical evidence — a
  `proxy_next_upstream` retry marks the skipped member down, 502–504 finals
  mark the serving member down, any success flips it back.
- **VRRP split brain**: two masters on one VRID.

Make sure the decommission window exceeds your longest legitimate quiet
period (monthly batch jobs) before acting on the candidates list.

## Testing

```sh
go test ./...            # 30 tests, runs anywhere (includes golden-pcap replay)
```

The VRRP parser is validated against a checked-in capture of real keepalived
adverts (`internal/vrrp/testdata/`) — this fixture is what caught the RFC
3768 checksum-scope bug synthetic tests missed. To validate a live pair,
stop keepalived on the master and watch the standby's exporter: expect the
takeover within ~2 s (graceful stop announces priority-0 first) and exactly
one `vrrp_transitions_total` increment per direction per observer.

## Known limitations

- `worker_processes auto` reports as `0`; count the per-pid worker series
  instead.
- Config topology uses upstream *hostnames*; ingress observes resolved
  *IPs*. Until the exporter resolves configured names, join those two by
  vhost rather than by upstream address.
- Ingress attribution is per-request from logs — HTTP/2 coalescing is a
  non-issue (unlike SNI-based approaches), but vhosts with their own
  `access_log` are invisible (see Install boundary above).
- VRRPv3 support is implemented and unit-tested but not yet validated
  against a live v3 pair.
- The metrics endpoint itself has no TLS/auth; firewall it or front it if
  the scrape network is untrusted.

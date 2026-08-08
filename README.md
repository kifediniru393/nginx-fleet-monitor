# nginx-fleet-exporter

A standard Prometheus exporter for nginx fleets. Single Go binary, one
`/metrics` endpoint (default `:9942`), modular collectors:

| Collector | Status | Metrics |
|---|---|---|
| config | always on | `nginx_fleet_vhost_info`, `nginx_fleet_worker_processes`, `nginx_fleet_worker_connections_limit` — intended topology from `nginx -T` |
| workers | always on (Linux) | per-worker fds/CPU/RSS from `/proc` |
| vrrp | optional | passive VRRP cluster identity: master state, transitions, stepdown early-warning, `nginx_fleet_active` |
| ingress | optional | log-tailing runtime attribution: per-vhost requests/bytes, per-upstream traffic and failures, `nginx_fleet_upstream_up`, last-traffic timestamps (decommission signal) |

Design: see `nginx-fleet-exporter-plan.md` (revision 3).

## Ingress collector setup

The one permitted piece of nginx config coupling — add to `http {}`:

```nginx
log_format fleet escape=json '{"host":"$host","upstream":"$upstream_addr",'
    '"bytes_sent":$bytes_sent,"request_length":$request_length,'
    '"status":$status,"upstream_time":"$upstream_response_time"}';
access_log /var/log/nginx/fleet.log fleet;
```

Then run with `--ingress.access-log=/var/log/nginx/fleet.log`. The tailer
starts at end-of-file, survives logrotate (inode change) and truncation, and
retries if the file disappears. Retried requests (`proxy_next_upstream`) count
every non-final attempt as an empirical failure for that member.

## Usage

```
nginx-fleet-exporter \
  --web.listen-address=:9942 \
  --nginx.t-command="nginx -T" \
  --vrrp=auto \                  # on | off | auto (enable when adverts heard)
  --decommission-window=120h     # 5 days, per ops policy
```

The VRRP module needs `CAP_NET_RAW` (see `deploy/nginx-fleet-exporter.service`)
and Linux. With `--vrrp=off`, without the capability, or on a host with no
keepalived, the exporter runs normally with `nginx_fleet_vrrp_enabled 0` and
`nginx_fleet_active{method="static"} 1`.

`nginx -T` must be runnable by the exporter user; it reads any TLS key files
referenced by the config, so either grant group read access or use
`--nginx.t-command="sudo nginx -T"` with a scoped sudoers rule.

## Build and test

```
go build ./cmd/nginx-fleet-exporter
go test ./...
GOOS=linux GOARCH=amd64 go build ./cmd/nginx-fleet-exporter   # deploy target
```

Parser tests (VRRP v2/v3, checksum, truncation, version-branch; nginx -T
topology) run anywhere. Tier 1 netns integration tests require Linux.

## Rules

`rules/hygiene.yml` — decommission-candidate alerts (idle > 5 days) and VRRP
split-brain, for VictoriaMetrics/Prometheus rule evaluation.

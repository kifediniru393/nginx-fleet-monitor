#!/bin/sh
# Deploy nginx-fleet-exporter to one or more hosts in a single command.
#
# Usage:
#   deploy/deploy.sh user@host [user@host ...]
#
# What it does, per host:
#   1. Builds a stamped static binary locally (CGO_ENABLED=0, arch of the target
#      via $GOARCH, default amd64).
#   2. Ships the binary + systemd unit + nginx drop-ins + logrotate config.
#   3. Creates the nginx-exporter user (idempotent), validates and reloads nginx,
#      enables/starts (or restarts) the service.
#   4. Verifies /metrics answers.
#
# Every step is additive and idempotent: re-running upgrades the binary and
# refreshes the config files; no existing nginx config is edited.
#
# Env overrides:
#   GOARCH=arm64   target architecture (default amd64)
#   SKIP_BUILD=1   reuse the ./nginx-fleet-exporter binary already present
#   METRICS_PORT   port checked in the verify step (default 9942)
#   NGINX_BIN      path to the nginx binary on the target hosts (default: nginx
#                  from the remote PATH; source builds often need
#                  NGINX_BIN=/usr/local/nginx/sbin/nginx)

set -eu

[ $# -ge 1 ] || { echo "usage: $0 user@host [user@host ...]" >&2; exit 1; }

cd "$(dirname "$0")/.."

GOARCH="${GOARCH:-amd64}"
METRICS_PORT="${METRICS_PORT:-9942}"
NGINX_BIN="${NGINX_BIN:-nginx}"

if [ "${SKIP_BUILD:-0}" != "1" ]; then
    echo ">>> building nginx-fleet-exporter (linux/$GOARCH, static)"
    CGO_ENABLED=0 GOOS=linux GOARCH="$GOARCH" go build \
        -ldflags "-X main.version=$(git describe --tags --always --dirty 2>/dev/null || echo dev) \
                  -X main.commit=$(git rev-parse --short HEAD 2>/dev/null || echo unknown) \
                  -X main.date=$(date +%F)" \
        -o nginx-fleet-exporter ./cmd/nginx-fleet-exporter
fi

for HOST in "$@"; do
    echo ">>> deploying to $HOST"

    # Stage everything in one scp; install with correct paths remotely.
    # The binary goes via a temp path + install(1) so an in-use binary is
    # replaced atomically instead of failing with "text file busy".
    ssh "$HOST" 'mkdir -p /tmp/nginx-fleet-deploy'
    scp -q nginx-fleet-exporter \
        deploy/nginx-fleet-exporter.service \
        deploy/zz-fleet-logging.conf \
        deploy/zz-stub-status.conf \
        deploy/fleet-logrotate \
        "$HOST:/tmp/nginx-fleet-deploy/"

    ssh "$HOST" "NGINX_BIN='$NGINX_BIN' sh -s" <<'EOF'
set -eu
cd /tmp/nginx-fleet-deploy

id nginx-exporter >/dev/null 2>&1 || useradd -r -s /usr/sbin/nologin nginx-exporter
usermod -aG adm nginx-exporter

install -m 0755 nginx-fleet-exporter /usr/local/bin/nginx-fleet-exporter
install -m 0644 nginx-fleet-exporter.service /etc/systemd/system/
install -m 0644 zz-fleet-logging.conf /etc/nginx/conf.d/
install -m 0644 zz-stub-status.conf /etc/nginx/conf.d/
install -m 0644 fleet-logrotate /etc/logrotate.d/nginx-fleet

"$NGINX_BIN" -t
"$NGINX_BIN" -s reload

# The drop-ins only take effect if nginx.conf includes /etc/nginx/conf.d/*.conf
# inside http{} (stock on distro packages; often absent on source builds).
# Check the assembled running config actually picked them up.
if ! "$NGINX_BIN" -T 2>/dev/null | grep -q zz-fleet-logging.conf; then
    echo "WARNING: $(hostname): nginx.conf does not include /etc/nginx/conf.d/*.conf —" >&2
    echo "         fleet log + stub_status drop-ins are installed but INACTIVE." >&2
    echo "         Add 'include /etc/nginx/conf.d/*.conf;' inside http{} and reload." >&2
fi

systemctl daemon-reload
systemctl enable --now nginx-fleet-exporter
systemctl restart nginx-fleet-exporter

rm -rf /tmp/nginx-fleet-deploy
EOF

    echo ">>> verifying $HOST"
    TARGET="${HOST#*@}"
    sleep 1
    if curl -sf --max-time 5 "http://$TARGET:$METRICS_PORT/metrics" | grep -q nginx_fleet_build_info; then
        curl -s --max-time 5 "http://$TARGET:$METRICS_PORT/metrics" | grep '^nginx_fleet_build_info'
    else
        echo "!!! $HOST: /metrics not answering on :$METRICS_PORT" >&2
        ssh "$HOST" 'systemctl status nginx-fleet-exporter --no-pager -l | tail -20' >&2 || true
        exit 1
    fi
done

echo ">>> done: $# host(s) deployed"

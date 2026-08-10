#!/bin/sh
# Ship rules/hygiene.yml to the vmalert host and reload.
# Usage: deploy/deploy-rules.sh [user@host]   (default root@192.168.2.200)
set -e
HOST="${1:-root@192.168.2.200}"
scp rules/hygiene.yml "$HOST:/etc/victoriametrics/rules/"
ssh "$HOST" 'systemctl restart vmalert && sleep 2 && systemctl is-active vmalert'

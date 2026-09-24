#!/usr/bin/env bash
# uninstall-consul.sh
# Completely remove Consul installed by run-consul.sh
set -euo pipefail

FORCE=""
[[ "${1:-}" == "--force" ]] && FORCE="1"

RED='\033[0;31m'; GREEN='\033[0;32m'; NC='\033[0m'
log_info()  { echo -e "${GREEN}[INFO]${NC} $*"; }
log_warn()  { echo -e "${RED}[WARN]${NC} $*"; }

if [[ $EUID -ne 0 ]]; then
   exec sudo bash "$0" "$@"
fi

# 要清理的对象放数组，方便复用
SYSTEMD_UNIT="/etc/systemd/system/consul.service"
CONSUL_DIRS=(/etc/consul.d /data/consul /opt/consul /var/log/consul /tmp/consul-acl-bootstrap-done)

# ------------------  dry-run  ------------------
if [[ -z "$FORCE" ]]; then
  log_info "[DRY-RUN] The following items will be removed:"
  systemctl is-active  consul.service &>/dev/null && echo "  - stop consul.service"
  systemctl is-enabled consul.service &>/dev/null && echo "  - disable consul.service"
  [[ -f $SYSTEMD_UNIT ]] && echo "  - remove $SYSTEMD_UNIT"
  pgrep -x consul &>/dev/null && echo "  - kill consul processes"
  for d in "${CONSUL_DIRS[@]}"; do [[ -e "$d" ]] && echo "  - remove $d"; done
  echo
  log_warn "No action taken. Use --force to perform actual removal."
  exit 0
fi

# ------------------  force mode  ------------------
if systemctl is-active --quiet consul.service; then
  log_info "Stopping consul.service ..."
  systemctl stop consul.service
fi
if systemctl is-enabled --quiet consul.service 2>/dev/null; then
  log_info "Disabling consul.service ..."
  systemctl disable consul.service
fi

if [[ -f $SYSTEMD_UNIT ]]; then
  log_info "Removing systemd unit ..."
  rm -f "$SYSTEMD_UNIT"
  systemctl daemon-reload
fi

if pgrep -x consul >/dev/null; then
  log_info "Killing remaining consul processes ..."
  pkill -9 consul || true
fi

for dir in "${CONSUL_DIRS[@]}"; do
  if [[ -e "$dir" ]]; then
    log_info "Removing $dir ..."
    rm -rf "$dir"
  fi
done

log_info "Consul has been completely removed."


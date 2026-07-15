#!/usr/bin/env bash
set -euo pipefail

DRY_RUN=true                         # Dry-run by default; set --force to delete
NOMAD_CONFIG_DIR="/etc/nomad.d"      # Corresponds to --config-dir
NOMAD_DATA_DIR="/data/nomad"         # Corresponds to --data-dir
NOMAD_BIN_DIR="/usr/local/bin"       # Corresponds to --bin-dir
NOMAD_LOG_DIR="/log/nomad"           # Corresponds to --log-dir

function log_info  { echo -e "\033[1;32m[INFO]\033[0m $*"; }
function log_warn  { echo -e "\033[1;33m[WARN]\033[0m $*"; }
function log_error { echo -e "\033[1;31m[ERROR]\033[0m $*"; }

function safe_rm {
  local path="$1"
  if [[ ! -e "$path" ]]; then
    log_warn "$path does not exist, skipping"
    return
  fi
  if [[ "$DRY_RUN" == "true" ]]; then
    log_info "[Dry-run] Will remove: $path"
  else
    log_info "Removing: $path"
    rm -rf "$path"
  fi
}

function main {
  if [[ "$DRY_RUN" != "true" ]]; then
    sudo systemctl stop    nomad || true
    sudo systemctl disable nomad || true
  fi

  # 2. Remove systemd unit
  SYSTEMD_PATH="/etc/systemd/system/nomad.service"
  safe_rm "$SYSTEMD_PATH"

  find /data/nomad/alloc -type d \( -name "secrets" -o -name "private" \) -exec umount {} \; 2>/dev/null; rm -rf /data/nomad/alloc
  # 3. Remove directories
  safe_rm "$NOMAD_CONFIG_DIR"
  safe_rm "$NOMAD_DATA_DIR"
  safe_rm "$NOMAD_LOG_DIR"

  # 4. Remove nomad binary (comment out if not desired)
  if [[ -f "$NOMAD_BIN_DIR/nomad" ]]; then
    safe_rm "$NOMAD_BIN_DIR/nomad"
  fi

  # 5. Remove plugins and docker auth dir (hard-coded in install script)
  safe_rm "/opt/nomad/plugins"
  safe_rm "/root/docker"

  # 6. Reload systemd
  if [[ "$DRY_RUN" != "true" ]]; then
    log_info "Reloading systemd daemon"
    sudo systemctl daemon-reload
    sudo systemctl reset-failed 2>/dev/null || true
  fi

  log_info "Uninstall finished"
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --force|-f)
      DRY_RUN=false
      shift
      ;;
    --config-dir)
      NOMAD_CONFIG_DIR="$2"
      shift 2
      ;;
    --data-dir)
      NOMAD_DATA_DIR="$2"
      shift 2
      ;;
    --bin-dir)
      NOMAD_BIN_DIR="$2"
      shift 2
      ;;
    --log-dir)
      NOMAD_LOG_DIR="$2"
      shift 2
      ;;
    --help|-h)
      cat <<EOF
Usage: $0 [OPTIONS]

OPTIONS:
  -f, --force        Perform actual deletion without prompting (dry-run by default)
  --config-dir PATH  Config directory path (default /etc/nomad.d)
  --data-dir PATH    Data directory path (default /data/nomad)
  --bin-dir PATH     Binary directory path (default /usr/local/bin)
  --log-dir PATH     Log directory path (default /log/nomad)
  -h, --help         Show this help message
EOF
      exit 0
      ;;
    *)
      log_error "Unknown argument: $1"
      exit 1
      ;;
  esac
done

if [[ "$DRY_RUN" == "true" ]]; then
  log_info "Currently in **Dry-run** mode; only paths to be removed will be printed."
  log_info "To execute removal, run again with: $0 --force"
fi

main



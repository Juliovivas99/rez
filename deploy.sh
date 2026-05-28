#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")" && pwd)"
cd "$ROOT"

REMOTE_USER="${REMOTE_USER:-root}"
REMOTE_DIR="${REMOTE_DIR:-/opt/reservation-bot}"
BINARY="reservation-bot"

# Fill in droplet IPs (or export before running):
#   export CARBONE_IP=...
#   export FOUR_CHARLES_IP=...
#   export TORRISI_IP=...
#   export I_SODI_IP=...
#   export VIA_CAROTA_IP=...
CARBONE_IP="${CARBONE_IP:-}"
FOUR_CHARLES_IP="${FOUR_CHARLES_IP:-}"
TORRISI_IP="${TORRISI_IP:-}"
I_SODI_IP="${I_SODI_IP:-}"
VIA_CAROTA_IP="${VIA_CAROTA_IP:-}"

declare -A DROPLETS=(
  [carbone]="configs/carbone.yaml"
  [4-charles]="configs/4-charles.yaml"
  [torrisi]="configs/torrisi.yaml"
  [i-sodi]="configs/i-sodi.yaml"
  [via-carota]="configs/via-carota.yaml"
)

droplet_ip() {
  case "$1" in
    carbone) echo "$CARBONE_IP" ;;
    4-charles) echo "$FOUR_CHARLES_IP" ;;
    torrisi) echo "$TORRISI_IP" ;;
    i-sodi) echo "$I_SODI_IP" ;;
    via-carota) echo "$VIA_CAROTA_IP" ;;
    *) echo "" ;;
  esac
}

deploy_one() {
  local name="$1"
  local config_file="${DROPLETS[$name]}"
  local ip
  ip="$(droplet_ip "$name")"

  if [[ -z "$ip" ]]; then
    echo "[$name] skip: set IP env var for $name"
    return 1
  fi

  echo "[$name] deploying to $ip ..."
  ssh "${REMOTE_USER}@${ip}" "mkdir -p ${REMOTE_DIR}"
  scp "$BINARY" "${REMOTE_USER}@${ip}:${REMOTE_DIR}/${BINARY}"
  scp "$config_file" "${REMOTE_USER}@${ip}:${REMOTE_DIR}/config.yaml"

  ssh "${REMOTE_USER}@${ip}" bash -s <<EOF
set -euo pipefail

# chrony: continuous OS-level sync (best practice on Linux; complements in-app NTP).
if command -v apt-get >/dev/null 2>&1; then
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq
  apt-get install -y chrony
  systemctl enable chrony 2>/dev/null || true
  systemctl restart chrony 2>/dev/null || true
fi

cd ${REMOTE_DIR}
chmod +x ${BINARY}
tmux kill-session -t resbot 2>/dev/null || true
tmux new-session -d -s resbot "./${BINARY} --config config.yaml"
EOF

  echo "[$name] done"
}

echo "Building linux/amd64 binary..."
GOOS=linux GOARCH=amd64 go build -o "$BINARY" .

targets=()
if [[ $# -gt 0 ]]; then
  for arg in "$@"; do
    if [[ -z "${DROPLETS[$arg]+x}" ]]; then
      echo "Unknown restaurant: $arg"
      echo "Valid: ${!DROPLETS[*]}"
      exit 1
    fi
    targets+=("$arg")
  done
else
  targets=("${!DROPLETS[@]}")
fi

pids=()
for name in "${targets[@]}"; do
  deploy_one "$name" &
  pids+=($!)
done

fail=0
for pid in "${pids[@]}"; do
  wait "$pid" || fail=1
done

exit "$fail"

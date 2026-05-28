#!/usr/bin/env bash
# Install chrony for continuous sub-ms clock sync on Linux droplets.
# Run on each server: sudo ./scripts/setup-clock.sh
set -euo pipefail

if [[ "$(uname -s)" != "Linux" ]]; then
  echo "chrony setup is for Linux servers; macOS uses system time + in-app NTP"
  exit 0
fi

export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y chrony

# Prefer low-latency public strata (chrony replaces ntpdate for continuous sync).
cat >/etc/chrony/chrony.conf <<'EOF'
pool time.google.com iburst
pool time.cloudflare.com iburst
pool pool.ntp.org iburst
makestep 0.1 3
rtcsync
EOF

systemctl enable chrony
systemctl restart chrony
sleep 1
chronyc tracking
chronyc sources -v

echo "Clock sync ready. The bot also runs in-app NTP median sync at startup and before drop."

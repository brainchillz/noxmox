#!/bin/bash
# Install or update nox on one Proxmox VE node. Run as root on the node, or
# from a workstation as: ./deploy/install.sh root@node HOST_ID DEVICE
#
# What it does: installs the nox binary, the noxd unit and its defaults, the
# PVE storage plugin, and the wdmd drop-in that loads the hardware watchdog
# PVE deny-lists. It does not format anything and does not touch a running
# sanlock daemon.
set -euo pipefail

usage() { echo "usage: $0 root@node HOST_ID DEVICE_PATH" >&2; exit 2; }
[ $# -eq 3 ] || usage
TARGET=$1; HOST_ID=$2; DEVICE=$3
HERE=$(cd "$(dirname "$0")/.." && pwd)

[ -x "$HERE/bin/nox" ] || (cd "$HERE" && go build -ldflags "-X main.version=$(git describe --tags --always --dirty 2>/dev/null || echo dev)" -o bin/nox ./cmd/nox)

scp -q "$HERE/bin/nox" "$HERE/deploy/noxd.service" "$HERE/deploy/nox-watchdog-order.service" "$HERE/deploy/90-nox-watchdog.rules" "$HERE/pve/NoxPlugin.pm" "$TARGET:/tmp/"
ssh "$TARGET" bash -s "$HOST_ID" "$DEVICE" <<'REMOTE'
set -euo pipefail
HOST_ID=$1; DEVICE=$2
export DEBIAN_FRONTEND=noninteractive
dpkg -s sanlock >/dev/null 2>&1 || apt-get install -y -qq sanlock >/dev/null

# Watchdog split. The iTCO timer on the lab's HP EliteDesk 800 G6 never
# resets the machine (tested 2026-10-28), so wdmd gets softdog, the one
# device proven to fire. watchdog-mux keeps /dev/watchdog, which becomes the
# iTCO because it is loaded first. PVE HA self-fencing on such a node is
# therefore nominal only; on hardware with a working watchdog, drop the
# order unit and point wdmd at the hardware device instead.
install -m 0644 /tmp/nox-watchdog-order.service /etc/systemd/system/nox-watchdog-order.service
install -m 0644 /tmp/90-nox-watchdog.rules /etc/udev/rules.d/90-nox-watchdog.rules
echo 'wdmd_opts="-w /dev/watchdog-soft"' > /etc/default/wdmd
rm -f /etc/systemd/system/wdmd.service.d/nox-itco.conf
systemctl enable nox-watchdog-order.service >/dev/null 2>&1
udevadm control --reload

install -m 0755 /tmp/nox /usr/local/sbin/nox
install -m 0644 /tmp/noxd.service /etc/systemd/system/noxd.service
printf 'NOX_DEVICE=%s\nNOX_HOST_ID=%s\n' "$DEVICE" "$HOST_ID" > /etc/default/noxd
perl -I/usr/share/perl5 -c /tmp/NoxPlugin.pm >/dev/null
install -m 0644 /tmp/NoxPlugin.pm /usr/share/perl5/PVE/Storage/Custom/NoxPlugin.pm

systemctl daemon-reload
systemctl enable --now wdmd sanlock >/dev/null 2>&1 || true
systemctl enable noxd >/dev/null 2>&1
if systemctl is-active --quiet noxd; then systemctl restart noxd; else systemctl start noxd || true; fi
systemctl restart pvedaemon pveproxy pvestatd pvescheduler
echo "$(hostname): wdmd=$(systemctl is-active wdmd) sanlock=$(systemctl is-active sanlock) noxd=$(systemctl is-active noxd) host_id=$HOST_ID"
REMOTE

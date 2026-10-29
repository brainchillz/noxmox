#!/bin/bash
# Build the nox Debian package for Proxmox VE. Needs go, libsanlock-dev and
# dpkg-deb. Output: dist/nox_<version>_amd64.deb
set -euo pipefail
HERE=$(cd "$(dirname "$0")/.." && pwd)
cd "$HERE"
GITVER=$(git describe --tags --always --dirty 2>/dev/null || echo dev)
# v0.1.0-4-gabc123 -> 0.1.0+4.gabc123 (sorts after 0.1.0, before 0.1.1)
VERSION=${VERSION:-$(echo "$GITVER" | sed -E 's/^v//; s/-([0-9]+)-g/+\1.g/; s/-dirty/+dirty/')}
ARCH=$(dpkg --print-architecture)
STAGE=$(mktemp -d)
trap 'rm -rf "$STAGE"' EXIT

go build -ldflags "-X main.version=$GITVER" -o "$STAGE/nox" ./cmd/nox

install -D -m 0755 "$STAGE/nox" "$STAGE/root/usr/sbin/nox"
install -D -m 0644 deploy/noxd@.service "$STAGE/root/lib/systemd/system/noxd@.service"
install -D -m 0644 deploy/nox-watchdog-order.service "$STAGE/root/lib/systemd/system/nox-watchdog-order.service"
install -D -m 0644 deploy/90-nox-watchdog.rules "$STAGE/root/lib/udev/rules.d/90-nox-watchdog.rules"
install -D -m 0644 pve/NoxPlugin.pm "$STAGE/root/usr/share/perl5/PVE/Storage/Custom/NoxPlugin.pm"
install -D -m 0644 deploy/pool.conf.example "$STAGE/root/usr/share/doc/nox/pool.conf.example"
install -D -m 0644 README.md "$STAGE/root/usr/share/doc/nox/README.md"
install -D -m 0644 LICENSE "$STAGE/root/usr/share/doc/nox/LICENSE"
install -d -m 0755 "$STAGE/root/etc/nox"

mkdir -p "$STAGE/root/DEBIAN"
cat > "$STAGE/root/DEBIAN/control" <<CONTROL
Package: nox
Version: $VERSION
Section: admin
Priority: optional
Architecture: $ARCH
Depends: sanlock, libsanlock1 (>= 3.9), qemu-utils, dmsetup
Recommends: pve-storage
Maintainer: brainchill <david@davidrodgers.com>
Homepage: https://github.com/brainchillz/noxmox
Description: shared iSCSI/FC block pool for Proxmox VE with sanlock leases
 One LUN, every node opens it read-write, each VM disk is a device-mapper
 volume over fixed extents. Ownership is a sanlock lease on the LUN, so a
 node that stops renewing is fenced and its disks become available to the
 survivors, with no cluster filesystem or kernel module involved.
CONTROL
cat > "$STAGE/root/DEBIAN/postinst" <<'POSTINST'
#!/bin/sh
set -e
if [ "$1" = configure ]; then
    systemctl daemon-reload >/dev/null 2>&1 || true
    udevadm control --reload >/dev/null 2>&1 || true
    # restart running pool daemons so they pick up the new binary; their
    # leases are orphans the new process adopts
    for u in $(systemctl list-units --plain --no-legend 'noxd@*.service' 2>/dev/null | awk '{print $1}'); do
        systemctl restart "$u" >/dev/null 2>&1 || true
    done
    # the storage plugin is loaded by the PVE daemons at start
    for s in pvedaemon pveproxy pvestatd pvescheduler; do
        systemctl try-restart "$s" >/dev/null 2>&1 || true
    done
fi
POSTINST
cat > "$STAGE/root/DEBIAN/prerm" <<'PRERM'
#!/bin/sh
set -e
# Do not stop the pool daemons on upgrade; on removal leave leases as orphans
# too, sanlock keeps them until the lockspace is left.
exit 0
PRERM
chmod 0755 "$STAGE/root/DEBIAN/postinst" "$STAGE/root/DEBIAN/prerm"

mkdir -p dist
dpkg-deb --root-owner-group --build "$STAGE/root" "dist/nox_${VERSION}_${ARCH}.deb" >/dev/null
echo "dist/nox_${VERSION}_${ARCH}.deb"

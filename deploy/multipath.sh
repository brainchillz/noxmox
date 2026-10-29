#!/bin/bash
# Put the pool LUN under multipath on one PVE node. Run from a workstation:
#   ./deploy/multipath.sh root@node WWID PORTAL2_IP [PORTAL1_IP]
# WWID is the LUN's SCSI id (the 3600... part of /dev/disk/by-id/scsi-3600...).
#
# What it does: installs multipath-tools, writes a multipath.conf that claims
# only this WWID, lowers the iSCSI replacement timeout so a failed path is
# given up before sanlock gives up on the host, logs in to the second portal
# and reports the multipath device. It does NOT change the pool config: move
# every guest off the node, then set NOX_DEVICE in /etc/nox/<pool>.conf to
# /dev/mapper/<WWID> and restart noxd@<pool>; the daemon joins through the
# new path. See README "Timeouts that must agree".
set -euo pipefail
[ $# -ge 3 ] || { echo "usage: $0 root@node WWID PORTAL2_IP [PORTAL1_IP]" >&2; exit 2; }
TARGET=$1; WWID=$2; PORTAL2=$3; PORTAL1=${4:-}
ssh "$TARGET" bash -s "$WWID" "$PORTAL2" "$PORTAL1" <<'REMOTE'
set -euo pipefail
WWID=$1; PORTAL2=$2; PORTAL1=$3
export DEBIAN_FRONTEND=noninteractive
dpkg -s multipath-tools >/dev/null 2>&1 || apt-get install -y -qq multipath-tools >/dev/null

cat > /etc/multipath.conf <<CONF
# nox pool LUN. Only this WWID is multipathed; everything else stays alone.
defaults {
    user_friendly_names no
    find_multipaths strict
    polling_interval 5
}
blacklist {
    wwid ".*"
}
blacklist_exceptions {
    wwid "$WWID"
}
multipaths {
    multipath {
        wwid "$WWID"
        # give up a dead path after 6 checks of 5 s: 30 s, well inside
        # sanlock's 80 s (8 x io_timeout 10) before it expires the host
        no_path_retry 6
    }
}
CONF
# a failed iSCSI session must return errors before sanlock expires the host
sed -i 's/^node.session.timeo.replacement_timeout = .*/node.session.timeo.replacement_timeout = 15/' /etc/iscsi/iscsid.conf
IQN=$(iscsiadm -m session 2>/dev/null | awk '{print $4}' | head -1)
for p in $(iscsiadm -m node 2>/dev/null | awk '{print $1}' | cut -d: -f1 | sort -u); do
    iscsiadm -m node -p "$p" -o update -n node.session.timeo.replacement_timeout -v 15 >/dev/null 2>&1 || true
done
# second path
iscsiadm -m discovery -t sendtargets -p "$PORTAL2" >/dev/null
iscsiadm -m node -p "$PORTAL2" -o update -n node.session.timeo.replacement_timeout -v 15 >/dev/null 2>&1 || true
iscsiadm -m node -p "$PORTAL2" -o update -n node.startup -v automatic >/dev/null 2>&1 || true
iscsiadm -m node -p "$PORTAL2" --login >/dev/null 2>&1 || true
systemctl enable --now multipathd >/dev/null 2>&1
systemctl reload multipathd 2>/dev/null || systemctl restart multipathd
sleep 3
multipath -r >/dev/null 2>&1 || true
echo "$(hostname): sessions=$(iscsiadm -m session 2>/dev/null | wc -l) replacement_timeout=$(iscsiadm -m session -P2 2>/dev/null | grep -m1 'Recovery Timeout' | awk '{print $NF}')"
multipath -ll "$WWID" 2>/dev/null | head -12 || echo "no multipath map yet (device still in use by a single path opener?)"
ls -l /dev/mapper/"$WWID" 2>/dev/null || true
REMOTE

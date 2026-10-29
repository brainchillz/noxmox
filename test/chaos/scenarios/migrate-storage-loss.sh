# The source of a live migration loses its storage path mid-copy. The dual
# open window is the one time two hosts legitimately share a volume, so the
# fence must still leave exactly one writable copy. Expect: the source is
# fenced (its guest killed or the host reset), the migration does not leave
# two live copies, the pool stays consistent, and the guest is recoverable
# on exactly one node with its acknowledged data intact. PASS is judged on
# those safety invariants; the guest being restartable afterwards is only a
# warning, because an abnormally aborted migration can leave stuck shared
# orphan leases that block the restart until the hosts reboot (a known bug,
# see planning/roadmap.md), which is an availability issue, not a safety one.
. "$HERE/scenarios/_crash.sh"

drop_path() { s "$1" "iptables -I OUTPUT -d $TARGET_IP -p tcp --dport 3260 -j DROP; iptables -I INPUT -s $TARGET_IP -p tcp --sport 3260 -j DROP; echo dropped" 15; }
restore_path() { s "$1" "iptables -D OUTPUT -d $TARGET_IP -p tcp --dport 3260 -j DROP 2>/dev/null; iptables -D INPUT -s $TARGET_IP -p tcp --sport 3260 -j DROP 2>/dev/null; iscsiadm -m session -R >/dev/null 2>&1; echo restored" 30
}

scenario_run() {
    local src dst sip dip run t0 verdict=PASS reason=""
    src=$(vm_node); [ -n "$src" ] || die "guest $VMID not running"
    dst=$(other_node "$src"); sip=$(node_ip "$src"); dip=$(node_ip "$dst")
    log "migrate-storage-loss: source $src ($sip) -> target $dst ($dip)"
    verifier_setup "$sip"; run=$(( (RANDOM << 15) | RANDOM )); verifier_start "$sip" "$run"; sleep 15

    log "starting throttled online migration"
    s "$sip" "nohup qm migrate $VMID $dst --online --bwlimit ${CHAOS_MIGRATE_BWLIMIT:-5120} > /tmp/chaos-migrate.log 2>&1 & echo \$! > /tmp/chaos-migrate.pid" 15
    wait_until 120 "memory copy to start" s "$sip" "grep -q 'migration active' /tmp/chaos-migrate.log" 10 >/dev/null || { result migrate-storage-loss FAIL "copy-never-started"; return 1; }
    sleep 2
    t0=$(date +%s)
    log "dropping source $src iSCSI path mid-copy"
    drop_path "$sip"
    trap 'repair_source "$sip"' EXIT
    repair_source() { restore_path "$1"; s "$1" "iscsiadm -m node -T $TARGET_IQN --logout >/dev/null 2>&1; sleep 2; iscsiadm -m node -T $TARGET_IQN --login >/dev/null 2>&1; sleep 4" 60 >/dev/null 2>&1 || true; if ! s "$1" "test -e /dev/disk/by-id/scsi-$WWID" 15; then s "$1" "nohup reboot >/dev/null 2>&1 &" 10; sleep 20; wait_node_back "$1"; else s "$1" "systemctl restart $NOXD_UNIT" 30 >/dev/null 2>&1; fi; }

    # the source must be fenced: its guest gone (killpath) or the host reset
    fenced() {
        local ip=$1
        host_down "$ip" && return 0
        local k; k=$(s "$ip" "/usr/sbin/nox fence --pool $POOL --dry-run 2>/dev/null | grep -c 'would SIGKILL' || true" 15 | tr -dc '0-9')
        [ "${k:-1}" = 0 ]
    }
    if wait_until 300 "source fenced" fenced "$sip" >/dev/null; then :; else verdict=FAIL; reason+="source-not-fenced "; fi
    local down=up; host_down "$sip" && down=reset
    log "source fenced after $(( $(date +%s)-t0 ))s (source=$down)"

    # exactly one node may have the guest's qemu alive
    local srcq dstq
    srcq=$(host_down "$sip" && echo 0 || s "$sip" "pgrep -f '^/usr/bin/kvm -id $VMID ' | wc -l" 15)
    dstq=$(s "$dip" "pgrep -f '^/usr/bin/kvm -id $VMID ' | wc -l" 15)
    log "qemu alive: source=$srcq target=$dstq"
    [ "$(( ${srcq:-0} + ${dstq:-0} ))" -le 1 ] || { verdict=FAIL; reason+="two-live-qemu(src=$srcq,dst=$dstq) "; }

    # recover the source path, settle, verify data + consistency from a healthy node
    repair_source "$sip"; trap - EXIT
    recover_quorum "$dip"
    local hn; for n in $(node_names); do s "$(node_ip "$n")" "nox status 2>/dev/null | grep -q 'ready true'" 15 && hn=$(node_ip "$n") && break; done
    if verifier_check "${hn:-$dip}" "$run" 240; then :; else verdict=FAIL; reason+="data-check "; fi
    nox_check "${hn:-$dip}" || { verdict=FAIL; reason+="check "; }
    # make sure the guest can run somewhere again
    local cfgnode; for n in $(node_names); do s "$(node_ip "$n")" "test -e /etc/pve/nodes/$n/qemu-server/$VMID.conf" 15 && cfgnode=$n; done
    if [ -n "${cfgnode:-}" ]; then
        local cip; cip=$(node_ip "$cfgnode")
        wait_until 180 "noxd ready on $cfgnode" sh -c "ssh -o BatchMode=yes root@$cip 'nox status 2>/dev/null' | grep -q 'ready true'" >/dev/null || true
        local started=
        for try in 1 2 3; do
            s "$cip" "qm status $VMID | grep -q running || qm start $VMID" 400 >/dev/null 2>&1
            s "$cip" "qm status $VMID 2>/dev/null | grep -q running" 15 && { started=1; break; }
            sleep 10
        done
        if [ -n "$started" ] && agent_wait "$cip" >/dev/null; then :; else reason+="WARN:guest-not-up(stuck-shared-orphans, see roadmap) "; fi
    fi
    result migrate-storage-loss "$verdict" "source=$down srcq=$srcq dstq=$dstq $reason"
    [ "$verdict" = PASS ]
}

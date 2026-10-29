# iSCSI path loss on the holder only (drop the portal with iptables).
# With the sanlock killpath, losing the LUN makes sanlock run `nox fence`,
# which SIGKILLs the guests using the pool and stops noxd, so the guest
# stops writing before any survivor can acquire. The host does NOT reset
# (that is the watchdog's job only if the fence cannot finish), so its
# iSCSI path is repaired afterwards. Expect: the holder's guest is gone
# before the survivor acquires, the survivor's data is intact, the pool is
# consistent, and the guest restarts on the survivor.
. "$HERE/scenarios/_crash.sh"

drop_path() {
    log "dropping iSCSI traffic to $TARGET_IP on $1"
    s "$1" "iptables -I OUTPUT -d $TARGET_IP -p tcp --dport 3260 -j DROP; iptables -I INPUT -s $TARGET_IP -p tcp --sport 3260 -j DROP; echo dropped" 15
}
restore_path() {
    s "$1" "iptables -D OUTPUT -d $TARGET_IP -p tcp --dport 3260 -j DROP 2>/dev/null; iptables -D INPUT -s $TARGET_IP -p tcp --sport 3260 -j DROP 2>/dev/null; iscsiadm -m session -R >/dev/null 2>&1; echo restored" 30
}
# A holder that fenced by killing its guest (not by resetting) is left with
# a wedged iSCSI session: this LIO fileio target does not re-present its LUN
# after a portal flap until a fresh login, so a rescan is not enough. Try a
# clean logout/login, and if the LUN still will not come back, reboot the
# node (it has no guests at this point).
repair_holder() {
    local ip=$1
    restore_path "$ip"
    s "$ip" "iscsiadm -m node -T $TARGET_IQN --logout >/dev/null 2>&1; sleep 2; iscsiadm -m node -T $TARGET_IQN --login >/dev/null 2>&1; sleep 4; udevadm settle" 60 >/dev/null 2>&1 || true
    if ! s "$ip" "test -e /dev/disk/by-id/scsi-$WWID" 15; then
        log "holder $ip did not re-present the LUN after re-login; rebooting it"
        s "$ip" "nohup reboot >/dev/null 2>&1 &" 10 || true
        sleep 20
        wait_node_back "$ip" || return 1
        return 0
    fi
    s "$ip" "systemctl restart $NOXD_UNIT" 30 >/dev/null 2>&1 || true
    wait_until 120 "noxd ready on $ip" sh -c "ssh -o BatchMode=yes root@$ip 'nox status 2>/dev/null' | grep -q 'ready true'" >/dev/null || true
}

scenario_run() {
    local holder target hip tip run t0 verdict=PASS reason=""
    holder=$(vm_node); [ -n "$holder" ] || die "guest $VMID not running"
    target=$(other_node "$holder"); hip=$(node_ip "$holder"); tip=$(node_ip "$target")
    log "path-loss-holder: holder $holder ($hip), survivor $target ($tip)"
    verifier_setup "$hip"
    run=$(( (RANDOM << 15) | RANDOM ))
    verifier_start "$hip" "$run"
    sleep 20
    log "writer progress: $(agent_exec "$hip" "tail -1 /root/writer.log")"

    t0=$(date +%s)
    drop_path "$hip"
    trap 'repair_holder "$hip"' EXIT

    # the survivor takes the verifier volume; this only succeeds once the
    # holder's lease has expired, which is after sanlock gave up and ran the
    # fence
    local vfy; if verifier_check "$tip" "$run" 360; then vfy=ok; else vfy=FAIL; fi
    local acquire_secs=$(( $(date +%s) - t0 ))
    log "survivor acquired after ${acquire_secs}s"

    # The holder's guest must not be alive once the survivor has the lease.
    # Two safe outcomes: the fence killed the guest and the host stayed up
    # (gentle), or the fence could not reap a wedged guest and the watchdog
    # reset the host (backstop). A live guest on a still-up holder is not.
    local outcome
    if host_down "$hip" || ! ssh_up "$hip"; then
        outcome=watchdog-reset
        log "holder reset by the watchdog (guest could not be reaped before expiry)"
    else
        local still qemu
        still=$(s "$hip" "/usr/sbin/nox fence --pool $POOL --dry-run 2>/dev/null | grep -c 'would SIGKILL' || true" 20)
        qemu=$(s "$hip" "pgrep -f '^/usr/bin/kvm -id $VMID ' | wc -l" 15)
        outcome="guest-killed(holders=$still,qemu=$qemu)"
        log "holder stayed up; $still process(es) on pool volumes, $qemu qemu for $VMID"
        [ "${still:-1}" = 0 ] || { verdict=FAIL; reason+="holder-still-holds "; }
        [ "${qemu:-1}" = 0 ] || { verdict=FAIL; reason+="holder-guest-alive "; }
    fi
    [ "$vfy" = ok ] || { verdict=FAIL; reason+="data-check "; }

    # bring the guest up on the survivor
    recover_quorum "$tip"
    move_config "$tip" "$holder" "$target" >/dev/null
    local t1=$(date +%s)
    s "$tip" "qm start $VMID" 400 | tail -1
    if agent_wait "$tip" >/dev/null; then :; else verdict=FAIL; reason+="guest-not-up "; fi
    local agent_secs=$(( $(date +%s) - t1 ))

    if [ "$outcome" = watchdog-reset ]; then wait_node_back "$hip" || { verdict=FAIL; reason+="holder-not-back "; }; else repair_holder "$hip"; fi
    trap - EXIT
    nox_check "$tip" || { verdict=FAIL; reason+="check "; }
    result path-loss-holder "$verdict" "acquire=${acquire_secs}s guest-up=${agent_secs}s outcome=${outcome} $reason"
    [ "$verdict" = PASS ]
}

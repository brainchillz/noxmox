# iSCSI target gone for everyone: disable the target portal group. Expect:
# every node that holds a volume lease is fenced, either by killing its
# guests and staying up (killpath) or by a watchdog reset; an idle node
# holds nothing and need not fence. Once the target returns the pool is
# consistent and the verifier data is intact. Takes the whole lab down.
scenario_run() {
    [ -n "$TARGET_SSH" ] || { log "TARGET_SSH not set, skipping"; result path-loss-both SKIP ""; return 0; }
    local holder hip run t0 n ip verdict=PASS reason=""
    holder=$(vm_node); [ -n "$holder" ] || die "guest not running"
    hip=$(node_ip "$holder")
    verifier_setup "$hip"; run=$(( (RANDOM << 15) | RANDOM )); verifier_start "$hip" "$run"; sleep 15

    declare -A held
    for n in $(node_names); do
        held[$n]=$(s "$(node_ip "$n")" "nox status 2>/dev/null | grep -c '^held' || true" 15 | tr -dc '0-9')
        held[$n]=${held[$n]:-0}
        log "$n holds ${held[$n]} leases before the outage"
    done

    reenable() { timeout 60 ssh -o BatchMode=yes "$TARGET_SSH" sudo targetcli "/iscsi/$TARGET_IQN/tpg1 enable" >/dev/null 2>&1 || true; }
    # a held node that fenced by killing its guests (not resetting) is "safe"
    # once nothing on it holds a pool volume; a reset node is safe when down.
    fenced() {
        local ip=$1
        host_down "$ip" && return 0
        local n; n=$(s "$ip" "/usr/sbin/nox fence --pool $POOL --dry-run 2>/dev/null | grep -c 'would SIGKILL' || true" 15 | tr -dc '0-9')
        [ "${n:-1}" = 0 ]
    }
    repair() {
        local ip=$1
        if host_down "$ip"; then wait_node_back "$ip"; return; fi
        s "$ip" "iscsiadm -m node -T $TARGET_IQN --logout >/dev/null 2>&1; sleep 2; iscsiadm -m node -T $TARGET_IQN --login >/dev/null 2>&1; sleep 4; udevadm settle" 60 >/dev/null 2>&1 || true
        if ! s "$ip" "test -e /dev/disk/by-id/scsi-$WWID" 15; then
            log "$ip did not re-present the LUN; rebooting it"
            s "$ip" "nohup reboot >/dev/null 2>&1 &" 10 || true; sleep 20; wait_node_back "$ip"
        else
            s "$ip" "systemctl restart $NOXD_UNIT" 30 >/dev/null 2>&1 || true
            wait_until 120 "noxd ready on $ip" sh -c "ssh -o BatchMode=yes root@$ip 'nox status 2>/dev/null' | grep -q 'ready true'" >/dev/null || true
        fi
    }

    t0=$(date +%s)
    log "disabling target portal group on $TARGET_SSH"
    timeout 60 ssh -o BatchMode=yes "$TARGET_SSH" sudo targetcli "/iscsi/$TARGET_IQN/tpg1 disable" 2>&1 | tail -1
    trap reenable EXIT

    declare -A outcome
    for n in $(node_names); do
        ip=$(node_ip "$n")
        if [ "${held[$n]:-0}" -gt 0 ]; then
            if wait_until 360 "$n to fence" fenced "$ip" >/dev/null; then
                outcome[$n]=$(host_down "$ip" && echo "reset@$(( $(date +%s)-t0 ))s" || echo "guest-killed@$(( $(date +%s)-t0 ))s")
            else
                outcome[$n]=NOT-FENCED; verdict=FAIL; reason+="$n-held-but-never-fenced "
            fi
            log "$n (holder): ${outcome[$n]}"
        else
            outcome[$n]=idle; log "$n held nothing, not required to fence"
        fi
    done

    log "re-enabling target"; reenable; trap - EXIT
    for n in $(node_names); do repair "$(node_ip "$n")"; done
    for n in $(node_names); do
        ip=$(node_ip "$n"); local stale; stale=$(s "$ip" "dmsetup ls | grep -c '^$POOL-' || true" 20 | tr -dc '0-9')
        [ "${stale:-0}" = 0 ] || { verdict=FAIL; reason+="$n-stale-dm:$stale "; }
    done
    # pick a healthy node to verify from
    local hn=""; for n in $(node_names); do s "$(node_ip "$n")" "nox status 2>/dev/null | grep -q 'ready true'" 15 && hn=$(node_ip "$n") && break; done
    [ -n "$hn" ] || { result path-loss-both FAIL "no-node-ready"; return 1; }
    recover_quorum "$hn"
    if verifier_check "$hn" "$run" 240; then :; else verdict=FAIL; reason+="data-check "; fi
    nox_check "$hn" || { verdict=FAIL; reason+="check "; }
    local cfgnode; for n in $(node_names); do s "$(node_ip "$n")" "test -e /etc/pve/nodes/$n/qemu-server/$VMID.conf" 15 && cfgnode=$n; done
    [ -n "${cfgnode:-}" ] && { s "$(node_ip "$cfgnode")" "qm start $VMID" 400 | tail -1; agent_wait "$(node_ip "$cfgnode")" >/dev/null || { verdict=FAIL; reason+="guest-not-up "; }; }
    local summary=""; for n in $(node_names); do summary+="$n=${outcome[$n]} "; done
    result path-loss-both "$verdict" "$summary$reason"
    [ "$verdict" = PASS ]
}

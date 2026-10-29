# iSCSI target gone for everyone: disable the target portal group. Expect:
# every node that holds a volume lease fences itself (an idle node holds
# nothing, so it has nothing to fence for and need not), the pool is
# consistent once the target returns, and the guest is restartable.
# Resets every node that was running a guest on the pool.
scenario_run() {
    [ -n "$TARGET_SSH" ] || { log "TARGET_SSH not set, skipping"; result path-loss-both SKIP ""; return 0; }
    local holder hip run t0 n ip verdict=PASS reason=""
    holder=$(vm_node); [ -n "$holder" ] || die "guest not running"
    hip=$(node_ip "$holder")
    verifier_setup "$hip"; run=$(( (RANDOM << 15) | RANDOM )); verifier_start "$hip" "$run"; sleep 15

    # which nodes hold a lease now: only these must fence
    declare -A held
    for n in $(node_names); do
        ip=$(node_ip "$n")
        held[$n]=$(s "$ip" "nox status 2>/dev/null | grep -c '^held'" 15 || echo 0)
        log "$n holds ${held[$n]} leases before the outage"
    done

    local tcli="timeout 60 ssh -o BatchMode=yes $TARGET_SSH sudo targetcli"
    reenable() { timeout 60 ssh -o BatchMode=yes "$TARGET_SSH" sudo targetcli "/iscsi/$TARGET_IQN/tpg1 enable" >/dev/null 2>&1 || true; }
    t0=$(date +%s)
    log "disabling target portal group on $TARGET_SSH"
    $tcli "/iscsi/$TARGET_IQN/tpg1 disable" 2>&1 | tail -1
    trap reenable EXIT

    declare -A downat
    for n in $(node_names); do
        ip=$(node_ip "$n")
        if [ "${held[$n]:-0}" -gt 0 ]; then
            if wait_until 300 "$n to fence" host_down "$ip" >/dev/null; then downat[$n]=$(( $(date +%s) - t0 )); else downat[$n]=none; verdict=FAIL; reason+="$n-held-but-never-fenced "; fi
            log "$n (holder) off the network after ${downat[$n]}s"
        else
            downat[$n]=idle
            log "$n held nothing, not required to fence"
        fi
    done

    log "re-enabling target"
    reenable; trap - EXIT
    for n in $(node_names); do wait_node_back "$(node_ip "$n")" || { verdict=FAIL; reason+="$n-not-back "; }; done
    for n in $(node_names); do
        ip=$(node_ip "$n"); local stale; stale=$(s "$ip" "dmsetup ls | grep -c '^nox-' || true" 20)
        [ "${stale:-0}" = 0 ] || { verdict=FAIL; reason+="$n-stale-dm:$stale "; }
    done
    recover_quorum "$hip"
    if verifier_check "$hip" "$run" 240; then :; else verdict=FAIL; reason+="data-check "; fi
    nox_check "$hip" || { verdict=FAIL; reason+="check "; }
    # restart the guest wherever its config now lives
    local cfgnode; for n in $(node_names); do s "$(node_ip "$n")" "test -e /etc/pve/nodes/$n/qemu-server/$VMID.conf" 15 && cfgnode=$n; done
    [ -n "${cfgnode:-}" ] && { s "$(node_ip "$cfgnode")" "qm start $VMID" 400 | tail -1; agent_wait "$(node_ip "$cfgnode")" >/dev/null || { verdict=FAIL; reason+="guest-not-up "; }; }
    local summary=""; for n in $(node_names); do summary+="$n=${downat[$n]} "; done
    result path-loss-both "$verdict" "fenced: $summary$reason"
    [ "$verdict" = PASS ]
}

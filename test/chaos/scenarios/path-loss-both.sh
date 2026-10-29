# iSCSI target gone for everyone: disable the target portal group. Expect:
# every node fences itself (documented behaviour), and the pool is
# consistent once the target is back. Resets every node in the lab.
scenario_run() {
    [ -n "$TARGET_SSH" ] || { log "TARGET_SSH not set, skipping"; result path-loss-both SKIP ""; return 0; }
    local holder hip tip target run t0 n ip verdict=PASS reason=""
    holder=$(vm_node); [ -n "$holder" ] || die "guest not running"
    hip=$(node_ip "$holder"); target=$(other_node "$holder"); tip=$(node_ip "$target")
    verifier_setup "$hip"; run=$(( (RANDOM << 15) | RANDOM )); verifier_start "$hip" "$run"; sleep 15
    local tc="timeout 60 ssh -o BatchMode=yes $TARGET_SSH sudo targetcli"
    t0=$(date +%s)
    log "disabling target portal group on $TARGET_SSH"
    $tc "/iscsi/$TARGET_IQN/tpg1 disable" 2>&1 | tail -1
    trap '$tc "/iscsi/$TARGET_IQN/tpg1 enable" >/dev/null 2>&1 || true' EXIT
    declare -A downat
    for n in $(node_names); do
        ip=$(node_ip "$n")
        if wait_until 400 "$n to fence" host_down "$ip" >/dev/null; then downat[$n]=$(( $(date +%s) - t0 )); else downat[$n]=none; verdict=FAIL; reason+="$n-never-fenced "; fi
        log "$n off the network after ${downat[$n]}s"
    done
    log "re-enabling target"
    $tc "/iscsi/$TARGET_IQN/tpg1 enable" 2>&1 | tail -1
    for n in $(node_names); do wait_node_back "$(node_ip "$n")" || { verdict=FAIL; reason+="$n-not-back "; }; done
    for n in $(node_names); do
        ip=$(node_ip "$n"); local stale; stale=$(s "$ip" "dmsetup ls | grep -c '^nox-' || true" 20)
        [ "${stale:-0}" = 0 ] || { verdict=FAIL; reason+="$n-stale-dm "; }
    done
    recover_quorum "$hip"
    if verifier_check "$hip" "$run" 240; then :; else verdict=FAIL; reason+="data-check "; fi
    nox_check "$hip" || { verdict=FAIL; reason+="check "; }
    s "$hip" "qm start $VMID" 400 | tail -1
    agent_wait "$hip" >/dev/null || { verdict=FAIL; reason+="guest-not-up "; }
    local summary=""; for n in $(node_names); do summary+="$n=${downat[$n]}s "; done
    result path-loss-both "$verdict" "fenced: $summary$reason"
    [ "$verdict" = PASS ]
}

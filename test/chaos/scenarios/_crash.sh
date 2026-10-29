# Shared body for the scenarios that take the lease holder down while the
# guest is writing. KILL_FN is called with the holder's ip; it must make the
# node reset (directly or through fencing).
#
# Measures: seconds from the kill to the holder dropping off the network,
# seconds until the survivor acquires the verifier volume, and seconds to a
# working guest on the survivor. Checks: every acknowledged block is intact
# and at most the block in flight is torn, the survivor acquires only after
# the holder is dead, and the holder comes back with no stale dm devices.
crash_scenario() {
    local name=$1 kill_fn=$2 reset_max=$3 acquire_max=$4
    local holder target hip tip run t0 reset_secs start_secs agent_secs vfy
    holder=$(vm_node); [ -n "$holder" ] || die "guest $VMID is not running"
    target=$(other_node "$holder"); hip=$(node_ip "$holder"); tip=$(node_ip "$target")
    log "$name: holder $holder ($hip), survivor $target ($tip)"
    verifier_setup "$hip"
    run=$(( (RANDOM << 15) | RANDOM ))
    verifier_start "$hip" "$run"
    sleep 20
    log "writer progress: $(agent_exec "$hip" "tail -1 /root/writer.log")"

    t0=$(date +%s)
    $kill_fn "$hip"
    # background monitor: when does the holder vanish?
    local mon; mon=$(mktemp)
    ( while host_up "$hip"; do sleep 1; done; echo $(( $(date +%s) - t0 )) > "$mon" ) &
    local monpid=$!

    # the survivor tries to take the verifier volume; this only succeeds once
    # the holder's lease has expired
    if verifier_check "$tip" "$run" "$acquire_max"; then vfy=ok; else vfy=FAIL; fi
    local acquire_secs=$(( $(date +%s) - t0 ))
    wait "$monpid" 2>/dev/null || true
    reset_secs=$(cat "$mon" 2>/dev/null || echo "none"); rm -f "$mon"
    log "holder off the network after ${reset_secs}s, survivor acquired after ${acquire_secs}s"

    local verdict=PASS reason=""
    [ "$vfy" = ok ] || { verdict=FAIL; reason+="data-check "; }
    if [ "$reset_secs" = none ]; then verdict=FAIL; reason+="holder-never-reset "; 
    elif [ "$reset_secs" -gt "$reset_max" ]; then verdict=FAIL; reason+="reset-too-slow "; fi
    if [ "$reset_secs" != none ] && [ "$acquire_secs" -lt "$reset_secs" ]; then verdict=FAIL; reason+="acquired-before-fence "; fi

    # bring the guest up on the survivor
    recover_quorum "$tip"
    move_config "$tip" "$holder" "$target" >/dev/null
    local t1=$(date +%s)
    s "$tip" "qm start $VMID" 400 | tail -2
    start_secs=$(( $(date +%s) - t1 ))
    if agent_wait "$tip" >/dev/null; then agent_secs=$(( $(date +%s) - t1 )); else agent_secs=none; verdict=FAIL; reason+="guest-not-up "; fi
    log "guest started in ${start_secs}s, agent answering after ${agent_secs}s"

    wait_node_back "$hip" || { verdict=FAIL; reason+="holder-not-back "; }
    local stale; stale=$(s "$hip" "dmsetup ls | grep -c '^nox-' || true" 20)
    [ "${stale:-0}" = 0 ] || { verdict=FAIL; reason+="stale-dm:$stale "; }
    nox_check "$tip" || { verdict=FAIL; reason+="check "; }
    result "$name" "$verdict" "reset=${reset_secs}s acquire=${acquire_secs}s start=${start_secs}s agent=${agent_secs}s $reason"
    [ "$verdict" = PASS ]
}

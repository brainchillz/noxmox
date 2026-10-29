# Put one node's clock five minutes ahead and migrate there and back.
# Expect: no effect, sanlock and the daemon use monotonic time.
scenario_run() {
    local holder target hip tip verdict=PASS reason="" t0 mig1 mig2
    holder=$(vm_node); target=$(other_node "$holder"); hip=$(node_ip "$holder"); tip=$(node_ip "$target")
    log "skewing clock on $target by +5 min"
    s "$tip" "systemctl stop chrony; date -s '+5 minutes' >/dev/null; date" 20
    restore_clock() { s "$tip" "systemctl start chrony; sleep 2; chronyc makestep >/dev/null; date" 30; }
    sleep 25
    s "$tip" "nox status | grep '^host '" 20 | grep -q 'live' || { verdict=FAIL; reason+="hosts-not-live "; }
    t0=$(date +%s); s "$hip" "qm migrate $VMID $target --online" 600 | grep -E 'downtime|ERROR' | tail -1; mig1=$(( $(date +%s) - t0 ))
    [ "$(vm_node)" = "$target" ] || { verdict=FAIL; reason+="migrate-to-skewed "; }
    t0=$(date +%s); s "$tip" "qm migrate $VMID $holder --online" 600 | grep -E 'downtime|ERROR' | tail -1; mig2=$(( $(date +%s) - t0 ))
    [ "$(vm_node)" = "$holder" ] || { verdict=FAIL; reason+="migrate-back "; }
    sleep 5
    # everything the guest uses is back to exclusive on its node, and the
    # other node holds nothing of the guest
    [ "$(s "$hip" "nox status | grep '^held vm-$VMID-' | grep -c '(sh)' || true" 20)" = 0 ] || { verdict=FAIL; reason+="source-still-shared "; }
    [ "$(s "$tip" "nox status | grep -c '^held vm-$VMID-' || true" 20)" = 0 ] || { verdict=FAIL; reason+="target-still-holds "; }
    nox_check "$hip" || { verdict=FAIL; reason+="check "; }
    restore_clock
    result clock-skew "$verdict" "migrate-there=${mig1}s back=${mig2}s $reason"
    [ "$verdict" = PASS ]
}

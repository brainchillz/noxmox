# Kill the target QEMU in the middle of a live migration. Expect: the guest
# keeps running on the source, the source ends up exclusive again, the
# target holds nothing and has no dm devices left.
scenario_run() {
    local holder target hip tip t0 verdict=PASS reason=""
    holder=$(vm_node); target=$(other_node "$holder"); hip=$(node_ip "$holder"); tip=$(node_ip "$target")
    log "migrating $VMID $holder -> $target and killing the target QEMU"
    s "$hip" "nohup qm migrate $VMID $target --online > /tmp/chaos-migrate.log 2>&1 &" 15
    wait_until 120 "target qemu" s "$tip" "pgrep -f 'kvm.*-id $VMID' >/dev/null" 10 >/dev/null || { result migrate-abort FAIL "target-never-started"; return 1; }
    sleep 4
    s "$tip" "pkill -9 -f 'kvm.*-id $VMID'; echo killed" 15
    wait_until 300 "migrate command to finish" s "$hip" "! pgrep -f 'qm migrate $VMID' >/dev/null" 10 >/dev/null
    log "migrate log: $(s "$hip" "grep -E 'ERROR|failed|aborted' /tmp/chaos-migrate.log | tail -2" 10)"
    s "$hip" "qm status $VMID" 20 | grep -q running || { verdict=FAIL; reason+="guest-not-on-source "; }
    agent_wait "$hip" >/dev/null || { verdict=FAIL; reason+="guest-agent "; }
    # the source converts back to exclusive within its grace period
    t0=$(date +%s)
    local back; back=$(wait_until 150 "source exclusive" sh -c "ssh -o BatchMode=yes root@$hip 'nox status' | grep '^held' | grep -vq '(sh)'") || { verdict=FAIL; reason+="source-not-exclusive "; }
    local theld tdm
    theld=$(s "$tip" "nox status | grep -c '^held' || true" 20); tdm=$(s "$tip" "dmsetup ls | grep -c '^nox-vm-$VMID' || true" 20)
    [ "${theld:-0}" = 0 ] || { verdict=FAIL; reason+="target-holds:$theld "; }
    [ "${tdm:-0}" = 0 ] || { verdict=FAIL; reason+="target-dm:$tdm "; }
    nox_check "$hip" || { verdict=FAIL; reason+="check "; }
    result migrate-abort "$verdict" "source-ex-after=${back:-none}s target-held=$theld target-dm=$tdm $reason"
    [ "$verdict" = PASS ]
}

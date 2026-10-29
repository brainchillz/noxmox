# SIGKILL noxd on the holder while the guest writes. Expect: the guest never
# notices, the restarted daemon adopts its orphan leases.
scenario_run() {
    local holder hip run before after t0 ready verdict=PASS reason=""
    holder=$(vm_node); hip=$(node_ip "$holder")
    verifier_setup "$hip"; run=$(( (RANDOM << 15) | RANDOM )); verifier_start "$hip" "$run"; sleep 10
    before=$(s "$hip" "nox status | grep -c '^held'" 20)
    t0=$(date +%s)
    s "$hip" "systemctl kill -s KILL noxd; echo killed" 15
    ready=$(wait_until 120 "noxd ready" sh -c "ssh -o BatchMode=yes root@$hip 'nox status 2>/dev/null' | grep -q 'ready true'") || { verdict=FAIL; reason+="not-ready "; }
    after=$(s "$hip" "nox status | grep -c '^held'" 20)
    local adopted; adopted=$(s "$hip" "journalctl -u noxd --since '-3min' --no-pager -o cat | grep -c 'adopted orphan lease'" 20)
    log "held before=$before after=$after adopted=$adopted ready in ${ready:-?}s"
    [ "$after" = "$before" ] || { verdict=FAIL; reason+="held-changed "; }
    [ "${adopted:-0}" -ge "$before" ] || { verdict=FAIL; reason+="not-adopted "; }
    s "$hip" "qm status $VMID" 20 | grep -q running || { verdict=FAIL; reason+="guest-died "; }
    sleep 5
    local prog; prog=$(verifier_stop "$hip"); log "writer: $prog"
    echo "$prog" | grep -qE 'acked|done' || { verdict=FAIL; reason+="writer-stalled "; }
    result noxd-kill "$verdict" "ready=${ready:-none}s held=$after adopted=$adopted $reason"
    [ "$verdict" = PASS ]
}

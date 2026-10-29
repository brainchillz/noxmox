# SIGKILL noxd in the middle of creates. Expect: no half record is ever
# visible, every create either completed or failed cleanly, nox check is
# clean, and the slot leases of failed creates are not held by anyone.
scenario_run() {
    local node ip i ok=0 failed=0 verdict=PASS reason=""
    node=$(other_node "$(vm_node)"); ip=$(node_ip "$node")
    log "noxd-kill-create on $node"
    for i in $(seq 1 25); do
        s "$ip" "( nox create vm-7777-chaos-$i 256M >/tmp/chaos-create.out 2>&1; echo \$? > /tmp/chaos-create.rc ) & sleep 0.\$((RANDOM % 9)); systemctl kill -s KILL noxd; wait" 30 >/dev/null
        wait_until 60 "noxd ready" sh -c "ssh -o BatchMode=yes root@$ip 'nox status 2>/dev/null' | grep -q 'ready true'" >/dev/null || { verdict=FAIL; reason+="noxd-not-back "; break; }
        if [ "$(s "$ip" "cat /tmp/chaos-create.rc" 10)" = 0 ]; then ok=$((ok+1)); else failed=$((failed+1)); fi
        # the table must agree with the exit status: present iff rc==0,
        # except a create that was committed on disk just before the kill
        # and could not report (counted as a success on disk)
        if ! nox_check "$ip"; then verdict=FAIL; reason+="check-after-$i "; fi
    done
    local present; present=$(s "$ip" "nox list | grep -c vm-7777-chaos" 20)
    log "creates reported ok=$ok failed=$failed, volumes present=$present"
    [ "$present" -ge "$ok" ] || { verdict=FAIL; reason+="lost-volume "; }
    for i in $(seq 1 25); do s "$ip" "nox delete vm-7777-chaos-$i" 30 >/dev/null 2>&1 || true; done
    [ "$(s "$ip" "nox list | grep -c vm-7777-chaos" 20)" = 0 ] || { verdict=FAIL; reason+="cleanup "; }
    nox_check "$ip" || { verdict=FAIL; reason+="final-check "; }
    result noxd-kill-create "$verdict" "ok=$ok failed=$failed present=$present $reason"
    [ "$verdict" = PASS ]
}

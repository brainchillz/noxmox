# Standalone / cross-cluster safety: prove the ownership refusal happens at
# the on-disk lease, independent of Proxmox and corosync. Driven entirely
# through the nox CLI (no qm, no cluster), so it models two nodes that are
# NOT in the same Corosync domain pointed at the same LUN. One node takes a
# volume exclusive; the other's exclusive activate must be refused and must
# name the holder; after the holder releases, the other acquires. This is
# the guarantee the lease gives that quorum cannot (quorum only arbitrates
# within one cluster).
scenario_run() {
    local a b aip bip vol verdict=PASS reason=""
    local nodes=($(node_names))
    a=${nodes[0]}; b=${nodes[1]}; aip=$(node_ip "$a"); bip=$(node_ip "$b")
    vol="vm-7709-standalone"
    log "standalone-refusal: holder $a ($aip), contender $b ($bip)"

    s "$aip" "nox create $vol 256M" 60 >/dev/null 2>&1 || true
    # A takes it exclusive
    s "$aip" "nox activate $vol --timeout 10" 30 >/dev/null || { result standalone-refusal FAIL "A-activate"; s "$aip" "nox deactivate $vol; nox delete $vol" 60 >/dev/null 2>&1; return 1; }
    log "$a holds $vol exclusive"

    # B must be refused, and the message must name A's host id
    local out ahid
    ahid=$(s "$aip" "nox status | grep -oE 'host id [0-9]+' | grep -oE '[0-9]+'" 15 | head -1)
    out=$(s "$bip" "nox activate $vol --timeout 5" 30 2>&1)
    if echo "$out" | grep -qiE 'held by host|in use'; then
        log "$b refused as expected: $(echo "$out" | tail -1 | cut -c1-80)"
        echo "$out" | grep -q "host $ahid" || { verdict=FAIL; reason+="refusal-did-not-name-holder($ahid) "; }
    else
        verdict=FAIL; reason+="B-not-refused:$(echo "$out"|tail -1|cut -c1-60) "
        s "$bip" "nox deactivate $vol" 30 >/dev/null 2>&1
    fi

    # A releases; B may now acquire
    s "$aip" "nox deactivate $vol" 30 >/dev/null
    if s "$bip" "nox activate $vol --timeout 20" 40 >/dev/null; then
        log "$b acquired after $a released"
        s "$bip" "nox deactivate $vol" 30 >/dev/null
    else
        verdict=FAIL; reason+="B-could-not-acquire-after-release "
    fi

    s "$aip" "nox delete $vol" 60 >/dev/null 2>&1
    nox_check "$aip" || { verdict=FAIL; reason+="check "; }
    result standalone-refusal "$verdict" "holder=$a refusal-names-host=$ahid $reason"
    [ "$verdict" = PASS ]
}

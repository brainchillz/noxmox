# iSCSI target gone for everyone while nothing is held: stop the guest, then
# disable the target portal group for longer than sanlock's fail timeout.
# Expect: no node fences (there is no lease to kill for), and once the target
# is back every daemon notices on its own that sanlock dropped its lockspace,
# rejoins without a restart, and the pool is usable from both nodes. Guards
# the bug where an idle daemon kept reporting ready with no lockspace and
# every operation failed with "sanlock acquire: rv -28" until restarted.
scenario_run() {
    [ -n "$TARGET_SSH" ] || { log "TARGET_SSH not set, skipping"; result path-loss-idle SKIP ""; return 0; }
    local n ip verdict=PASS reason="" t0 cfgnode
    for n in $(node_names); do s "$(node_ip "$n")" "test -e /etc/pve/nodes/$n/qemu-server/$VMID.conf" 15 && cfgnode=$n; done
    [ -n "${cfgnode:-}" ] && s "$(node_ip "$cfgnode")" "qm stop $VMID --timeout 60 >/dev/null 2>&1 || true" 90 >/dev/null
    for n in $(node_names); do
        ip=$(node_ip "$n")
        wait_until 60 "$n idle" sh -c "ssh -o BatchMode=yes root@$ip 'nox status 2>/dev/null' | grep -qv '^held'" >/dev/null || true
        local held; held=$(s "$ip" "nox status 2>/dev/null | grep -c '^held' || true" 15 | tr -dc '0-9')
        [ "${held:-0}" = 0 ] || die "$n still holds ${held} lease(s); the scenario needs an idle pool"
    done
    local before; declare -A before
    for n in $(node_names); do before[$n]=$(s "$(node_ip "$n")" "nox metrics 2>/dev/null | awk '/^nox_lockspace_lost_total/{print \$2}'" 15 | tr -dc '0-9'); before[$n]=${before[$n]:-0}; done

    reenable() { timeout 60 ssh -o BatchMode=yes "$TARGET_SSH" sudo targetcli "/iscsi/$TARGET_IQN/tpg1 enable" >/dev/null 2>&1 || true; }
    t0=$(date +%s)
    log "disabling target portal group on $TARGET_SSH for 100 s (sanlock gives up after 8 x io_timeout)"
    timeout 60 ssh -o BatchMode=yes "$TARGET_SSH" sudo targetcli "/iscsi/$TARGET_IQN/tpg1 disable" 2>&1 | tail -1
    trap reenable EXIT
    sleep 100
    for n in $(node_names); do
        ip=$(node_ip "$n")
        host_down "$ip" && { verdict=FAIL; reason+="$n-reset-while-idle "; }
    done
    log "re-enabling target"; reenable; trap - EXIT
    for n in $(node_names); do
        ip=$(node_ip "$n")
        s "$ip" "iscsiadm -m session --rescan >/dev/null 2>&1; udevadm settle" 60 >/dev/null 2>&1 || true
        if wait_until 120 "$n ready again without restart" sh -c "ssh -o BatchMode=yes root@$ip 'nox status 2>/dev/null' | grep -q 'ready true'" >/dev/null; then
            log "$n ready at $(( $(date +%s)-t0 ))s"
        else
            verdict=FAIL; reason+="$n-never-ready "
        fi
        local after; after=$(s "$ip" "nox metrics 2>/dev/null | awk '/^nox_lockspace_lost_total/{print \$2}'" 15 | tr -dc '0-9')
        [ "${after:-0}" -gt "${before[$n]}" ] || { verdict=FAIL; reason+="$n-loss-not-counted "; }
        local restarted; restarted=$(s "$ip" "journalctl -u $NOXD_UNIT --since '-10min' --no-pager | grep -c 'joined lockspace' || true" 20 | tr -dc '0-9')
        [ "${restarted:-0}" -ge 1 ] || { verdict=FAIL; reason+="$n-no-rejoin-logged "; }
    done
    for n in $(node_names); do
        ip=$(node_ip "$n")
        s "$ip" "nox create chaos-idle-$n 1G >/dev/null && nox delete chaos-idle-$n >/dev/null" 60 >/dev/null 2>&1 || { verdict=FAIL; reason+="$n-create-delete "; }
    done
    local hn; hn=$(node_ip "$(node_names | head -1)")
    nox_check "$hn" || { verdict=FAIL; reason+="check "; }
    [ -n "${cfgnode:-}" ] && { s "$(node_ip "$cfgnode")" "qm start $VMID" 400 | tail -1; agent_wait "$(node_ip "$cfgnode")" >/dev/null || { verdict=FAIL; reason+="guest-not-up "; }; }
    result path-loss-idle "$verdict" "$reason"
    [ "$verdict" = PASS ]
}

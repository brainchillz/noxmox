# iSCSI path loss on the holder only (drop the portal with iptables).
# Expect: sanlock cannot renew, wdmd resets the node, the guest is
# restartable on the survivor. The rule disappears with the reboot.
. "$HERE/scenarios/_crash.sh"
drop_path() {
    log "dropping iSCSI traffic to $TARGET_IP on $1"
    s "$1" "iptables -I OUTPUT -d $TARGET_IP -p tcp --dport 3260 -j DROP; iptables -I INPUT -s $TARGET_IP -p tcp --sport 3260 -j DROP; echo dropped" 15
}
scenario_run() {
    local holder hip; holder=$(vm_node); hip=$(node_ip "$holder")
    # safety: if the node never fences, lift the rule so it is not left broken
    trap 's "$hip" "iptables -D OUTPUT -d $TARGET_IP -p tcp --dport 3260 -j DROP; iptables -D INPUT -s $TARGET_IP -p tcp --sport 3260 -j DROP" 15 >/dev/null 2>&1 || true' EXIT
    crash_scenario path-loss-holder drop_path 300 360
}

# iSCSI path loss on the holder only (drop the portal with iptables).
# Expect: sanlock cannot renew, wdmd resets the node, the guest is
# restartable on the survivor. The rule disappears with the reboot.
. "$HERE/scenarios/_crash.sh"
drop_path() {
    log "dropping iSCSI traffic to $TARGET_IP on $1"
    s "$1" "iptables -I OUTPUT -d $TARGET_IP -p tcp --dport 3260 -j DROP; iptables -I INPUT -s $TARGET_IP -p tcp --sport 3260 -j DROP; echo dropped" 15
}
restore_path() {
    log "restoring iSCSI traffic on $1"
    s "$1" "iptables -D OUTPUT -d $TARGET_IP -p tcp --dport 3260 -j DROP; iptables -D INPUT -s $TARGET_IP -p tcp --sport 3260 -j DROP; echo restored" 15
}
scenario_run() {
    # if the node never fences, the shared body kills its guest first and
    # then calls UNDO_FN so the node is not left without its storage
    UNDO_FN=restore_path
    crash_scenario path-loss-holder drop_path 300 360
}

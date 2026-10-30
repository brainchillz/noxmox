#!/bin/bash
# Chaos suite runner. Usage: test/chaos/run.sh [scenario ...]
# With no arguments runs the default set; "all" adds the scenarios that take
# the whole lab down. Logs go to test/chaos/logs/, results to results.tsv.
set -u
HERE=$(cd "$(dirname "$0")" && pwd)
. "$HERE/lib.sh"

DEFAULT="standalone-refusal noxd-kill noxd-kill-create concurrent-create-delete delete-vs-activate migrate-abort clock-skew hard-reset sanlock-freeze path-loss-holder"
ALL="$DEFAULT path-loss-both"
case "${1:-}" in
    "") set -- $DEFAULT ;;
    all) set -- $ALL ;;
    list) echo "$ALL" | tr ' ' '\n'; exit 0 ;;
esac

mkdir -p "$HERE/logs"
preflight() {
    local n ip
    for n in $(node_names); do
        ip=$(node_ip "$n")
        ssh_up "$ip" || die "$n ($ip) unreachable"
        s "$ip" "nox status" 20 | grep -q 'ready true' || die "noxd not ready on $n"
    done
    [ -n "$(vm_node)" ] || die "guest $VMID is not running anywhere"
}
preflight
status=0
for sc in "$@"; do
    f="$HERE/scenarios/$sc.sh"
    [ -f "$f" ] || { log "no such scenario: $sc"; status=1; continue; }
    logf="$HERE/logs/$(date +%Y%m%d-%H%M%S)-$sc.log"
    log "=== $sc (log: $logf)"
    ( . "$f"; scenario_run ) 2>&1 | tee "$logf"
    rc=${PIPESTATUS[0]}
    [ "$rc" = 0 ] || status=1
    # leave the lab settled before the next scenario
    sleep 10
done
echo; echo "results ($RESULTS):"; tail -n "$#" "$RESULTS" 2>/dev/null | cut -f2- | column -t -s $'\t'
exit $status

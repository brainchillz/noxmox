# Create and delete from both nodes at once, including fights over the
# same name. Contested names are not deleted inside the loop, so a name
# that both nodes "won" would be a duplicate record. Expect: the metadata
# lease serialises everything, every contested name exists exactly once at
# the end and was won exactly once, the table has no duplicates or
# overlaps, nothing is left behind after cleanup.
scenario_run() {
    local ips=() n i pids=() iters=${CHAOS_ITERS:-200} verdict=PASS reason="" t0
    for n in $(node_names); do ips+=("$(node_ip "$n")"); done
    local script='i=0; ok=0; lost=0; won=0; while [ $i -lt ITERS ]; do i=$((i+1));
      nox create vm-7701-NODE-$i 256M >/dev/null 2>&1 && ok=$((ok+1));
      if nox create vm-7700-shared-$i 256M >/dev/null 2>&1; then won=$((won+1)); else lost=$((lost+1)); fi;
      nox delete vm-7701-NODE-$i >/dev/null 2>&1; done; echo "ok=$ok won=$won lost=$lost"'
    t0=$(date +%s)
    local k=0 out=()
    for ip in "${ips[@]}"; do
        k=$((k+1)); out+=("$(mktemp)")
        s "$ip" "$(echo "$script" | sed "s/ITERS/$iters/; s/NODE/$k/")" 3600 > "${out[-1]}" &
        pids+=($!)
    done
    for p in "${pids[@]}"; do wait "$p" || true; done
    local secs=$(( $(date +%s) - t0 )) won=0 lost=0 oks=0
    for f in "${out[@]}"; do log "node result: $(cat "$f")"; won=$((won + $(sed -n 's/.*won=\([0-9]*\).*/\1/p' "$f"))); lost=$((lost + $(sed -n 's/.*lost=\([0-9]*\).*/\1/p' "$f"))); oks=$((oks + $(sed -n 's/.*ok=\([0-9]*\).*/\1/p' "$f"))); rm -f "$f"; done
    # each contested name must be won by exactly one node and exist once
    [ "$won" = "$iters" ] || { verdict=FAIL; reason+="won=$won/$iters "; }
    [ "$oks" = $((iters * ${#ips[@]})) ] || { verdict=FAIL; reason+="private-creates=$oks "; }
    local shared; shared=$(s "${ips[0]}" "nox list | grep -c 'vm-7700-shared-' || true" 20)
    [ "${shared:-0}" = "$iters" ] || { verdict=FAIL; reason+="shared-present=$shared "; }
    nox_check "${ips[0]}" || { verdict=FAIL; reason+="check "; }
    s "${ips[0]}" "for i in \$(seq 1 $iters); do nox delete vm-7700-shared-\$i >/dev/null 2>&1; done" 1800 >/dev/null
    local left; left=$(s "${ips[0]}" "nox list | grep -cE 'vm-770[01]-' || true" 20)
    [ "${left:-0}" = 0 ] || { verdict=FAIL; reason+="leftover=$left "; }
    nox_check "${ips[0]}" || { verdict=FAIL; reason+="final-check "; }
    result concurrent-create-delete "$verdict" "iters=$iters nodes=${#ips[@]} won=$won lost=$lost secs=$secs ops/s=$(( (iters * 3 * ${#ips[@]}) / (secs > 0 ? secs : 1) )) $reason"
    [ "$verdict" = PASS ]
}

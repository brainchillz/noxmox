# One node deletes a volume while the other activates it. Expect: exactly
# one wins every round and the loser gets a clear error.
scenario_run() {
    local a b i wins_del=0 wins_act=0 verdict=PASS reason="" ra rb
    a=$(node_ip "$(vm_node)"); b=$(node_ip "$(other_node "$(vm_node)")")
    for i in $(seq 1 ${CHAOS_ROUNDS:-10}); do
        s "$a" "nox create vm-7703-race-$i 256M" 30 >/dev/null || { verdict=FAIL; reason+="create-$i "; continue; }
        local fa fb; fa=$(mktemp); fb=$(mktemp)
        s "$a" "nox delete vm-7703-race-$i" 60 > "$fa" 2>&1 & local pa=$!
        s "$b" "nox activate vm-7703-race-$i --timeout 5" 60 > "$fb" 2>&1 & local pb=$!
        wait $pa; ra=$?; wait $pb; rb=$?
        if [ $ra = 0 ] && [ $rb != 0 ]; then
            wins_del=$((wins_del+1)); grep -qiE 'not found|in use' "$fb" || { verdict=FAIL; reason+="unclear-activate-error:$(tr -d '\n' < "$fb") "; }
        elif [ $ra != 0 ] && [ $rb = 0 ]; then
            wins_act=$((wins_act+1)); grep -qi 'in use' "$fa" || { verdict=FAIL; reason+="unclear-delete-error:$(tr -d '\n' < "$fa") "; }
            s "$b" "nox deactivate vm-7703-race-$i && nox delete vm-7703-race-$i" 60 >/dev/null
        else
            verdict=FAIL; reason+="round-$i:del=$ra,act=$rb "
            s "$b" "nox deactivate vm-7703-race-$i" 30 >/dev/null 2>&1; s "$a" "nox delete vm-7703-race-$i" 30 >/dev/null 2>&1
        fi
        rm -f "$fa" "$fb"
    done
    nox_check "$a" || { verdict=FAIL; reason+="check "; }
    result delete-vs-activate "$verdict" "delete-won=$wins_del activate-won=$wins_act $reason"
    [ "$verdict" = PASS ]
}

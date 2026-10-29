#!/bin/bash
# Reproduces the numbers quoted in the README on the lab described by
# test/chaos/lab.env: activation latency, refusal latency for a held volume,
# online resize, and live migration wall time and downtime. Dead-host and
# fencing timings come from the chaos suite's results.tsv when present.
# Prints a markdown table and the commit it measured.
set -u
HERE=$(cd "$(dirname "$0")" && pwd)
. "$HERE/chaos/lib.sh"
N=${N:-5}

holder=$(vm_node); [ -n "$holder" ] || die "guest $VMID not running"
other=$(other_node "$holder"); hip=$(node_ip "$holder"); oip=$(node_ip "$other")
commit=$(git -C "$HERE/.." describe --tags --always --dirty 2>/dev/null)
binver=$(s "$hip" "nox version" 10)
log "measuring on $holder/$other, nox $binver, repo $commit, N=$N"

median() { sort -n | awk '{a[NR]=$1} END{print (NR%2 ? a[(NR+1)/2] : (a[NR/2]+a[NR/2+1])/2)}'; }
ms() { date +%s%3N; }

# 1. activation: create a scratch volume on the idle node, activate/deactivate N times
s "$oip" "nox create vm-7790-measure-0 1G" 60 >/dev/null || die "create failed"
act=(); for i in $(seq 1 "$N"); do t0=$(ms); s "$oip" "nox activate vm-7790-measure-0" 60 >/dev/null; act+=($(( $(ms) - t0 ))); s "$oip" "nox deactivate vm-7790-measure-0" 60 >/dev/null; done
act_med=$(printf '%s\n' "${act[@]}" | median)
# subtract ssh round trip
rtt=(); for i in 1 2 3; do t0=$(ms); s "$oip" true 10; rtt+=($(( $(ms) - t0 ))); done; rtt_med=$(printf '%s\n' "${rtt[@]}" | median)
act_net=$(( act_med - rtt_med ))

# 2. refusal: activate on the idle node a volume the guest's node holds
ref=(); msg=""
for i in $(seq 1 "$N"); do t0=$(ms); out=$(s "$oip" "nox activate vm-$VMID-disk-0 --timeout 1" 60); ref+=($(( $(ms) - t0 ))); msg=$out; done
ref_med=$(( $(printf '%s\n' "${ref[@]}" | median) - rtt_med ))

# 3. online resize of the scratch volume while active
s "$oip" "nox activate vm-7790-measure-0" 60 >/dev/null
rs=(); sz=1; for i in $(seq 1 "$N"); do sz=$((sz+1)); t0=$(ms); s "$oip" "nox resize vm-7790-measure-0 ${sz}G" 60 >/dev/null; rs+=($(( $(ms) - t0 ))); done
rs_med=$(( $(printf '%s\n' "${rs[@]}" | median) - rtt_med ))
s "$oip" "nox deactivate vm-7790-measure-0; nox delete vm-7790-measure-0" 60 >/dev/null

# 4. live migration round trips
wall=(); down=(); src=$holder; dst=$other
for i in $(seq 1 "$N"); do
    t0=$(date +%s); out=$(s "$(node_ip "$src")" "qm migrate $VMID $dst --online" 900); wall+=($(( $(date +%s) - t0 )))
    d=$(echo "$out" | sed -n 's/.*downtime \([0-9]*\) *ms.*/\1/p' | tail -1); down+=(${d:-NA})
    tmp=$src; src=$dst; dst=$tmp; sleep 3
done
wall_med=$(printf '%s\n' "${wall[@]}" | median); down_med=$(printf '%s\n' "${down[@]}" | grep -v NA | median)
down_min=$(printf '%s\n' "${down[@]}" | grep -v NA | sort -n | head -1); down_max=$(printf '%s\n' "${down[@]}" | grep -v NA | sort -n | tail -1)

# 5. chaos results, if any
chaos() { grep -P "\t$1\tPASS\t" "$RESULTS" 2>/dev/null | tail -1 | cut -f4; }
hr=$(chaos hard-reset); sf=$(chaos sanlock-freeze)

cat <<TABLE

Measured $(date -I) on $holder/$other with nox $binver (repo $commit), N=$N, medians.

| Operation | Result |
|---|---|
| volume activation (lease + dm table) | ${act_net} ms |
| refusal of a volume held elsewhere | ${ref_med} ms, message: $(echo "$msg" | tail -1 | cut -c1-80) |
| online resize +1 GiB | ${rs_med} ms |
| live migration wall time | ${wall_med} s |
| live migration downtime | ${down_min} to ${down_max} ms (median ${down_med}) |
| hard reset of holder to survivor acquiring | ${hr:-run test/chaos/run.sh hard-reset} |
| sanlock frozen to watchdog reset | ${sf:-run test/chaos/run.sh sanlock-freeze} |
TABLE

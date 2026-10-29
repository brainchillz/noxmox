#!/bin/bash
# Two-node SCSI-3 persistent-reservation enforcement self-test. Proves the
# target actually enforces PR before anyone trusts it as a fence layer:
# both nodes register and write a shared LUN, one node preempts the other's
# key, and the preempted node's writes must then be rejected by the array
# while the other keeps writing. Pass => PR is usable; fail => fall back to
# the node-local fence. Writes only to the given scratch device, never a
# live pool. Usage: test/chaos/pr-selftest.sh [by-path-substring]
set -u
HERE=$(cd "$(dirname "$0")" && pwd)
. "$HERE/lib.sh"
MATCH="${1:-noxpr}"

nodes=($(node_names)); [ ${#nodes[@]} -ge 2 ] || die "need two nodes"
A=$(node_ip "${nodes[0]}"); B=$(node_ip "${nodes[1]}")
hida=1; hidb=2
dev_on() { s "$1" "ls /dev/disk/by-path/ 2>/dev/null | grep '$MATCH' | head -1"; }
da=$(dev_on "$A"); db=$(dev_on "$B")
[ -n "$da" ] && [ -n "$db" ] || die "scratch device matching '$MATCH' not found on both nodes"
DA="/dev/disk/by-path/$da"; DB="/dev/disk/by-path/$db"
log "PR self-test: ${nodes[0]} $DA / ${nodes[1]} $DB"

wr() { s "$1" "dd if=/dev/zero of=$2 bs=4096 count=1 oflag=direct 2>/dev/null && echo OK || echo REJECTED" 20; }
verdict=PASS; reason=""

s "$A" "nox pr --device $DA clear" 20 >/dev/null 2>&1
s "$A" "nox pr --device $DA --host-id $hida reserve" 20 >/dev/null || { verdict=FAIL; reason+="A-reserve "; }
s "$B" "nox pr --device $DB --host-id $hidb register" 20 >/dev/null || { verdict=FAIL; reason+="B-register "; }
keys=$(s "$A" "nox pr --device $DA keys" 20 | grep -c 0x)
[ "$keys" = 2 ] || { verdict=FAIL; reason+="keys=$keys "; }
log "both registered ($keys keys)"

[ "$(wr "$A" "$DA")" = OK ] || { verdict=FAIL; reason+="A-write-blocked "; }
[ "$(wr "$B" "$DB")" = OK ] || { verdict=FAIL; reason+="B-write-blocked "; }
log "both wrote while registered"

s "$A" "nox pr --device $DA --host-id $hida --victim $hidb preempt" 20 >/dev/null || { verdict=FAIL; reason+="preempt "; }
aw=$(wr "$A" "$DA"); bw=$(wr "$B" "$DB")
log "after preempt of host $hidb: A=$aw B=$bw"
[ "$aw" = OK ] || { verdict=FAIL; reason+="A-write-after=$aw "; }
[ "$bw" = REJECTED ] || { verdict=FAIL; reason+="B-not-fenced=$bw "; }

s "$A" "nox pr --device $DA clear" 20 >/dev/null 2>&1
mkdir -p "$HERE/logs"
printf '%s\tpr-selftest\t%s\t%s\n' "$(date -Is)" "$verdict" "$reason" >> "$HERE/logs/results.tsv"
log "RESULT pr-selftest: $verdict ${reason:-(enforced)}"
[ "$verdict" = PASS ]

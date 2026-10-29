# Shared helpers for the chaos scenarios. Sourced by run.sh.
# Everything runs from a workstation over ssh as root on the nodes.

set -u
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
. "${LAB_ENV:-$HERE/lab.env}"
NOXD_UNIT="noxd@${POOL:-nox}"

ts() { date +%H:%M:%S; }
log() { echo "[$(ts)] $*"; }
die() { log "FATAL: $*"; exit 2; }

# s IP CMD [TIMEOUT]: run a command on a node.
s() {
    local ip=$1 cmd=$2 to=${3:-90}
    timeout "$to" ssh -o BatchMode=yes -o ConnectTimeout=5 -o StrictHostKeyChecking=accept-new "root@$ip" "$cmd" 2>&1
}

node_ip() { local n; for n in $NODES; do [ "${n%%=*}" = "$1" ] && { echo "${n#*=}"; return; }; done; die "unknown node $1"; }
node_names() { local n; for n in $NODES; do echo "${n%%=*}"; done; }
other_node() { local n; for n in $(node_names); do [ "$n" != "$1" ] && { echo "$n"; return; }; done; }

# vm_node: name of the node where the guest currently runs, empty if stopped.
vm_node() {
    local n
    for n in $(node_names); do
        if s "$(node_ip "$n")" "qm status $VMID 2>/dev/null" 15 | grep -q running; then echo "$n"; return; fi
    done
}

# wait_until SECONDS DESC CMD...: poll CMD every 2 s until it succeeds.
wait_until() {
    local to=$1 desc=$2; shift 2
    local t0=$(date +%s)
    while :; do
        if "$@" >/dev/null 2>&1; then echo $(( $(date +%s) - t0 )); return 0; fi
        if [ $(( $(date +%s) - t0 )) -ge "$to" ]; then log "timeout after ${to}s waiting for $desc"; return 1; fi
        sleep 2
    done
}
host_up() { ping -c1 -W1 "$1" >/dev/null 2>&1; }
host_down() { ! host_up "$1"; }
ssh_up() { s "$1" true 10 >/dev/null 2>&1; }

crash_node() { # sysrq-b: immediate reset, no sync, no shutdown
    log "crashing $1 (sysrq b)"
    s "$1" "echo 1 > /proc/sys/kernel/sysrq; (sleep 1; echo b > /proc/sysrq-trigger) >/dev/null 2>&1 &" 15 || true
}

# In a two node cluster the survivor loses quorum; this restores it once
# corosync has noticed the loss. Harmless with three or more nodes.
recover_quorum() {
    local ip=$1
    wait_until 60 "quorum state" sh -c "ssh -o BatchMode=yes root@$ip 'pvecm status 2>/dev/null' | grep -qE 'Quorate: *(Yes|No)'" >/dev/null || true
    if ! s "$ip" "pvecm status" 20 | grep -q 'Quorate: *Yes'; then
        s "$ip" "pvecm expected 1" 20 >/dev/null || true
        wait_until 60 "quorum" sh -c "ssh -o BatchMode=yes root@$ip 'pvecm status 2>/dev/null' | grep -q 'Quorate: *Yes'" >/dev/null
    fi
}

move_config() { # move_config SURVIVOR_IP FROM_NODE TO_NODE
    s "$1" "test -e /etc/pve/nodes/$2/qemu-server/$VMID.conf && mv /etc/pve/nodes/$2/qemu-server/$VMID.conf /etc/pve/nodes/$3/qemu-server/$VMID.conf; echo moved" 30
}

wait_node_back() { # wait_node_back IP: ssh up, then noxd ready
    local ip=$1
    log "waiting for $ip to come back"
    wait_until 600 "ssh on $ip" ssh_up "$ip" >/dev/null || return 1
    wait_until 180 "noxd ready on $ip" sh -c "ssh -o BatchMode=yes root@$ip 'nox status 2>/dev/null' | grep -q 'ready true'" >/dev/null
}

agent_exec() { # agent_exec IP CMD...: run in the guest via the agent, print stdout
    local ip=$1; shift
    s "$ip" "qm guest exec $VMID --timeout 60 -- $*" 90 | python3 -c 'import json,sys
try:
    d=json.load(sys.stdin); sys.stdout.write(d.get("out-data","")); sys.exit(d.get("exitcode",1))
except Exception as e:
    print("agent:", e); sys.exit(1)'
}
agent_wait() { wait_until 300 "guest agent" s "$1" "qm guest exec $VMID -- true" 20; }

nox_check() { # nox_check IP: table and lease consistency, prints problems
    local out
    out=$(s "$1" "nox check --full" 300) || { echo "$out" | grep -E 'PROBLEM|error' ; return 1; }
    return 0
}

# ---- verifier: a guest-side writer and a host-side checker ----------------

verifier_setup() { # verifier_setup IP: make sure the guest has the verifier disk
    local ip=$1
    if ! s "$ip" "qm config $VMID" 20 | grep -q '^scsi1:'; then
        log "adding verifier disk scsi1 (${VERIFIER_GB}G) to guest $VMID"
        s "$ip" "qm set $VMID --scsi1 $STORAGE:$VERIFIER_GB,ssd=1" 120 >/dev/null || die "cannot add verifier disk"
        sleep 3
    fi
    VERIFIER_VOL=$(s "$ip" "qm config $VMID" 20 | sed -n 's/^scsi1: *[^:]*:\([^,]*\).*/\1/p')
    [ -n "$VERIFIER_VOL" ] || die "cannot read verifier volume name"
    # push the writer
    local b64; b64=$(base64 -w0 "$HERE/writer.py")
    agent_exec "$ip" "sh -c 'echo $b64 | base64 -d > /root/writer.py; chmod +x /root/writer.py'" >/dev/null || die "cannot install writer in guest"
}

verifier_start() { # verifier_start IP RUNID: start the writer in the guest
    agent_exec "$1" "sh -c 'kill \$(cat /root/writer.pid 2>/dev/null) 2>/dev/null; sleep 1; nohup python3 /root/writer.py /dev/sdb $2 > /root/writer.log 2>&1 & echo \$! > /root/writer.pid'" >/dev/null || die "cannot start writer"
}
verifier_stop() { agent_exec "$1" "sh -c 'kill -INT \$(cat /root/writer.pid) 2>/dev/null; sleep 1; tail -1 /root/writer.log'"; }

# verifier_check IP RUNID TIMEOUT: activate the verifier volume on IP
# exclusively (waiting for a dead holder's lease to expire), check the data,
# deactivate. Prints the checker's summary; returns its exit status.
verifier_check() {
    local ip=$1 run=$2 to=${3:-240} t0 t1 rc
    local b64; b64=$(base64 -w0 "$HERE/verify.py")
    s "$ip" "echo $b64 | base64 -d > /tmp/verify.py" 20 >/dev/null
    t0=$(date +%s)
    s "$ip" "nox activate $VERIFIER_VOL --timeout $to" $((to+30)) >/dev/null || { log "activate $VERIFIER_VOL on $ip failed"; return 1; }
    t1=$(date +%s)
    log "verifier volume acquired on $ip after $((t1-t0))s"
    ACQUIRE_SECS=$((t1-t0))
    s "$ip" "python3 /tmp/verify.py /dev/mapper/nox-$VERIFIER_VOL $run" 300; rc=$?
    s "$ip" "nox deactivate $VERIFIER_VOL" 60 >/dev/null
    return $rc
}

# ---- results ----------------------------------------------------------------
RESULTS=${RESULTS:-$HERE/logs/results.tsv}
result() { # result NAME PASS|FAIL "numbers"
    mkdir -p "$(dirname "$RESULTS")"
    printf '%s\t%s\t%s\t%s\n' "$(date -Is)" "$1" "$2" "$3" >> "$RESULTS"
    log "RESULT $1: $2 ($3)"
}

# Failure modes tested

What was deliberately broken, how it was broken, and what had to hold
afterwards. Everything here is reproducible from the scripts under `test/`;
the lab, the pass criteria and the latest measured results are all in the
repository, not in anyone's memory.

Lab: two HP EliteDesk 800 G6 minis (pve-01, pve-02) on Proxmox VE 9.2 and
1 GbE, sharing a 1 TB LIO fileio iSCSI LUN at io_timeout 10. wdmd runs on
softdog because the iTCO watchdog on these boxes never fires. The chaos
suite expects to be the only user of the lab and will crash nodes.

Three layers of testing, each catching a different class of failure:

| Layer | Where | Catches |
|---|---|---|
| Unit, property and fuzz tests | `internal/store` (`go test`) | corrupt or hostile on-disk bytes, allocator bugs, a damaged superblock or record copy, v1 to v2 upgrade |
| Crash replay | `test/replay` | a torn write or power loss at any point during any metadata mutation |
| Chaos suite | `test/chaos` | real nodes dying, daemons dying, storage paths vanishing, races between nodes, clock skew |

## How the chaos suite measures safety

Every scenario that takes a node down while a guest is writing uses the
same verifier. A writer inside the test guest (`test/chaos/writer.py`)
writes sequence numbered 4 KiB blocks to a dedicated raw disk, fsyncing
after every block. Each block carries a run id, its sequence number and a
CRC. After the failure, the surviving node activates that disk exclusively
(which can only succeed once the dead holder's lease has expired) and runs
`test/chaos/verify.py`, which requires:

- every acknowledged block forms a contiguous valid prefix,
- at most one block after the prefix is torn (the one in flight),
- nothing from this run exists beyond it.

On top of the data check, the crash scenarios also assert that the survivor
acquired only after the holder was off the network (no acquire before the
fence), that the holder comes back with no stale device-mapper devices, and
that `nox check --full` finds no table or lease problems. Each scenario
appends one line with its measured times to `test/chaos/logs/results.tsv`.

Run with `test/chaos/run.sh` (default set), `test/chaos/run.sh all` (adds the
scenario that takes the whole lab down), or `test/chaos/run.sh <name>`.

## Node and daemon failures

### Holder hard reset mid-write (`hard-reset`)

- **Failure:** the node running the guest dies instantly with no shutdown.
- **How:** `echo b > /proc/sysrq-trigger` on the holder while the verifier writes.
- **Must hold:** holder off the network within 30 s; survivor acquires the
  verifier volume within 150 s and only after the holder is gone; data
  consistent to the last fsync; guest restarts on the survivor; holder
  returns with no stale dm devices.
- **Measured:** reset 2 to 3 s, survivor acquires at 77 to 81 s, guest
  answering about 135 s after start. A host that rebooted and rejoined in
  55 s freed its old leases at 56 s via the new lockspace generation.

### sanlock frozen on the holder (`sanlock-freeze`)

- **Failure:** sanlock stops renewing but the node is otherwise alive, so
  nothing stops the guest writing unless the watchdog does.
- **How:** `pkill -STOP -x sanlock` on the holder while the verifier writes.
- **Must hold:** wdmd resets the node before any peer can acquire (limit
  145 s to reset, 200 s to acquire); data check and stale-dm check as above.
- **Measured:** watchdog reset at 130 to 138 s, survivor acquires at 151 to
  152 s. One run at 131 s with a tighter limit was the reason the reset
  limit is 145 s.

### noxd SIGKILLed under a running guest (`noxd-kill`)

- **Failure:** the lease-holding daemon dies but the guest's QEMU keeps the
  disks open.
- **How:** `systemctl kill -s KILL noxd@nox` on the holder while the
  verifier writes.
- **Must hold:** the guest never notices (writer still making progress, VM
  still running); the restarted daemon reports ready and adopts every
  orphan lease (count of held leases unchanged, journal shows one
  "adopted orphan lease" per lease).
- **Measured:** ready in 4 s, 3 of 3 leases adopted. One run reported zero
  adoptions with the daemon "ready" in 0 s; the script was killing a stale
  unit name and never touched the daemon. Fixed in the suite, rerun passed.

### noxd SIGKILLed mid-create (`noxd-kill-create`)

- **Failure:** the daemon dies at a random point during a metadata write.
- **How:** 25 rounds of `nox create` with a SIGKILL to the daemon after a
  random 0 to 0.9 s delay, on the idle node.
- **Must hold:** after each round the daemon comes back and `nox check` is
  clean; every volume whose create reported success is present (a create
  committed on disk just before the kill may be present without having
  reported, which is allowed); all volumes delete cleanly afterwards.
- **Measured:** 20 to 22 of 25 creates completed, the rest failed cleanly,
  no half record ever visible.

## Storage path failures

### iSCSI path lost on the holder only (`path-loss-holder`)

- **Failure:** the holder's node can no longer reach the LUN but sanlock,
  noxd and the guest are all alive and the guest keeps trying to write.
- **How:** iptables DROP of TCP port 3260 to and from the target IP on the
  holder, while the verifier writes.
- **Must hold:** sanlock loses the lockspace and runs the killpath
  (`nox fence`), which SIGKILLs every process holding a pool volume and
  stops noxd. Two acceptable outcomes: the guest is killed and the host
  stays up (gentle path), or a guest wedged in dead-storage I/O cannot be
  reaped and the watchdog resets the host (backstop). A live guest on a
  still-up holder is a failure. Survivor acquires within 360 s, data check
  passes, guest restarts on the survivor, pool consistent.
- **Measured:** both outcomes seen. Guest killed with the host up: survivor
  acquires at 139 to 164 s (io_timeout 10) and 224 s (io_timeout 20).
  Watchdog reset: acquire at 135 s. The first run of this scenario found a
  survivor acquiring while the holder's guest was still alive; that was
  the storage-loss fencing bug fixed by adding the killpath.
- **Repair:** this LIO fileio target does not re-present its LUN after a
  portal flap until a fresh login, so the script logs out and in again and
  reboots the node if the LUN is still missing. Documented in the README.

### iSCSI target gone for everyone (`path-loss-both`, in the `all` set)

- **Failure:** the shared LUN disappears from every node at once.
- **How:** `targetcli /iscsi/<iqn>/tpg1 disable` on the storage server over
  ssh, while the verifier writes; re-enabled after the checks.
- **Must hold:** every node that held a volume lease is fenced within 360 s
  (guests killed, or watchdog reset); a node holding nothing need not
  fence; after the target returns no node has stale dm devices, the pool
  is consistent, the verifier data is intact, and the guest starts again.
- **Measured:** holder's guest killed at 68 s, idle node untouched, data
  intact. Four earlier runs failed on the fence never firing and on nodes
  not re-presenting the LUN after reboot; those drove the killpath work and
  the re-login guidance.

### Source loses its storage path during live migration (`migrate-storage-loss`)

- **Failure:** the one legitimate window where two hosts share a writable
  volume (both hold shared during the memory copy) is hit by a storage
  loss on the source.
- **How:** online migration throttled to 5 MiB/s so the kill lands mid-copy,
  then iptables DROP of iSCSI traffic on the source once the memory copy
  is active.
- **Must hold:** the source is fenced (its guest killed or host reset);
  at most one QEMU for the guest is alive across both nodes; pool consistent;
  verifier data intact on a healthy node. The guest being restartable
  afterwards is a warning only, because an abnormally aborted migration
  can leave stuck shared orphan leases (availability, not safety).
- **Measured:** source fenced with host up, zero live QEMU on either node,
  data intact, three passes. One pass carried the stuck-orphans warning;
  the reaper and healthcheck fixes of 2026-10-29 address that.

## Lease arbitration and races between nodes

### Two nodes outside any cluster (`standalone-refusal`)

- **Failure mode guarded against:** two hosts not in the same corosync
  domain, pointed at the same LUN, both try to own one volume. Quorum
  cannot arbitrate this; only the on-disk lease can.
- **How:** driven purely through the nox CLI, no qm and no cluster. Node A
  activates a volume exclusive; node B tries the same with a 5 s timeout;
  A releases; B tries again.
- **Must hold:** B is refused, the error names A's host id, and B acquires
  after A releases. Pool consistent after cleanup.
- **Measured:** pass, refusal names host 1. Refusal latency is under a
  second (from `test/measure.sh`).

### Delete against activate (`delete-vs-activate`)

- **Failure:** one node deletes a volume at the same moment the other
  activates it.
- **How:** 10 rounds; `nox delete` on node A and `nox activate --timeout 5`
  on node B launched concurrently against a fresh volume.
- **Must hold:** exactly one wins each round; the loser gets a clear
  "not found" or "in use" error; `nox check` clean.
- **Measured:** mixed wins both ways (for example delete 7, activate 3).
  The first run saw both sides succeed in four of ten rounds, a real race
  in the daemon, fixed in the format v2 commit before the suite went green.

### Concurrent create and delete from both nodes (`concurrent-create-delete`)

- **Failure:** metadata writes from two nodes interleave, including fights
  over the same name.
- **How:** 200 iterations per node in parallel, each iteration creating a
  private volume, racing for a shared name, and deleting its private one.
- **Must hold:** each contested name is won exactly once and exists
  exactly once; every private create succeeds; no duplicate or overlapping
  records; nothing left after cleanup.
- **Measured:** won 200 of 200, lost 200, about 5 ops/s end to end. The
  first run saw 399 wins for 200 names, meaning duplicate records, the
  second real race fixed in the format v2 commit.

### Live migration aborted mid-copy (`migrate-abort`)

- **Failure:** the target QEMU dies during the memory copy, while both
  sides hold the volumes shared.
- **How:** throttled online migration, then SIGKILL of the target's kvm
  process once the copy is active.
- **Must hold:** guest keeps running on the source with its agent
  answering; the source converts back to exclusive within 150 s; the target
  holds no leases and has no dm devices; pool consistent.
- **Measured:** source exclusive again after 1 to 79 s, target clean.

### Clock skew (`clock-skew`)

- **Failure:** one node's wall clock is wrong by five minutes.
- **How:** stop chrony and `date -s '+5 minutes'` on the target, then
  migrate the guest there and back.
- **Must hold:** hosts still shown live, both migrations succeed, the
  volumes end exclusive on the final node and the other node holds
  nothing, pool consistent. sanlock and the daemon use monotonic time, so
  skew must have no effect.
- **Measured:** 9 s each way, no effect.

## Fence layers proven separately

### SCSI-3 persistent reservations (`test/chaos/pr-selftest.sh`)

- **Failure mode guarded against:** trusting an array to enforce PR when it
  silently does not.
- **How:** both nodes register keys on a scratch LUN and write to it; node A
  preempts node B's key; both write again.
- **Must hold:** both write while registered; after preemption A still
  writes and B's write is rejected by the array.
- **Result:** pass on a zvol-backed scratch LUN on the lab target. The
  pool's own fileio LUN does not enforce PR, which is exactly what this
  test exists to detect. The PR layer is integrated into the daemon and
  off by default: `nox pr-enable` and `nox pr-reserve` turn it on, every
  node then registers its key on start, and a survivor taking over from a
  dead host preempts that host's key. The chaos scenarios above all ran
  with PR off; the node-local killpath and watchdog are the fence they
  exercised.

### Watchdog hardware

The iTCO timer on the EliteDesk 800 G6 was opened and left unpetted for
four minutes; the machine never reset. wdmd therefore uses softdog through
a udev symlink. That is sufficient for the chaos suite because the kernel
stays alive in every scenario here, but it is not a production watchdog.

## On-disk format: crash and corruption

### Crash replay (`test/replay`)

- **Failure:** power loss or crash at any point during format, create,
  grow or delete, including a write that lands only partially.
- **How:** a fixed sequence of 13 operations is run against a file-backed
  pool with every write recorded. The device is then rebuilt one write at a
  time and, inside each multi-sector write, one sector at a time. After
  every step the pool must open, pass `Check`, and its table must equal
  either the state before or the state after the operation in progress,
  never anything else. Two variants: the store's own write recorder (runs
  in CI without root) and dm-log-writes at the block layer (needs root and
  `dmsetup`).

### Store unit, property and fuzz tests (`internal/store`)

- **Damaged superblock:** corrupt the primary and the pool must open from
  the mirror; repair the primary, corrupt the mirror, and it must open from
  the primary (`TestSuperMirror`).
- **Damaged record copy:** overwrite one of a record's two copies with
  garbage; check must pass on the surviving copy and `RepairCopies` must
  restore the bad one (`TestRepairCopies`).
- **Allocator invariants:** 2000 random iterations where runs never overlap,
  never leave the device, and cover the request exactly; plus a churn test
  that checks the table against an independent model after every mutation
  and after a fresh re-read (`TestAllocateProperties`, `TestPoolChurn`).
- **Hostile bytes:** fuzz targets for the superblock, volume record, whole
  table and mailbox decoders. Garbage may be rejected, never cause a panic
  (`FuzzDecodeSuper`, `FuzzDecodeVolume`, `FuzzCheckTable`,
  `FuzzDecodeMailbox`).
- **Upgrade:** a v1 pool opens unchanged, old records keep working, new
  records carry v2 fields, and the mailbox lands in the gap before the data
  (`TestUpgradeV1`).

CI runs `go vet`, the store tests, the recorded replay variant, and a Perl
syntax check of the PVE plugin against stubs.

## Timeout interaction measured

The holder's self-fence on path loss and the survivor's dead-host
detection do not scale together. Survivor acquires the holder's volume
after:

| Failure mode | io_timeout 10 | io_timeout 20 |
|---|---|---|
| Hard reset (holder gone at once) | ~77 s | ~77 s |
| Storage path lost (holder self-fences) | ~150 s | ~224 s |

The iSCSI `replacement_timeout` plus the SCSI command timeout must be
shorter than 8 times io_timeout, or a path blip fences the node. The
quickstart sets it and the package warns if it is too high.

## Not yet tested

- Multipath, and the `no_path_retry` interaction with sanlock's window.
- Clusters of three or more nodes (the suite restores quorum by hand with
  `pvecm expected 1`, which a third node or QDevice would make unnecessary).
- A real hardware watchdog or IPMI fencing; the lab uses softdog.
- Network partition between the nodes with storage intact (corosync loss
  without storage loss). The standalone-refusal scenario shows the lease
  does not depend on corosync, but a partition of a running cluster has
  not been driven as a scenario.
- Storage path loss on a node that holds only shared (read-only backing)
  leases.
- The chaos scenarios with PR enabled on an enforcing LUN. Only the
  two-node enforcement self-test has run.

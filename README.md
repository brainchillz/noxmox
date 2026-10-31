# nox

[![ci](https://github.com/brainchillz/noxmox/actions/workflows/ci.yml/badge.svg)](https://github.com/brainchillz/noxmox/actions/workflows/ci.yml)

**Shared iSCSI storage for Proxmox that works the way you expected it to.**

Ever pointed a Proxmox cluster at an iSCSI LUN and found it tedious to manage?
You get LVM on top of the LUN, no snapshots, no thin provisioning, and a nagging
feeling that nothing is actually stopping two nodes from writing to the same
disk at once. The alternatives are a clustered filesystem with its own DLM and
fencing stack to babysit, or giving up and using NFS.

If you came from VMware, you remember it being easier. One LUN, every host sees
it, VMs live on it, vMotion just works. That is VMFS, and Linux never had an
equivalent.

nox is that equivalent. Format one iSCSI or Fibre Channel LUN once, every node
that can see it opens it read-write, and each VM disk becomes a plain block
device over fixed extents on the LUN. Ownership is decided by sanlock leases
stored on the LUN itself, so a node that stops renewing gets fenced and its
disks become available to the survivors, with no corosync, DLM, kernel module,
or clustered filesystem involved. Proxmox sees it as an ordinary shared storage
type: create, resize, clone, and live migrate from the UI.

**The lock lives on the disk, not in the cluster.** Proxmox protects shared LVM
with a lock held in its cluster filesystem, which means the protection only
exists between nodes that are in the same cluster. Point two standalone nodes
at the same LVM LUN and nothing stops them from starting the same VM twice and
shredding its disk. With nox the lease is on the LUN, so the same pool can be
shared safely by nodes in one cluster, in different clusters, or in no cluster
at all. Two independent Proxmox boxes and one SAN LUN is a supported layout.

## What it does today

Status: working prototype, tested on a two-node Proxmox VE 9.2 cluster against
a 1 TB iSCSI LUN. Every claim below is exercised by the test suite in
`test/`; the numbers come from `test/measure.sh` and the chaos suite on that
lab (HP EliteDesk 800 G6 minis, 10 GbE, LIO target on a file).

- Volumes are device-mapper linear maps over fixed extents (256 MiB by
  default, 16 MiB minimum). Activation takes about 100 ms. Resize is online
  and instant.
- Volumes are raw or qcow2. qcow2 volumes get snapshots the way PVE 9 does
  them on LVM with snapshot-as-volume-chain: a snapshot is a sibling volume,
  taken and deleted while the guest runs, rolled back while it is stopped.
  Templates and linked clones work; a base image is held shared by every
  node that runs a clone of it.
- Containers work: the rootfs is a volume with a filesystem inside, like on
  LVM. Online resize and offline migration included.
- Live migration through the Proxmox UI works in both directions, with a
  snapshot chain too. Handoff through the on-disk mailbox takes about a
  second; measured wall time 7 to 12 s for the 2 GiB test guest, downtime
  under 200 ms.
- A second node that tries to start the same VM is refused in under a second
  and told which host holds the disk.
- Hard-reset a node mid-write and the survivor can take its disks about two
  minutes later, with every block the guest got an fsync for intact. A node
  whose lease renewals stall is reset by the watchdog before anyone else can
  touch its volumes.
- The daemon can be restarted under a running VM. Leases survive as sanlock
  orphans and are re-adopted in a few seconds.
- The on-disk format is crash safe: a replay suite rebuilds the pool one
  write and one torn sector at a time and checks it at every step.
- `nox check` audits the table and the lease holders; `nox repair` fixes
  what is provably safe. `nox metrics` exports Prometheus text.
- One node can serve several pools, each a daemon instance with its own
  socket and device-mapper prefix.

Not built: thin provisioning (see below), hardware watchdog or IPMI fencing
integration beyond what sanlock and wdmd give you, and the Incus driver.

## Reproducing the numbers

Every figure above comes from a script on the lab, not a guess:

- Activation, refusal, resize and live-migration numbers: `test/measure.sh`
  (medians over N runs; prints the commit and host it measured).
- Dead-host takeover, watchdog-reset and the fence outcomes:
  `test/chaos/run.sh`, which writes one line per scenario with its measured
  times to `test/chaos/logs/results.tsv`; `test/measure.sh` folds the latest
  of those into its table.
- Crash safety and the allocator numbers: `go test ./internal/store/ ./test/replay/`.

The quoted values are from the two-node HP EliteDesk 800 G6 lab against a
1 TB LIO fileio LUN at io_timeout 10; re-run the scripts on your hardware
for yours.

## Why not just use ...

| Option | What you give up |
|---|---|
| LVM on iSCSI (what Proxmox offers) | the lock lives in the cluster filesystem so standalone nodes get no protection at all, no fencing of a wedged node, no linked clones |
| OCFS2 / GFS2 | a kernel filesystem, a DLM, corosync coupling, and a fencing stack to tune |
| NFS or Ceph | a different storage protocol; your existing SAN stays idle |
| nox | a Go daemon, a Perl plugin, and sanlock. Nothing in kernel space but device-mapper |

## Pieces

| Path | What |
|---|---|
| `cmd/nox` | single binary: `nox daemon` (noxd), the CLI, and `nox fence` (the killpath) |
| `internal/store` | on-disk format v1 and v2, O_DIRECT device I/O, allocator, mailbox, checker, dm tables |
| `internal/sanlock` | cgo binding over libsanlock (leases, orphans, host events) |
| `internal/daemon` | noxd: lockspace join, lease holding, chain activation, migration handoff, socket API |
| `internal/proto` | JSON-over-Unix-socket protocol shared by CLI and plugin |
| `pve/NoxPlugin.pm` | `PVE::Storage::Custom::NoxPlugin`, storage type `nox` |
| `deploy/` | `noxd@.service`, udev rule, watchdog order unit, `install.sh`, `build-deb.sh` |
| `test/` | fuzz and property tests, crash replay, chaos suite, measurements ([test/README.md](test/README.md)) |

## On-disk layout (format v2)

| Offset | Region |
|---|---|
| 0 | superblock (4 KiB), mirrored in the last 4 KiB of the LUN |
| 1 MiB | sanlock lockspace (delta leases, host ids 1..2000) |
| 2 MiB | metadata lease (paxos) |
| 3 MiB | one paxos lease per volume slot, 1 MiB each (1024 slots by default) |
| after leases | volume table: 2 x 4 KiB record per slot, CRC32C + generation, A/B copies |
| after table | mailbox lease (1 MiB), then one 4 KiB mailbox block per host id |
| next extent boundary | data extents |

There is no journal. A record is one aligned 4 KiB block; writers alternate
copies and readers take the highest valid generation, so a torn write can
only lose the write in flight, never the previous version. The allocator
bitmap is derived from the table on every mutation, under the metadata lease.
A record carries its backing volume's slot, its format, the guest visible
size of a qcow2 image and the host that created it.

The superblock records the format version, feature bits, the sanlock
io_timeout every host must use, and the host id range. A pool formatted by
the first release (v1) is read and written unchanged; `nox upgrade` moves it
to v2 in place while every volume is inactive, placing the mailbox in the
gap before the first data extent. Running daemons pick the new superblock up
on their own.

## Lease model

- A running guest's volume is held **exclusive** by noxd on that node, as a
  sanlock orphan lease (`SANLK_RES_PERSISTENT`), so noxd can restart without
  the guest noticing and re-adopts its leases on start.
- Every volume beneath a qcow2 top (snapshots, a template's base image) is
  held **shared** by whoever has the chain open. Backing volumes are read
  only for everyone, so any number of nodes may hold one.
- Live migration is the only dual-open window of a writable volume. The
  target asks for a shared lease; if the source holds exclusive, the target
  posts a request into the source's mailbox block on the LUN and the source
  converts within a second. Both hold shared during the memory copy. When
  the source deactivates, the target's upgrader loop converts back to
  exclusive within two seconds. Pools without a mailbox (v1) use a sanlock
  host event instead, which costs one delta lease renewal.
- A dead host's leases expire through sanlock's delta lease; measured 125 s
  from hard reset to the survivor acquiring on this hardware (io_timeout 10)
  when the host stays down. A host that reboots and rejoins sooner gets a
  new lockspace generation, and sanlock frees the leases of its previous
  life at once: the chaos suite measured 56 s with a 55 s reboot.
- Fencing is reliable when the node itself stops: a hard crash, or sanlock
  unable to run (frozen or dead), means it cannot pet the watchdog and wdmd
  resets it 123 s after the last renewal, before any peer acquires. Both
  are verified in the chaos suite.

- Losing only the storage path, while sanlock keeps running, is fenced
  through a sanlock killpath, the same mechanism libvirt uses. noxd holds
  the leases, but the data path is the guests' QEMU, so killing noxd alone
  would let sanlock call the host clean while a guest kept writing. Instead,
  when sanlock loses the lockspace it runs `nox fence`, which SIGKILLs every
  local process holding one of the pool's volumes and then stops noxd. While
  a guest still holds a volume the fence leaves noxd alive and the watchdog
  armed, so a guest that is wedged in dead-storage I/O and cannot be reaped
  takes the whole host down by watchdog instead. Either way the guest is
  gone before a survivor acquires. The chaos suite exercises both outcomes.
  A shorter iSCSI `replacement_timeout` makes the gentle path (guest killed,
  host stays up) win over the watchdog reset more often.
- Fencing is wdmd on a kernel watchdog device. On the lab's HP EliteDesk
  800 G6 the iTCO timer loads but never resets the machine (tested by
  opening it and not petting it for four minutes), so wdmd uses softdog
  via a udev symlink `/dev/watchdog-soft`, and PVE's watchdog-mux is given
  the iTCO by loading it first. That leaves PVE HA self-fencing nominal on
  those nodes; use real hardware watchdogs or IPMI fencing in production.

### Timeouts that must agree

sanlock gives up on a host `8 * io_timeout` seconds after its last
successful renewal and lets others take its leases about 45 s after that.
The node itself must get an I/O error back from a failed path before that,
or it fences itself for a path blip: the iSCSI `replacement_timeout` plus
the SCSI command timeout must be shorter than `8 * io_timeout`. With the
default io_timeout 10 that is 80 s; the PVE defaults (120 s replacement
timeout) are too long for it and should be lowered to about 60 s, or
io_timeout raised to 20 at format time. The pool records io_timeout so
every host joins with the same value.

Raising io_timeout does not speed recovery; it trades recovery time for
blip tolerance, and the two failure modes do not move together. Measured on
the lab, survivor acquires the holder's volume after:

| Failure mode | io_timeout 10 | io_timeout 20 |
|---|---|---|
| Hard reset (holder gone at once) | ~77 s | ~77 s |
| Storage path lost (holder self-fences) | ~150 s | ~224 s |

A hard crash is found through the survivor's dead-host detection, which
floored near 80 s on this hardware and did not grow with io_timeout. A lost
path is slower because the holder must fail `8 * io_timeout` of renewals
before sanlock runs the fence, so doubling io_timeout adds about that much.
Raising io_timeout therefore buys tolerance for a storage blip shorter than
`8 * io_timeout` (it no longer fences the node) at the cost of slower
recovery from a real path loss, and leaves clean-crash takeover about where
it was. Lower it only on storage that returns an I/O error well inside
`8 * io_timeout`: io_timeout 5 gives a 40 s window, shorter than a single
iSCSI command can take to fail (a 15 s `replacement_timeout` plus the 30 s
SCSI command timeout is up to 45 s), so a brief path flap would fence the
node. io_timeout 10 is the default for that reason; change it only with the
path timeouts measured to match. Use `nox set-io-timeout` to change it in
place: every daemon but one must be stopped and out of the lockspace first,
because all hosts have to rejoin at the new value together.

## Standalone nodes

Nothing in nox talks to corosync or pmxcfs. Each node needs a unique host id
in its pool configuration and its own `pvesm add nox ...` entry, and that is
all. Between nodes that are not clustered:

- A VM whose disk is held by one node cannot be started on another. The second
  start is refused within a second and names the holder.
- Cold moves are a config file copy. Shut down on A, copy the guest config to B,
  start it there. No disk copy; the lease moves with the start.
- If a node dies, its leases expire about two minutes later and any other node
  can start its guests. There is no quorum to reason about.
- Live migration is a cluster feature of Proxmox and is not available. The
  remote-migrate path between separate nodes copies the disk, which works but
  duplicates the volume on the same LUN.

Give each node a disjoint VMID range. Volume names are derived from the VMID.
The plugin refuses to delete a volume that another host created unless a
guest with that VMID on this node references it, so "purge unreferenced
disks" on one node cannot remove the other node's volumes; set
`allow-foreign-delete` on the storage to turn that off inside one cluster.

## Install on each PVE node

From a release package:

    apt install ./nox_<version>_amd64.deb
    install -d /etc/nox
    printf 'NOX_DEVICE=/dev/disk/by-id/scsi-<wwid>\nNOX_HOST_ID=1\n' > /etc/nox/nox.conf   # unique host id per node
    # A lost path must error before sanlock's 8 x io_timeout window (80 s at
    # the default io_timeout 10), or the node fences on a blip. The stock
    # iSCSI replacement timeout is 120 s; lower it. This is node-wide, so if
    # other iSCSI storage shares this host, set it on nox's target records
    # only. See "Timeouts that must agree".
    sed -i 's/^node.session.timeo.replacement_timeout = .*/node.session.timeo.replacement_timeout = 15/' /etc/iscsi/iscsid.conf
    systemctl enable --now wdmd sanlock noxd@nox

The package points wdmd at `/dev/watchdog`; confirm that device actually
resets this hardware (see the watchdog note below), and `deploy/install.sh`
does both of these steps and the watchdog wiring for you.

Or from a checkout, which does the same over ssh:

    ./deploy/install.sh root@pve-01 1 /dev/disk/by-id/scsi-<wwid>
    ./deploy/install.sh root@pve-02 2 /dev/disk/by-id/scsi-<wwid>

Then, once:

    nox format --device /dev/disk/by-id/scsi-<wwid>        # on any node
    pvesm add nox noxpool --content images,rootdir --shared 1

A second pool is a second config file and a second storage with
`--pool NAME`: `/etc/nox/fast.conf`, `systemctl enable --now noxd@fast`,
`pvesm add nox fastpool --pool fast ...`. Its volumes map as `fast-<name>`.

## Persistent reservations (optional, where the target enforces them)

On a target that truly enforces SCSI-3 persistent reservations (a real SAN,
or a zvol/iblock LIO backstore, not a fileio backstore), nox can add PR as a
second fence layer so the array itself rejects a fenced node's writes,
independent of the watchdog. It is off by default and never required.

    nox pr-probe --device DEV      # does this target implement PR at all?
    test/chaos/pr-selftest.sh      # two-node proof that it actually enforces

If both pass, enable it: `nox pr-enable` on each node (sets the pool's PR
bit and registers the node), restart the daemons so every node registers,
confirm `nox pr-status` lists every host key, then `nox pr-reserve` once to
start enforcement. `pr-reserve` refuses while fewer than two hosts are
registered (so turning enforcement on cannot lock out a peer whose daemon
has not joined PR mode yet); pass `--force` for a deliberate single-node
pool. From then on each node registers its key on start and refuses to
start if it cannot, and a survivor taking a volume over from a dead host
preempts that host's key so the array drops its writes.

Set `NOX_METRICS_ADDR` (or `nox daemon --metrics-addr :9965`) to serve the
Prometheus metrics over HTTP at `/metrics` for a scraper, instead of only
`nox metrics` on the CLI.

## CLI

    nox status | list | info NAME | check [--full] | repair | metrics
    nox pr-probe --device DEV | pr-status | pr-enable | pr-reserve
    nox create NAME SIZE [--format qcow2] [--parent NAME] [--virt-size SIZE]
    nox delete NAME | resize NAME SIZE | rename NAME NEWNAME
    nox activate NAME [--shared] [--timeout SEC] | deactivate NAME
    nox format --device DEV [--extent-size 256M] [--slots 1024] [--io-timeout 10]
    nox daemon --device DEV --host-id N [--pool NAME] [--metrics-addr :9965]
    nox upgrade                  # format v1 to v2, with every volume inactive
    nox set-io-timeout SEC       # rewrite the lockspace timeout, other daemons stopped
    nox version

Every command takes `--pool NAME` (default `nox`). `nox fence --pool NAME`
exists too but is the daemon's sanlock killpath, not something you run by
hand; `nox fence --pool NAME --dry-run` reports what it would stop.

## Known limits

- No thin provisioning. A qcow2 volume reserves its fully allocated size,
  as PVE's LVM chains do. A grow-on-demand scheme (allocate small, grow the
  dm table ahead of QEMU's write pointer, pause the guest on ENOSPC if a
  burst wins) is possible and planned after the chain code settles.
- One extent size per pool, chosen at format time. Small volumes such as
  cloud-init drives still cost one extent.
- Deleting a volume requires it to be inactive everywhere; the daemon proves
  that by taking its lease exclusively.
- A two-node cluster needs `pvecm expected 1` or a QDevice before a VM can be
  recovered after a node dies.
- Never restart sanlock on a node with a joined lockspace: wdmd will reset
  the node. Stopping noxd is safe, leases stay as orphans.
- After a node reboots during a full loss of the target, some iSCSI
  targets (LIO fileio especially) do not re-present the LUN until a fresh
  login, so the node can come back with the pool device missing and noxd
  restart-looping. Set the iSCSI nodes to automatic startup and, after such
  an outage, re-login the target (`iscsiadm -m node -T <iqn> --logout` then
  `--login`) or reboot. Enforced-PR and multipath both need the session
  healthy.
- Multipath is untested. The timeout rule above applies to `no_path_retry`
  as well: it must give up before sanlock does.

## Building and contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for the build commands, the CI setup
and the repository hooks. Releases are tagged `vX.Y.Z`; `nox version` prints
the tag a binary was built from.

## License

Apache License 2.0, see [LICENSE](LICENSE). sanlock itself is LGPL-2.1+ and
is linked dynamically.

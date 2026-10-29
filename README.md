# nox

[![ci](https://github.com/brainchillz/noxmox/actions/workflows/ci.yml/badge.svg)](https://github.com/brainchillz/noxmox/actions/workflows/ci.yml)

**Shared iSCSI storage for Proxmox that works the way you expected it to.**

Ever pointed a Proxmox cluster at an iSCSI LUN and found it tedious to manage?
You get LVM on top of the LUN, no snapshots, no thin provisioning, and a nagging
feeling that nothing is actually stopping two nodes from writing to the same
disk at once. The alternatives are a clustered filesystem with its own DLM and
fencing stack to babysit, or giving up and buying NFS.

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
a 1 TB iSCSI LUN.

- Volumes are device-mapper linear maps over 256 MiB extents. Activation takes
  under 150 ms. Resize is online and instant.
- Live migration through the Proxmox UI works in both directions. Measured
  downtime 6 to 44 ms.
- A second node that tries to start the same VM is refused in under a second
  and told which host holds the disk.
- Hard-reset a node mid-write and the survivor can take its disks about two
  minutes later. A node whose lease renewals stall is reset by the watchdog
  before anyone else can touch its volumes.
- The daemon can be restarted under a running VM. Leases survive as sanlock
  orphans and are re-adopted.

Snapshots, thin provisioning, templates and linked clones are not built yet.
See [Known limits](#known-limits-of-the-prototype) before trusting it with
anything you love.

## Why not just use ...

| Option | What you give up |
|---|---|
| LVM on iSCSI (what Proxmox offers) | no snapshots, no thin, the lock lives in the cluster filesystem so standalone nodes get no protection at all, no fencing of a wedged node |
| OCFS2 / GFS2 | a kernel filesystem, a DLM, corosync coupling, and a fencing stack to tune |
| NFS or Ceph | a different storage protocol; your existing SAN stays idle |
| nox | a Go daemon, a Perl plugin, and sanlock. Nothing in kernel space but device-mapper |

## Pieces

| Path | What |
|---|---|
| `cmd/nox` | single binary: `nox daemon` (noxd) and the CLI |
| `internal/store` | on-disk format v1, O_DIRECT device I/O, allocator, dm tables |
| `internal/sanlock` | cgo binding over libsanlock (leases, orphans, host events) |
| `internal/daemon` | noxd: lockspace join, lease holding, migration handoff, socket API |
| `internal/proto` | JSON-over-Unix-socket protocol shared by CLI and plugin |
| `pve/NoxPlugin.pm` | `PVE::Storage::Custom::NoxPlugin`, storage type `nox` |
| `deploy/` | `noxd.service` and `install.sh` |

## On-disk layout (format v1)

| Offset | Region |
|---|---|
| 0 | superblock (4 KiB), mirrored in the last 4 KiB of the LUN |
| 1 MiB | sanlock lockspace (delta leases, host ids 1..2000) |
| 2 MiB | metadata lease (paxos) |
| 3 MiB | one paxos lease per volume slot, 1 MiB each (1024 slots by default) |
| after leases | volume table: 2 x 4 KiB record per slot, CRC32C + generation, A/B copies |
| next extent boundary | data extents (256 MiB default) |

There is no journal. A record is one aligned 4 KiB block; writers alternate
copies and readers take the highest valid generation, so a torn write can
only lose the write in flight, never the previous version. The allocator
bitmap is derived from the table on every mutation, under the metadata lease.

## Lease model

- A running VM's volume is held **exclusive** by noxd on that node, as a
  sanlock orphan lease (`SANLK_RES_PERSISTENT`), so noxd can restart without
  the VM noticing and re-adopts its leases on start.
- Live migration is the only dual-open window. The target asks for a
  **shared** lease; if the source holds exclusive, the target sends a sanlock
  host event through the lockspace and the source converts. Both hold shared
  during the memory copy. When the source deactivates, the target's upgrader
  loop converts back to exclusive within two seconds.
- A dead host's leases expire through sanlock's delta lease; measured 125 s
  from hard reset to the survivor acquiring on this hardware (io_timeout 10).
  A node whose sanlock stops renewing is reset by wdmd and softdog 123 s
  after the last renewal, before any peer can take its leases.
- Fencing is wdmd on a kernel watchdog device. On the lab's HP EliteDesk
  800 G6 the iTCO timer loads but never resets the machine (tested by
  opening it and not petting it for four minutes), so wdmd uses softdog
  via a udev symlink `/dev/watchdog-soft`, and PVE's watchdog-mux is given
  the iTCO by loading it first. That leaves PVE HA self-fencing nominal on
  those nodes; use real hardware watchdogs or IPMI fencing in production.

## Standalone nodes

Nothing in nox talks to corosync or pmxcfs. Each node needs a unique host id in
`/etc/default/noxd` and its own `pvesm add nox ...` entry, and that is all.
Between nodes that are not clustered:

- A VM whose disk is held by one node cannot be started on another. The second
  start is refused within a second and names the holder.
- Cold moves are a config file copy. Shut down on A, copy the guest config to B,
  start it there. No disk copy; the lease moves with the start.
- If a node dies, its leases expire about two minutes later and any other node
  can start its guests. There is no quorum to reason about.
- Live migration is a cluster feature of Proxmox and is not available. The
  remote-migrate path between separate nodes copies the disk, which works but
  duplicates the volume on the same LUN.

Give each node a disjoint VMID range. Volume names are derived from the VMID,
and two unrelated guests both numbered 100 would share a name prefix in the
pool. Creation still picks free names, but a destroy with "purge unreferenced
disks" on one node could remove the other node's volumes.

## Install on each PVE node

    ./deploy/install.sh root@pve-01 1 /dev/disk/by-id/scsi-<wwid>
    ./deploy/install.sh root@pve-02 2 /dev/disk/by-id/scsi-<wwid>

Host ids must be unique per node and stable. Then, once:

    nox format --device /dev/disk/by-id/scsi-<wwid>        # on any node
    pvesm add nox noxpool --content images,rootdir --shared 1

## CLI

    nox status | list | info NAME
    nox create NAME SIZE | delete NAME | resize NAME SIZE
    nox activate NAME [--shared] [--timeout SEC] | deactivate NAME

## Known limits of the prototype

- No snapshots, templates, linked clones, or renames. Full clones work.
- Event-based migration handoff costs one sanlock renewal cycle (about 10
  to 30 s) before the memory copy starts. All volumes of one guest are
  folded into a single event.
- One pool per node (one socket, `/run/noxd.sock`).
- Deleting a volume requires it to be inactive everywhere; the daemon proves
  that by taking its lease exclusively.
- A two-node cluster needs `pvecm expected 1` or a QDevice before a VM can be
  recovered after a node dies.
- Never restart sanlock on a node with a joined lockspace: wdmd will reset
  the node. Stopping noxd is safe, leases stay as orphans.

## Building and contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for the build commands, the CI setup
and the repository hooks. Releases are tagged `vX.Y.Z`; `nox version` prints
the tag a binary was built from.

## License

Apache License 2.0, see [LICENSE](LICENSE). sanlock itself is LGPL-2.1+ and
is linked dynamically.

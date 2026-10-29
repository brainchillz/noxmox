# nox: a VMFS-style shared-LUN block pool for Proxmox VE

Status: working prototype (2026-10-28). One iSCSI or FC LUN, every node in the
cluster opens it read-write, VM disks are device-mapper volumes over fixed
extents, and sanlock leases on the LUN itself decide who may write. No
clustered filesystem, no kernel module, no DLM, no corosync dependency.
Design notes are kept outside the repository.

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

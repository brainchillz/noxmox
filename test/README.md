# Tests

| Directory | What | Needs |
|---|---|---|
| `../internal/store` (`go test`) | format and record codecs, allocator properties, churn against a file backed pool, superblock mirror fallback, mailbox codec, v1 to v2 upgrade, fuzz targets for every parser | nothing |
| `replay/` | crash replay: every write the store makes is recorded and replayed one write and one sector at a time; the pool must be consistent and equal to a known state at every step | nothing for the recorded variant; root, `dmsetup` and a working dm-log-writes for the block layer variant |
| `chaos/` | scenario suite against a real two node lab: daemon kills, mid-create kills, concurrent creates, delete against activate, aborted migration, clock skew, hard reset, frozen sanlock, iSCSI path loss | the lab in `chaos/lab.env`, root ssh to the nodes |
| `measure.sh` | reproduces the numbers in the README on the lab | same as chaos |
| `pve-stubs/` | just enough of the PVE Perl modules for `perl -c pve/NoxPlugin.pm` without Proxmox | nothing |

CI runs the store tests, the recorded replay and the Perl syntax check.

## Fuzzing

    go test ./internal/store/ -run '^$' -fuzz FuzzDecodeVolume -fuzztime 60s

Targets: `FuzzDecodeSuper`, `FuzzDecodeVolume`, `FuzzCheckTable`, `FuzzDecodeMailbox`.

## Chaos suite

    test/chaos/run.sh list              # scenarios
    test/chaos/run.sh                   # the default set (crashes the lease holder)
    test/chaos/run.sh all               # adds the scenario that takes every node down
    test/chaos/run.sh noxd-kill         # one scenario

Logs land in `chaos/logs/`, one line per scenario in `chaos/logs/results.tsv`
with the measured times. The crash scenarios run a writer in the test guest
that fsyncs every 4 KiB block; after the crash the survivor reads the disk
and checks that every acknowledged block is intact and at most the block
in flight is torn.

The suite resets nodes and expects to be the only user of the lab. With two
nodes it restores quorum with `pvecm expected 1` after a crash and moves the
guest config by hand, which a third node or a QDevice would make unnecessary.

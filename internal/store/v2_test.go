package store

import (
	"os"
	"path/filepath"
	"testing"
)

func tmpDevice(t *testing.T, size int64) *Device {
	t.Helper()
	path := filepath.Join(t.TempDir(), "lun.img")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	f.Close()
	dev, err := OpenDevice(path)
	if err != nil {
		t.Skipf("O_DIRECT not supported here: %v", err)
	}
	t.Cleanup(func() { dev.Close() })
	return dev
}

// A pool formatted by the first release upgrades in place: the table is
// read unchanged, old records keep working, new records carry v2 fields,
// and the mailbox lands in the gap before the data.
func TestUpgradeV1(t *testing.T) {
	dev := tmpDevice(t, 4<<30)
	p, err := FormatVersion(dev, 16*MiB, 8, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if p.Super.Version != 1 || p.Super.HasMailbox() {
		t.Fatal("v1 format wrong")
	}
	tb, _ := p.ReadTable()
	v1, err := p.Create(tb, 0, "vm-100-disk-0", 40*MiB)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.CreateWith(tb, 1, "vm-100-disk-1.qcow2", 16*MiB, CreateOpts{Format: FormatQcow2}); err == nil {
		t.Fatal("v1 pool accepted a qcow2 record")
	}
	dataOff := p.Super.DataOff

	n, err := p.PrepareUpgrade("test")
	if err != nil {
		t.Fatal(err)
	}
	if n.MailboxOff+n.MailboxSize() > dataOff || n.MailboxLeaseOff < p.Super.VolTableOff+p.Super.TableSize() {
		t.Fatalf("mailbox placed badly: lease %d mailbox %d data %d", n.MailboxLeaseOff, n.MailboxOff, dataOff)
	}
	// crash before WriteSuper: still a valid v1 pool
	q, err := Open(dev)
	if err != nil || q.Super.Version != 1 {
		t.Fatalf("pool damaged by PrepareUpgrade: %v", err)
	}
	if err := p.WriteSuper(n); err != nil {
		t.Fatal(err)
	}
	q, err = Open(dev)
	if err != nil {
		t.Fatal(err)
	}
	if q.Super.Version != 2 || !q.Super.HasMailbox() || q.Super.DataOff != dataOff || q.Super.Tool != "test" || q.Super.IOTimeout != 10 || q.Super.MaxHosts < MinHosts || q.Super.MaxHosts > DefaultHosts {
		t.Fatalf("upgraded super wrong: %+v", q.Super)
	}
	tb, _ = q.ReadTable()
	if v := tb.Lookup("vm-100-disk-0"); v == nil || v.Size != v1.Size || v.Runs[0] != v1.Runs[0] {
		t.Fatal("old record not readable after upgrade")
	}
	top, err := q.CreateWith(tb, 1, "vm-100-disk-1.qcow2", 16*MiB, CreateOpts{Format: FormatQcow2, Parent: tb.Lookup("vm-100-disk-0"), Creator: 7})
	if err != nil {
		t.Fatal(err)
	}
	tb2, _ := q.ReadTable()
	got := tb2.Lookup("vm-100-disk-1.qcow2")
	if !got.HasParent() || got.ParentSlot() != 0 || got.Format != FormatQcow2 || got.Creator != 7 {
		t.Fatalf("v2 fields lost: %+v", got)
	}
	if err := q.Delete(tb2, tb2.Lookup("vm-100-disk-0")); err == nil {
		t.Fatal("deleted a backing volume with a child")
	}
	if probs, _ := q.Check(); len(probs) != 0 {
		t.Fatalf("check: %v", probs)
	}
	// growing the old v1 record rewrites it as v2; still fine
	if _, err := q.Grow(tb2, tb2.Lookup("vm-100-disk-0"), 50*MiB); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Rename(tb2, top, "vm-100-disk-2.qcow2"); err != nil {
		t.Fatal(err)
	}
	tb3, _ := q.ReadTable()
	if tb3.Lookup("vm-100-disk-2.qcow2") == nil || tb3.Lookup("vm-100-disk-1.qcow2") != nil {
		t.Fatal("rename not persisted")
	}
	t.Logf("upgraded pool: max hosts %d, mailbox at %d", q.Super.MaxHosts, q.Super.MailboxOff)
	if probs, _ := q.Check(); len(probs) != 0 {
		t.Fatalf("check after grow/rename: %v", probs)
	}
	// reload on another handle sees the upgrade
	stale := &Pool{Dev: dev, Super: p.Super}
	stale.Super = &Super{Version: 1, DevSize: dev.Size}
	if ch, err := stale.ReloadSuper(); err != nil || !ch || stale.Super.Version != 2 {
		t.Fatalf("reload: changed=%v err=%v", ch, err)
	}
}

func TestMailbox(t *testing.T) {
	dev := tmpDevice(t, 4<<30)
	p, err := Format(dev, 16*MiB, 8)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Super.HasMailbox() {
		t.Fatal("v2 format has no mailbox")
	}
	es, seq, err := p.ReadMailbox(1)
	if err != nil || len(es) != 0 || seq != 0 {
		t.Fatalf("fresh mailbox: %v %d %v", es, seq, err)
	}
	now := int64(1000)
	if err := p.PostMail(2, MailEntry{Sender: 1, Op: MailConvertShared, Arg: 5, Seq: 1, Deadline: now + 60}, now); err != nil {
		t.Fatal(err)
	}
	if err := p.PostMail(2, MailEntry{Sender: 3, Op: MailConvertSharedGuest, Arg: 900, Seq: 9, Deadline: now + 30}, now); err != nil {
		t.Fatal(err)
	}
	// repeat from sender 1 replaces, does not duplicate
	if err := p.PostMail(2, MailEntry{Sender: 1, Op: MailConvertShared, Arg: 5, Seq: 2, Deadline: now + 90}, now); err != nil {
		t.Fatal(err)
	}
	es, seq, err = p.ReadMailbox(2)
	if err != nil || len(es) != 2 || seq != 3 {
		t.Fatalf("after posts: %v seq %d err %v", es, seq, err)
	}
	// expired entries vanish on the next write
	if err := p.PostMail(2, MailEntry{Sender: 4, Op: MailConvertShared, Arg: 1, Seq: 1, Deadline: now + 200}, now+100); err != nil {
		t.Fatal(err)
	}
	es, _, _ = p.ReadMailbox(2)
	if len(es) != 1 || es[0].Sender != 4 {
		t.Fatalf("expiry: %v", es)
	}
	if err := p.ClearMail(2, 4, now+100); err != nil {
		t.Fatal(err)
	}
	if es, _, _ = p.ReadMailbox(2); len(es) != 0 {
		t.Fatalf("clear: %v", es)
	}
	// other hosts' blocks untouched, out of range refused
	if es, _, _ = p.ReadMailbox(1); len(es) != 0 {
		t.Fatal("host 1 block changed")
	}
	if _, _, err := p.ReadMailbox(uint64(p.Super.MaxHosts) + 1); err == nil {
		t.Fatal("accepted host id beyond max")
	}
	// fill to capacity: the oldest deadlines are dropped, never an error
	for i := 0; i < MailboxCap+10; i++ {
		if err := p.PostMail(1, MailEntry{Sender: uint32(10 + i), Op: 1, Arg: uint64(i), Deadline: now + int64(i)}, now); err != nil {
			t.Fatal(err)
		}
	}
	if es, _, _ = p.ReadMailbox(1); len(es) != MailboxCap {
		t.Fatalf("cap: %d", len(es))
	}
}

func FuzzDecodeMailbox(f *testing.F) {
	b, _ := encodeMailbox(3, 1, []MailEntry{{1, 1, 5, 1, 100}})
	f.Add(b)
	f.Add(make([]byte, MailboxBlock))
	f.Fuzz(func(t *testing.T, b []byte) {
		es, _, ok := decodeMailbox(b, 3)
		if ok && len(es) > MailboxCap {
			t.Fatal("too many entries")
		}
	})
}

func TestRepairCopies(t *testing.T) {
	dev := tmpDevice(t, 4<<30)
	p, err := Format(dev, 16*MiB, 8)
	if err != nil {
		t.Fatal(err)
	}
	tb, _ := p.ReadTable()
	v, err := p.Create(tb, 2, "vm-1-disk-0", 32*MiB)
	if err != nil {
		t.Fatal(err)
	}
	// damage the unused copy with garbage: one valid copy left
	junk := make([]byte, RecSize)
	for i := range junk {
		junk[i] = 0xA5
	}
	if err := dev.WriteAt(p.Super.RecOff(2, 1), junk); err != nil {
		t.Fatal(err)
	}
	if probs, _ := p.Check(); len(probs) != 0 {
		t.Fatalf("one valid copy must pass check: %v", probs)
	}
	fixed, err := p.RepairCopies()
	if err != nil || len(fixed) != 1 || fixed[0] != 2 {
		t.Fatalf("repair: %v %v", fixed, err)
	}
	raw, _ := dev.ReadAt(p.Super.RecOff(2, 1), RecSize)
	if r, ok := decodeVolume(raw); !ok || r.Name != v.Name || r.Gen != v.Gen {
		t.Fatal("bad copy not restored")
	}
	if fixed, _ = p.RepairCopies(); len(fixed) != 0 {
		t.Fatal("repair is not idempotent")
	}
}

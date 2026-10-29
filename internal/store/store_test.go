package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFormatAndTable(t *testing.T) {
	dir, err := os.MkdirTemp(".", "stest-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "lun.img")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(2 << 30); err != nil {
		t.Fatal(err)
	}
	f.Close()
	dev, err := OpenDevice(path)
	if err != nil {
		t.Skipf("O_DIRECT not supported here: %v", err)
	}
	p, err := Format(dev, 16*MiB, 8)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("layout: table@%d data@%d extents=%d", p.Super.VolTableOff, p.Super.DataOff, p.Super.DataExtents)
	dev.Close()
	dev, _ = OpenDevice(path)
	p, err = Open(dev)
	if err != nil {
		t.Fatal(err)
	}
	tb, err := p.ReadTable()
	if err != nil {
		t.Fatal(err)
	}
	if len(tb.Active()) != 0 {
		t.Fatal("expected empty table")
	}
	s1, _ := tb.FreeSlot()
	v1, err := p.Create(tb, s1, "vm-100-disk-0", 40*MiB)
	if err != nil {
		t.Fatal(err)
	}
	if v1.Extents() != 3 {
		t.Fatalf("40M/16M should be 3 extents, got %d", v1.Extents())
	}
	s2, _ := tb.FreeSlot()
	v2, err := p.Create(tb, s2, "vm-101-disk-0", 16*MiB)
	if err != nil {
		t.Fatal(err)
	}
	if s2 != 1 || v2.Runs[0].Start != 3 {
		t.Fatalf("unexpected placement slot=%d runs=%v", s2, v2.Runs)
	}
	if _, err := p.Create(tb, 2, "vm-100-disk-0", MiB); err == nil {
		t.Fatal("duplicate name accepted")
	}
	// reread and verify persistence
	tb2, _ := p.ReadTable()
	if tb2.Lookup("vm-100-disk-0") == nil || tb2.Lookup("vm-101-disk-0") == nil {
		t.Fatal("volumes not persisted")
	}
	// delete v1, create v3 that reuses the hole
	if err := p.Delete(tb2, tb2.Lookup("vm-100-disk-0")); err != nil {
		t.Fatal(err)
	}
	tb3, _ := p.ReadTable()
	if tb3.Lookup("vm-100-disk-0") != nil {
		t.Fatal("delete did not persist")
	}
	if free := tb3.FreeExtents(p.Super.DataExtents); free != p.Super.DataExtents-1 {
		t.Fatalf("free extents %d", free)
	}
	s3, _ := tb3.FreeSlot()
	if s3 != 0 {
		t.Fatalf("slot 0 should be reusable, got %d", s3)
	}
	v3, err := p.Create(tb3, s3, "vm-102-disk-0", 100*MiB)
	if err != nil {
		t.Fatal(err)
	}
	if v3.Gen != 3 {
		t.Fatalf("gen should keep increasing across reuse, got %d", v3.Gen)
	}
	// grow
	nv, err := p.Grow(tb3, v3, 200*MiB)
	if err != nil {
		t.Fatal(err)
	}
	if nv.Extents() != 13 {
		t.Fatalf("grow: %d extents", nv.Extents())
	}
	t.Logf("table:\n%s", p.DMTable(nv))
	// over-allocate
	if _, err := p.Create(tb3, 2, "big", 100<<30); err == nil {
		t.Fatal("overallocation accepted")
	}
	// torn write resilience: corrupt the winner copy of slot 1, loser must still be valid? (loser is the free zero copy here) -> expect slot free
	tb4, _ := p.ReadTable()
	if tb4.Lookup("vm-102-disk-0") == nil {
		t.Fatal("v3 missing after grow")
	}
}

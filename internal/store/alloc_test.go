package store

import (
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

// Allocator invariants under random churn: runs never overlap, never leave
// the device, the sum of runs covers the request exactly, and the bitmap
// derived from the table always matches what the records say.

func checkRuns(t *testing.T, used []bool, runs []Run, need uint64) {
	t.Helper()
	var got uint64
	seen := map[uint32]bool{}
	for _, r := range runs {
		if r.Count == 0 {
			t.Fatalf("empty run %+v", r)
		}
		for e := r.Start; e < r.Start+r.Count; e++ {
			if e >= uint32(len(used)) {
				t.Fatalf("run %+v beyond %d extents", r, len(used))
			}
			if used[e] {
				t.Fatalf("run %+v hits used extent %d", r, e)
			}
			if seen[e] {
				t.Fatalf("extent %d given twice", e)
			}
			seen[e] = true
		}
		got += uint64(r.Count)
	}
	if got != need {
		t.Fatalf("asked %d extents, got %d", need, got)
	}
}

func TestAllocateProperties(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for iter := 0; iter < 2000; iter++ {
		n := 1 + rng.Intn(300)
		used := make([]bool, n)
		for i := range used {
			used[i] = rng.Intn(3) == 0
		}
		free := 0
		for _, u := range used {
			if !u {
				free++
			}
		}
		need := uint64(rng.Intn(n + 2))
		runs, err := allocate(used, need)
		if need == 0 {
			if err != nil || len(runs) != 0 {
				t.Fatalf("need 0: runs=%v err=%v", runs, err)
			}
			continue
		}
		if need > uint64(free) {
			if err == nil {
				t.Fatalf("allocated %d of %d free", need, free)
			}
			continue
		}
		if err != nil {
			t.Fatalf("iter %d: %v (need %d free %d)", iter, err, need, free)
		}
		checkRuns(t, used, runs, need)
		for i := 1; i < len(runs); i++ {
			if runs[i].Start <= runs[i-1].Start {
				t.Fatalf("runs not sorted: %v", runs)
			}
		}
	}
}

// TestPoolChurn drives create/grow/delete through a real (file backed) pool
// and verifies, after every mutation and after a fresh re-read, that the
// table is consistent and matches an independent model.
func TestPoolChurn(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lun.img")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	const extent = 16 * MiB
	const slots = 32
	if err := f.Truncate(4 << 30); err != nil {
		t.Fatal(err)
	}
	f.Close()
	dev, err := OpenDevice(path)
	if err != nil {
		t.Skipf("O_DIRECT not supported here: %v", err)
	}
	defer dev.Close()
	p, err := Format(dev, extent, slots)
	if err != nil {
		t.Fatal(err)
	}
	tb, err := p.ReadTable()
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(7))
	model := map[string]uint64{} // name -> size
	next := 0
	var runCounts, vols int
	for op := 0; op < 600; op++ {
		switch r := rng.Intn(10); {
		case r < 5: // create
			name := "vm-1-disk-" + itoa(next)
			next++
			size := uint64(1+rng.Intn(12)) * extent / uint64(1+rng.Intn(3))
			slot, err := tb.FreeSlot()
			if err != nil {
				continue
			}
			v, err := p.Create(tb, slot, name, size)
			if err != nil {
				if tb.FreeExtents(p.Super.DataExtents) >= (size+extent-1)/extent {
					t.Fatalf("create %s %d failed with %d free: %v", name, size, tb.FreeExtents(p.Super.DataExtents), err)
				}
				continue
			}
			model[name] = size
			runCounts += len(v.Runs)
			vols++
		case r < 8: // delete
			if len(model) == 0 {
				continue
			}
			name := pick(rng, model)
			if err := p.Delete(tb, tb.Lookup(name)); err != nil {
				t.Fatal(err)
			}
			delete(model, name)
		default: // grow
			if len(model) == 0 {
				continue
			}
			name := pick(rng, model)
			v := tb.Lookup(name)
			ns := v.Size + uint64(1+rng.Intn(4))*extent/2
			nv, err := p.Grow(tb, v, ns)
			if err != nil {
				continue
			}
			model[name] = nv.Size
		}
		if probs := checkRaw(p.Super, mustRead(t, p)); len(probs) != 0 {
			t.Fatalf("op %d: %v", op, probs)
		}
		// Independent re-read must agree with the in-memory table and model.
		fresh, err := p.ReadTable()
		if err != nil {
			t.Fatal(err)
		}
		if len(fresh.Active()) != len(model) {
			t.Fatalf("op %d: %d active on disk, model has %d", op, len(fresh.Active()), len(model))
		}
		var ext uint64
		for name, size := range model {
			v := fresh.Lookup(name)
			if v == nil || v.Size != size {
				t.Fatalf("op %d: %s missing or wrong size", op, name)
			}
			ext += v.Extents()
		}
		if free := fresh.FreeExtents(p.Super.DataExtents); free+ext != p.Super.DataExtents {
			t.Fatalf("op %d: free %d + used %d != %d", op, free, ext, p.Super.DataExtents)
		}
	}
	t.Logf("churn: %d creates, mean %.2f runs per volume at creation, %d extents", vols, float64(runCounts)/float64(vols), p.Super.DataExtents)
}

func mustRead(t *testing.T, p *Pool) []byte {
	t.Helper()
	raw, err := p.Dev.ReadAt(p.Super.VolTableOff, int(p.Super.TableSize()))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func pick(rng *rand.Rand, m map[string]uint64) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// deterministic order for a reproducible run
	sortStrings(keys)
	return keys[rng.Intn(len(keys))]
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

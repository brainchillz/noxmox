package replay

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"noxmox/internal/store"
)

const (
	devSize = 1 << 30
	logSize = 256 << 20
	extent  = 16 * store.MiB
	slots   = 64
	dmName  = "nox-replay"
)

// snapshot is what the test expects the table to say after a given op.
type snapshot map[string]volState

type volState struct {
	Size uint64
	Runs []store.Run
}

func snap(t *store.Table) snapshot {
	s := snapshot{}
	for _, v := range t.Active() {
		s[v.Name] = volState{v.Size, append([]store.Run(nil), v.Runs...)}
	}
	return s
}

func run(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

var ops = []struct {
	kind string
	name string
	size uint64
}{
	{"create", "vm-100-disk-0", 40 * store.MiB},
	{"create", "vm-101-disk-0", 16 * store.MiB},
	{"create", "vm-102-disk-0", 100 * store.MiB},
	{"grow", "vm-101-disk-0", 50 * store.MiB},
	{"delete", "vm-100-disk-0", 0},
	{"create", "vm-103-disk-0", 70 * store.MiB}, // reuses slot 0 and the hole
	{"grow", "vm-103-disk-0", 120 * store.MiB},
	{"delete", "vm-102-disk-0", 0},
	{"delete", "vm-101-disk-0", 0},
	{"create", "vm-104-disk-0", 300 * store.MiB},
	{"grow", "vm-104-disk-0", 500 * store.MiB},
	{"delete", "vm-104-disk-0", 0},
	{"create", "vm-105-disk-0", 8 * store.MiB},
}

// record runs the scenario with real store code on dev, emitting a mark
// after format and after every op, and returns the expected state after
// each mark.
func record(t *testing.T, dev *store.Device, mark func(string)) []snapshot {
	t.Helper()
	p, err := store.Format(dev, extent, slots)
	if err != nil {
		t.Fatal(err)
	}
	tb, err := p.ReadTable()
	if err != nil {
		t.Fatal(err)
	}
	expect := []snapshot{snap(tb)}
	mark("op0")
	for i, op := range ops {
		switch op.kind {
		case "create":
			slot, err := tb.FreeSlot()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := p.Create(tb, slot, op.name, op.size); err != nil {
				t.Fatalf("op %d %s: %v", i+1, op.name, err)
			}
		case "grow":
			if _, err := p.Grow(tb, tb.Lookup(op.name), op.size); err != nil {
				t.Fatalf("op %d grow %s: %v", i+1, op.name, err)
			}
		case "delete":
			if err := p.Delete(tb, tb.Lookup(op.name)); err != nil {
				t.Fatalf("op %d delete %s: %v", i+1, op.name, err)
			}
		}
		expect = append(expect, snap(tb))
		mark(fmt.Sprintf("op%d", i+1))
	}
	return expect
}

// replay rebuilds a device from the entries one write at a time and, inside
// each write, one sector at a time, and verifies the pool at every step.
func replay(t *testing.T, dir string, entries []Entry, ss uint32, expect []snapshot) {
	t.Helper()
	img := filepath.Join(dir, "replay.img")
	rf, err := os.OpenFile(img, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := rf.Truncate(devSize); err != nil {
		t.Fatal(err)
	}
	defer rf.Close()

	open := func() (*store.Pool, *store.Device, error) {
		rdev, err := store.OpenDevice(img)
		if err != nil {
			t.Fatal(err)
		}
		rp, err := store.Open(rdev)
		if err != nil {
			rdev.Close()
			return nil, nil, err
		}
		return rp, rdev, nil
	}

	// Before op k completes the device may show expect[k-1] or expect[k];
	// nothing else, ever.
	opIdx := 0
	formatted := false
	checked, applied, torn := 0, 0, 0
	verify := func(step string) {
		t.Helper()
		rp, rdev, err := open()
		if err != nil {
			if formatted {
				t.Fatalf("%s: pool unreadable once the superblock was written: %v", step, err)
			}
			return
		}
		defer rdev.Close()
		probs, err := rp.Check()
		if err != nil {
			t.Fatal(err)
		}
		if len(probs) != 0 {
			t.Fatalf("%s: inconsistent table: %v", step, probs)
		}
		rt, err := rp.ReadTable()
		if err != nil {
			t.Fatal(err)
		}
		got := snap(rt)
		ok := opIdx < len(expect) && reflect.DeepEqual(got, expect[opIdx])
		if !ok && opIdx > 0 {
			ok = reflect.DeepEqual(got, expect[opIdx-1])
		}
		if !ok {
			t.Fatalf("%s (op %d): table %s matches neither the previous nor the next state", step, opIdx, fmtSnap(got))
		}
		checked++
	}
	for i, e := range entries {
		if e.IsMark() {
			var n int
			if k, _ := fmt.Sscanf(e.Mark, "op%d", &n); k != 1 {
				continue // the kernel adds its own dm-log-writes-end mark
			}
			rp, rdev, err := open()
			if err != nil {
				t.Fatalf("mark %s: %v", e.Mark, err)
			}
			rt, err := rp.ReadTable()
			rdev.Close()
			if err != nil {
				t.Fatal(err)
			}
			if got := snap(rt); !reflect.DeepEqual(got, expect[n]) {
				t.Fatalf("at %s: got %s want %s", e.Mark, fmtSnap(got), fmtSnap(expect[n]))
			}
			formatted = true
			opIdx = n + 1
			continue
		}
		if !e.IsWrite() {
			continue
		}
		// torn variants: the first k sectors landed, the rest did not
		for k := uint64(1); k < e.NrSectors; k++ {
			if _, err := rf.WriteAt(e.Data[:k*uint64(ss)], int64(e.Sector*uint64(ss))); err != nil {
				t.Fatal(err)
			}
			torn++
			verify(fmt.Sprintf("entry %d torn after %d/%d sectors", i, k, e.NrSectors))
		}
		if _, err := rf.WriteAt(e.Data, int64(e.Sector*uint64(ss))); err != nil {
			t.Fatal(err)
		}
		applied++
		if e.Sector == 0 {
			// the primary superblock is complete: from here on the pool must
			// open at every step, torn mirror writes included
			formatted = true
		}
		verify(fmt.Sprintf("entry %d complete", i))
	}
	t.Logf("replayed %d writes plus %d torn variants, %d states verified", applied, torn, checked)
}

// TestCrashReplayRecorded uses the device layer's own write recorder, so it
// runs anywhere, including CI without root.
func TestCrashReplayRecorded(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lun.img")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(devSize); err != nil {
		t.Fatal(err)
	}
	f.Close()
	dev, err := store.OpenDevice(path)
	if err != nil {
		t.Skipf("O_DIRECT not supported here: %v", err)
	}
	var entries []Entry
	dev.Record = func(off uint64, data []byte) {
		entries = append(entries, Entry{Sector: off / 512, NrSectors: uint64(len(data)) / 512, Flags: flagFUA, Data: data})
	}
	expect := record(t, dev, func(m string) { entries = append(entries, Entry{Flags: flagMark, Mark: m}) })
	dev.Close()
	replay(t, dir, entries, 512, expect)
}

// TestCrashReplayLogWrites records through dm-log-writes, so the log is what
// the block layer completed rather than what the store issued. Needs root,
// dmsetup and a kernel whose dm-log-writes works.
func TestCrashReplayLogWrites(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root for dm-log-writes")
	}
	if _, err := exec.LookPath("dmsetup"); err != nil {
		t.Skip("dmsetup not installed")
	}
	if err := exec.Command("modprobe", "dm-log-writes").Run(); err != nil {
		t.Skipf("dm-log-writes unavailable: %v", err)
	}
	dir := t.TempDir()
	mk := func(name string, size int64) string {
		p := filepath.Join(dir, name)
		f, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Truncate(size); err != nil {
			t.Fatal(err)
		}
		f.Close()
		return run(t, "losetup", "--find", "--show", p)
	}
	data := mk("data.img", devSize)
	logd := mk("log.img", logSize)
	t.Cleanup(func() {
		exec.Command("dmsetup", "remove", "--retry", dmName).Run()
		exec.Command("losetup", "-d", data).Run()
		exec.Command("losetup", "-d", logd).Run()
	})
	run(t, "dmsetup", "create", dmName, "--table", fmt.Sprintf("0 %d log-writes %s %s", devSize/512, data, logd))
	dev, err := store.OpenDevice("/dev/mapper/" + dmName)
	if err != nil {
		t.Fatal(err)
	}
	expect := record(t, dev, func(m string) { run(t, "dmsetup", "message", dmName, "0", "mark", m) })
	dev.Close()
	st := run(t, "dmsetup", "status", dmName)
	run(t, "dmsetup", "remove", "--retry", dmName)
	if strings.Contains(st, "logging_disabled") {
		t.Skipf("dm-log-writes stopped logging on this kernel: %s", st)
	}
	if keep := os.Getenv("NOX_KEEP_LOG"); keep != "" {
		run(t, "dd", "if="+logd, "of="+keep, "bs=1M", "status=none")
	}
	entries, ss, err := ReadLog(logd)
	if err != nil {
		t.Fatal(err)
	}
	var writes, marks int
	for _, e := range entries {
		if e.IsWrite() {
			writes++
		}
		if e.IsMark() && strings.HasPrefix(e.Mark, "op") {
			marks++
		}
	}
	t.Logf("log: %d entries, %d writes, %d marks, sector size %d", len(entries), writes, marks, ss)
	if marks != len(expect) {
		t.Fatalf("expected %d marks, got %d", len(expect), marks)
	}
	replay(t, dir, entries, ss, expect)
}

func fmtSnap(s snapshot) string {
	names := make([]string, 0, len(s))
	for n := range s {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("{")
	for _, n := range names {
		fmt.Fprintf(&b, "%s:%d%v ", n, s[n].Size, s[n].Runs)
	}
	b.WriteString("}")
	return b.String()
}

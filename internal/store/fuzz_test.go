package store

import (
	"testing"
)

// The daemon runs as root and reads blocks that other hosts wrote. No input
// may make the parsers panic; they may only reject.

func FuzzDecodeSuper(f *testing.F) {
	s, _ := Layout(2<<30, 16*MiB, 8)
	f.Add(s.encode())
	f.Add(make([]byte, SBSize))
	f.Add([]byte(Magic))
	f.Fuzz(func(t *testing.T, b []byte) {
		d, err := decodeSuper(b)
		if err == nil && d == nil {
			t.Fatal("nil super without error")
		}
	})
}

func FuzzDecodeVolume(f *testing.F) {
	v := &Volume{Slot: 3, Gen: 7, State: StateActive, Name: "vm-100-disk-0", Size: 40 * MiB, Runs: []Run{{0, 3}}}
	b, _ := v.encode(Version)
	f.Add(b)
	b1, _ := v.encode(1)
	f.Add(b1)
	f.Add(make([]byte, RecSize))
	f.Add([]byte(VolMagic))
	f.Fuzz(func(t *testing.T, b []byte) {
		d, ok := decodeVolume(b)
		if ok != (d != nil) {
			t.Fatal("ok/result mismatch")
		}
		if ok {
			// Whatever decoded must re-encode, or the record could never
			// have been written by us.
			if _, err := d.encode(Version); err != nil {
				t.Fatalf("decoded record does not re-encode: %v", err)
			}
		}
	})
}

// FuzzCheckTable feeds a whole small table to the checker.
func FuzzCheckTable(f *testing.F) {
	s, _ := Layout(2<<30, 16*MiB, 4)
	raw := make([]byte, s.TableSize())
	v := &Volume{Slot: 1, Gen: 1, State: StateActive, Name: "vm-1-disk-0", Size: 20 * MiB, Runs: []Run{{2, 2}}}
	b, _ := v.encode(Version)
	copy(raw[1*2*RecSize:], b)
	f.Add(raw)
	f.Fuzz(func(t *testing.T, raw []byte) {
		if uint64(len(raw)) < s.TableSize() {
			return
		}
		checkRaw(s, raw[:s.TableSize()])
	})
}

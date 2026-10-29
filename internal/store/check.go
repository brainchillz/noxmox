package store

import (
	"fmt"
	"sort"
)

// Problem is one inconsistency found by Check.
type Problem struct {
	Slot int // -1 when not slot specific
	Msg  string
}

func (p Problem) String() string {
	if p.Slot < 0 {
		return p.Msg
	}
	return fmt.Sprintf("slot %d: %s", p.Slot, p.Msg)
}

// Check walks a raw volume table and reports every inconsistency it can see
// without leases: records whose copies are both unreadable, copies that
// disagree about their slot, duplicate names, runs outside the data area,
// runs that overlap another volume, and sizes that do not match the extents
// allocated. An empty result means the table is internally consistent.
//
// Check is deliberately tolerant of one bad copy per slot: the A/B scheme
// guarantees nothing more than one valid copy after a torn write.
func (p *Pool) Check() ([]Problem, error) {
	raw, err := p.Dev.ReadAt(p.Super.VolTableOff, int(p.Super.TableSize()))
	if err != nil {
		return nil, err
	}
	return checkRaw(p.Super, raw), nil
}

func checkRaw(s *Super, raw []byte) []Problem {
	var out []Problem
	add := func(slot int, f string, a ...any) { out = append(out, Problem{slot, fmt.Sprintf(f, a...)}) }

	type owner struct {
		slot uint32
		name string
	}
	used := map[uint64]owner{}
	names := map[string]uint32{}
	for slot := uint32(0); slot < s.Slots; slot++ {
		base := int(slot) * 2 * RecSize
		a, okA := decodeVolume(raw[base : base+RecSize])
		b, okB := decodeVolume(raw[base+RecSize : base+2*RecSize])
		if okA && a.Slot != slot {
			add(int(slot), "copy A claims slot %d", a.Slot)
			okA = false
		}
		if okB && b.Slot != slot {
			add(int(slot), "copy B claims slot %d", b.Slot)
			okB = false
		}
		var w *Volume
		switch {
		case okA && okB:
			w = a
			if b.Gen > a.Gen {
				w = b
			}
			if a.Gen == b.Gen && (a.Name != b.Name || a.State != b.State) {
				add(int(slot), "both copies have gen %d but differ", a.Gen)
			}
		case okA:
			w = a
		case okB:
			w = b
		default:
			// A never-written slot is all zeros: fine. Anything else is damage.
			if !allZero(raw[base : base+2*RecSize]) {
				add(int(slot), "no valid copy")
			}
			continue
		}
		if w.State != StateActive {
			if w.State != StateFree {
				add(int(slot), "unknown state %d", w.State)
			}
			continue
		}
		if w.Name == "" {
			add(int(slot), "active record without a name")
		} else if prev, dup := names[w.Name]; dup {
			add(int(slot), "name %q also used by slot %d", w.Name, prev)
		} else {
			names[w.Name] = slot
		}
		if w.Size == 0 {
			add(int(slot), "%s: size 0", w.Name)
		}
		var ext uint64
		for _, r := range w.Runs {
			if r.Count == 0 {
				add(int(slot), "%s: empty run at %d", w.Name, r.Start)
				continue
			}
			end := uint64(r.Start) + uint64(r.Count)
			if end > s.DataExtents {
				add(int(slot), "%s: run %d+%d beyond data area (%d extents)", w.Name, r.Start, r.Count, s.DataExtents)
				end = s.DataExtents
			}
			for e := uint64(r.Start); e < end; e++ {
				if o, taken := used[e]; taken {
					if o.slot == slot {
						add(int(slot), "%s: extent %d listed twice", w.Name, e)
					} else {
						add(int(slot), "%s: extent %d overlaps %s (slot %d)", w.Name, e, o.name, o.slot)
					}
					continue
				}
				used[e] = owner{slot, w.Name}
			}
			ext += uint64(r.Count)
		}
		need := (w.Size + s.ExtentSize - 1) / s.ExtentSize
		if ext < need {
			add(int(slot), "%s: size %d needs %d extents, has %d", w.Name, w.Size, need, ext)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Slot < out[j].Slot })
	return out
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

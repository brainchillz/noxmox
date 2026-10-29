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
			// A never-written slot is all zeros. One unreadable copy next to
			// a zero copy is a torn first write and the slot is still free.
			// Two unreadable non-zero copies cannot come from sequential
			// writes and mean damage.
			if !allZero(raw[base:base+RecSize]) && !allZero(raw[base+RecSize:base+2*RecSize]) {
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
		if w.HasParent() {
			ps := w.ParentSlot()
			if ps >= s.Slots {
				add(int(slot), "%s: parent slot %d out of range", w.Name, ps)
			} else {
				pb := int(ps) * 2 * RecSize
				pa, okPA := decodeVolume(raw[pb : pb+RecSize])
				pbv, okPB := decodeVolume(raw[pb+RecSize : pb+2*RecSize])
				var pw *Volume
				if okPA && pa.Slot == ps {
					pw = pa
				}
				if okPB && pbv.Slot == ps && (pw == nil || pbv.Gen > pw.Gen) {
					pw = pbv
				}
				if pw == nil || pw.State != StateActive {
					add(int(slot), "%s: parent slot %d is not an active volume", w.Name, ps)
				} else if ps == slot {
					add(int(slot), "%s: is its own parent", w.Name)
				}
			}
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

// RepairCopies rewrites, for every slot that has exactly one valid copy
// and one unreadable non-zero copy, the bad copy with the good record, so
// the next torn write cannot leave the slot without a valid copy. Caller
// holds the metadata lease. Returns the slots repaired.
func (p *Pool) RepairCopies() ([]uint32, error) {
	raw, err := p.Dev.ReadAt(p.Super.VolTableOff, int(p.Super.TableSize()))
	if err != nil {
		return nil, err
	}
	var fixed []uint32
	for slot := uint32(0); slot < p.Super.Slots; slot++ {
		base := int(slot) * 2 * RecSize
		ca, cb := raw[base:base+RecSize], raw[base+RecSize:base+2*RecSize]
		a, okA := decodeVolume(ca)
		b, okB := decodeVolume(cb)
		okA = okA && a.Slot == slot
		okB = okB && b.Slot == slot
		var good []byte
		var bad int
		switch {
		case okA && !okB && !allZero(cb):
			good, bad = ca, 1
		case okB && !okA && !allZero(ca):
			good, bad = cb, 0
		default:
			continue
		}
		if err := p.Dev.WriteAt(p.Super.RecOff(slot, bad), good); err != nil {
			return fixed, err
		}
		fixed = append(fixed, slot)
	}
	return fixed, nil
}

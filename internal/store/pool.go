package store

import (
	"crypto/rand"
	"errors"
	"fmt"
	"sort"
	"time"
)

// ErrNotFound is returned when a volume name has no active record.
var ErrNotFound = errors.New("volume not found")

// Pool is an open store on a device. It knows nothing about leases; callers
// must hold the metadata lease around any mutation.
type Pool struct {
	Dev   *Device
	Super *Super
}

// Format writes a fresh superblock and an empty volume table in the newest
// format. Lease regions are initialized by the caller through sanlock.
func Format(dev *Device, extentSize uint64, slots uint32) (*Pool, error) {
	return FormatVersion(dev, extentSize, slots, Version, "")
}

// FormatVersion formats in a specific version; tool is recorded in a v2
// superblock for diagnostics.
func FormatVersion(dev *Device, extentSize uint64, slots uint32, version uint32, tool string) (*Pool, error) {
	s, err := LayoutVersion(dev.Size, extentSize, slots, version)
	if err != nil {
		return nil, err
	}
	s.Tool = tool
	// Clear the volume table and the lease region headers so stale data from
	// a previous life of the LUN cannot be mistaken for records.
	if err := dev.Zero(s.VolTableOff, s.TableSize()); err != nil {
		return nil, err
	}
	if s.HasMailbox() {
		if err := dev.Zero(s.MailboxOff, s.MailboxSize()); err != nil {
			return nil, err
		}
	}
	sb := s.encode()
	if err := dev.WriteAt(0, sb); err != nil {
		return nil, err
	}
	if err := dev.WriteAt(dev.Size-SBSize, sb); err != nil {
		return nil, err
	}
	return &Pool{Dev: dev, Super: s}, nil
}

// Open reads and validates the superblock, falling back to the mirror.
func Open(dev *Device) (*Pool, error) {
	b, err := dev.ReadAt(0, SBSize)
	if err != nil {
		return nil, err
	}
	s, err := decodeSuper(b)
	if err != nil {
		mb, merr := dev.ReadAt(dev.Size-SBSize, SBSize)
		if merr != nil {
			return nil, err
		}
		if s, merr = decodeSuper(mb); merr != nil {
			return nil, fmt.Errorf("primary: %v; mirror: %v", err, merr)
		}
	}
	if s.DevSize != dev.Size {
		return nil, fmt.Errorf("device size changed: superblock says %d, device is %d", s.DevSize, dev.Size)
	}
	return &Pool{Dev: dev, Super: s}, nil
}

// Table is one consistent read of every slot: the winning record per slot.
type Table struct {
	Vols []*Volume // index = slot; nil when free
	// winner copy per slot, so the next write goes to the other one
	winner  []int
	freeGen map[uint32]uint64
}

// ReadTable reads the whole volume table in one I/O and resolves each slot.
func (p *Pool) ReadTable() (*Table, error) {
	raw, err := p.Dev.ReadAt(p.Super.VolTableOff, int(p.Super.TableSize()))
	if err != nil {
		return nil, err
	}
	t := &Table{Vols: make([]*Volume, p.Super.Slots), winner: make([]int, p.Super.Slots)}
	for slot := uint32(0); slot < p.Super.Slots; slot++ {
		base := int(slot) * 2 * RecSize
		a, okA := decodeVolume(raw[base : base+RecSize])
		b, okB := decodeVolume(raw[base+RecSize : base+2*RecSize])
		var w *Volume
		wi := -1
		if okA && a.Slot == slot {
			w, wi = a, 0
		}
		if okB && b.Slot == slot && (w == nil || b.Gen > w.Gen) {
			w, wi = b, 1
		}
		t.winner[slot] = wi
		if w != nil && w.State == StateActive {
			t.Vols[slot] = w
		} else if w != nil {
			// keep the free record around so Gen keeps increasing
			t.Vols[slot] = nil
			t.winner[slot] = wi
			t.setFreeGen(slot, w.Gen)
		}
	}
	return t, nil
}

func (t *Table) setFreeGen(slot uint32, gen uint64) {
	if t.freeGen == nil {
		t.freeGen = map[uint32]uint64{}
	}
	t.freeGen[slot] = gen
}

// Lookup finds an active volume by name.
func (t *Table) Lookup(name string) *Volume {
	for _, v := range t.Vols {
		if v != nil && v.Name == name {
			return v
		}
	}
	return nil
}

// Active lists active volumes sorted by name.
func (t *Table) Active() []*Volume {
	var out []*Volume
	for _, v := range t.Vols {
		if v != nil {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// bitmap marks used extents.
func (t *Table) bitmap(n uint64) []bool {
	used := make([]bool, n)
	for _, v := range t.Vols {
		if v == nil {
			continue
		}
		for _, r := range v.Runs {
			for e := uint64(r.Start); e < uint64(r.Start)+uint64(r.Count) && e < n; e++ {
				used[e] = true
			}
		}
	}
	return used
}

// FreeExtents counts unallocated extents.
func (t *Table) FreeExtents(n uint64) uint64 {
	var free uint64
	for _, u := range t.bitmap(n) {
		if !u {
			free++
		}
	}
	return free
}

// allocate picks runs covering `need` extents, first fit, largest holes first
// to keep run counts low.
func allocate(used []bool, need uint64) ([]Run, error) {
	type hole struct{ start, count uint64 }
	var holes []hole
	for i := uint64(0); i < uint64(len(used)); {
		if used[i] {
			i++
			continue
		}
		j := i
		for j < uint64(len(used)) && !used[j] {
			j++
		}
		holes = append(holes, hole{i, j - i})
		i = j
	}
	sort.Slice(holes, func(a, b int) bool { return holes[a].count > holes[b].count })
	var runs []Run
	for _, h := range holes {
		if need == 0 {
			break
		}
		take := h.count
		if take > need {
			take = need
		}
		runs = append(runs, Run{uint32(h.start), uint32(take)})
		need -= take
	}
	if need > 0 {
		return nil, errors.New("not enough free space")
	}
	sort.Slice(runs, func(a, b int) bool { return runs[a].Start < runs[b].Start })
	return runs, nil
}

func (p *Pool) writeRecord(t *Table, v *Volume) error {
	b, err := v.encode(p.Super.Version)
	if err != nil {
		return err
	}
	c := 0
	if t.winner[v.Slot] == 0 {
		c = 1
	}
	if err := p.Dev.WriteAt(p.Super.RecOff(v.Slot, c), b); err != nil {
		return err
	}
	t.winner[v.Slot] = c
	return nil
}

func (t *Table) nextGen(slot uint32) uint64 {
	if v := t.Vols[slot]; v != nil {
		return v.Gen + 1
	}
	if g, ok := t.freeGen[slot]; ok {
		return g + 1
	}
	return 1
}

// FreeSlot returns the first unused slot.
func (t *Table) FreeSlot() (uint32, error) { return t.FreeSlotAfter(^uint32(0)) }

// FreeSlotAfter returns the first unused slot above after (all of them when
// after is the maximum value).
func (t *Table) FreeSlotAfter(after uint32) (uint32, error) {
	start := 0
	if after != ^uint32(0) {
		start = int(after) + 1
	}
	for i := start; i < len(t.Vols); i++ {
		if t.Vols[i] == nil {
			return uint32(i), nil
		}
	}
	return 0, errors.New("no free volume slots")
}

// CreateOpts are the optional attributes of a new volume.
type CreateOpts struct {
	Format  uint32 // FormatRaw or FormatQcow2
	Parent  *Volume
	Creator uint32 // host id creating the volume
}

// Create allocates extents and writes an active record into slot. Caller
// holds the metadata lease and has already initialized the slot's lease.
func (p *Pool) Create(t *Table, slot uint32, name string, size uint64) (*Volume, error) {
	return p.CreateWith(t, slot, name, size, CreateOpts{})
}

// CreateWith is Create with attributes.
func (p *Pool) CreateWith(t *Table, slot uint32, name string, size uint64, o CreateOpts) (*Volume, error) {
	if name == "" || len(name) >= NameLen {
		return nil, fmt.Errorf("invalid name %q", name)
	}
	if size == 0 {
		return nil, errors.New("size must be positive")
	}
	if t.Lookup(name) != nil {
		return nil, fmt.Errorf("volume %q already exists", name)
	}
	if slot >= p.Super.Slots || t.Vols[slot] != nil {
		return nil, fmt.Errorf("slot %d not free", slot)
	}
	need := (size + p.Super.ExtentSize - 1) / p.Super.ExtentSize
	runs, err := allocate(t.bitmap(p.Super.DataExtents), need)
	if err != nil {
		return nil, err
	}
	v := &Volume{Slot: uint32(slot), Gen: t.nextGen(uint32(slot)), State: StateActive, Name: name, Size: size, Created: time.Now().Unix(), Runs: runs,
		Format: o.Format, Creator: o.Creator}
	if o.Parent != nil {
		if t.Vols[o.Parent.Slot] == nil || t.Vols[o.Parent.Slot].Name != o.Parent.Name {
			return nil, fmt.Errorf("parent %q is not in the table", o.Parent.Name)
		}
		v.Parent = o.Parent.Slot + 1
	}
	if _, err := rand.Read(v.UUID[:]); err != nil {
		return nil, err
	}
	if err := p.writeRecord(t, v); err != nil {
		return nil, err
	}
	t.Vols[slot] = v
	return v, nil
}

// Children lists the active volumes backed by v.
func (t *Table) Children(v *Volume) []*Volume {
	var out []*Volume
	for _, c := range t.Vols {
		if c != nil && c.HasParent() && c.ParentSlot() == v.Slot {
			out = append(out, c)
		}
	}
	return out
}

// Rename rewrites a record under a new name.
func (p *Pool) Rename(t *Table, v *Volume, name string) (*Volume, error) {
	if name == "" || len(name) >= NameLen {
		return nil, fmt.Errorf("invalid name %q", name)
	}
	if t.Lookup(name) != nil {
		return nil, fmt.Errorf("volume %q already exists", name)
	}
	nv := *v
	nv.Runs = append([]Run(nil), v.Runs...)
	nv.Name = name
	nv.Gen = v.Gen + 1
	if err := p.writeRecord(t, &nv); err != nil {
		return nil, err
	}
	t.Vols[v.Slot] = &nv
	return &nv, nil
}

// Delete marks a slot free by writing a superseding free record. A volume
// that still backs others cannot go.
func (p *Pool) Delete(t *Table, v *Volume) error {
	if c := t.Children(v); len(c) > 0 {
		return fmt.Errorf("%s is the backing volume of %s", v.Name, c[0].Name)
	}
	free := &Volume{Slot: v.Slot, Gen: v.Gen + 1, State: StateFree, Name: v.Name, UUID: v.UUID}
	if err := p.writeRecord(t, free); err != nil {
		return err
	}
	t.Vols[v.Slot] = nil
	t.setFreeGen(v.Slot, free.Gen)
	return nil
}

// Grow extends a volume to newSize, appending runs as needed.
func (p *Pool) Grow(t *Table, v *Volume, newSize uint64) (*Volume, error) {
	if newSize <= v.Size {
		return nil, errors.New("shrinking is not supported")
	}
	have := v.Extents()
	need := (newSize + p.Super.ExtentSize - 1) / p.Super.ExtentSize
	nv := *v
	nv.Runs = append([]Run(nil), v.Runs...)
	if need > have {
		more, err := allocate(t.bitmap(p.Super.DataExtents), need-have)
		if err != nil {
			return nil, err
		}
		nv.Runs = append(nv.Runs, more...)
	}
	nv.Size = newSize
	nv.Gen = v.Gen + 1
	if err := p.writeRecord(t, &nv); err != nil {
		return nil, err
	}
	t.Vols[v.Slot] = &nv
	return &nv, nil
}

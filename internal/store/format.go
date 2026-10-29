// Package store implements the on-disk format of a nox shared block pool:
// a superblock, a sanlock lockspace, one paxos lease per volume slot, and a
// double-buffered volume table. There is no journal in v0: every record is
// one 4 KiB block protected by a CRC and a generation counter, written to
// the inactive copy of its slot, so a torn write never destroys the last
// good version.
package store

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"time"
)

const (
	Magic      = "NOXMOXSB"
	VolMagic   = "NOXVOL01"
	Version    = 1
	SBSize     = 4096
	RecSize    = 4096
	MiB        = 1 << 20
	SectorSize = 512
	MaxRuns    = 440 // (4096 - header - crc) / 8
	NameLen    = 64

	StateFree   = 0
	StateActive = 1

	defaultSlots      = 1024
	defaultExtentSize = 256 * MiB
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// Super is the superblock, stored at byte 0 and mirrored in the last 4 KiB.
type Super struct {
	UUID         [16]byte
	SectorSize   uint32
	ExtentSize   uint64
	Slots        uint32
	LockspaceOff uint64
	MetaLeaseOff uint64
	VolLeaseOff  uint64
	VolTableOff  uint64
	DataOff      uint64
	DataExtents  uint64
	DevSize      uint64
	Created      int64
}

// LockspaceName derives the sanlock lockspace name from the pool UUID.
func (s *Super) LockspaceName() string { return "nox-" + hex.EncodeToString(s.UUID[:6]) }

// UUIDString renders the pool UUID.
func (s *Super) UUIDString() string {
	u := s.UUID
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}

// SlotLeaseOff returns the byte offset of a volume slot's paxos lease.
func (s *Super) SlotLeaseOff(slot uint32) uint64 { return s.VolLeaseOff + uint64(slot)*MiB }

// RecOff returns the byte offset of copy c (0 or 1) of a slot's record.
func (s *Super) RecOff(slot uint32, c int) uint64 {
	return s.VolTableOff + uint64(slot)*2*RecSize + uint64(c)*RecSize
}

// TableSize is the byte length of the whole volume table.
func (s *Super) TableSize() uint64 { return uint64(s.Slots) * 2 * RecSize }

// Layout computes region offsets for a device of the given size.
func Layout(devSize uint64, extentSize uint64, slots uint32) (*Super, error) {
	if extentSize == 0 {
		extentSize = defaultExtentSize
	}
	if slots == 0 {
		slots = defaultSlots
	}
	if extentSize%MiB != 0 || extentSize < 16*MiB {
		return nil, errors.New("extent size must be a multiple of 1 MiB and at least 16 MiB")
	}
	s := &Super{SectorSize: SectorSize, ExtentSize: extentSize, Slots: slots, DevSize: devSize, Created: time.Now().Unix()}
	if _, err := rand.Read(s.UUID[:]); err != nil {
		return nil, err
	}
	s.LockspaceOff = 1 * MiB
	s.MetaLeaseOff = 2 * MiB
	s.VolLeaseOff = 3 * MiB
	s.VolTableOff = s.VolLeaseOff + uint64(slots)*MiB
	end := s.VolTableOff + s.TableSize()
	s.DataOff = (end + extentSize - 1) / extentSize * extentSize
	usable := devSize - SBSize // keep the mirror superblock out of the data area
	if usable <= s.DataOff {
		return nil, fmt.Errorf("device too small: %d bytes, metadata needs %d", devSize, s.DataOff)
	}
	s.DataExtents = (usable - s.DataOff) / extentSize
	if s.DataExtents == 0 {
		return nil, errors.New("device too small for a single extent")
	}
	return s, nil
}

func (s *Super) encode() []byte {
	b := make([]byte, SBSize)
	copy(b[0:8], Magic)
	le := binary.LittleEndian
	le.PutUint32(b[8:], Version)
	copy(b[16:32], s.UUID[:])
	le.PutUint32(b[32:], s.SectorSize)
	le.PutUint64(b[40:], s.ExtentSize)
	le.PutUint32(b[48:], s.Slots)
	le.PutUint64(b[56:], s.LockspaceOff)
	le.PutUint64(b[64:], s.MetaLeaseOff)
	le.PutUint64(b[72:], s.VolLeaseOff)
	le.PutUint64(b[80:], s.VolTableOff)
	le.PutUint64(b[88:], s.DataOff)
	le.PutUint64(b[96:], s.DataExtents)
	le.PutUint64(b[104:], s.DevSize)
	le.PutUint64(b[112:], uint64(s.Created))
	le.PutUint32(b[SBSize-4:], crc32.Checksum(b[:SBSize-4], castagnoli))
	return b
}

func decodeSuper(b []byte) (*Super, error) {
	if len(b) < SBSize || string(b[0:8]) != Magic {
		return nil, errors.New("bad superblock magic")
	}
	le := binary.LittleEndian
	if crc32.Checksum(b[:SBSize-4], castagnoli) != le.Uint32(b[SBSize-4:]) {
		return nil, errors.New("superblock crc mismatch")
	}
	if v := le.Uint32(b[8:]); v != Version {
		return nil, fmt.Errorf("unsupported format version %d", v)
	}
	s := &Super{}
	copy(s.UUID[:], b[16:32])
	s.SectorSize = le.Uint32(b[32:])
	s.ExtentSize = le.Uint64(b[40:])
	s.Slots = le.Uint32(b[48:])
	s.LockspaceOff = le.Uint64(b[56:])
	s.MetaLeaseOff = le.Uint64(b[64:])
	s.VolLeaseOff = le.Uint64(b[72:])
	s.VolTableOff = le.Uint64(b[80:])
	s.DataOff = le.Uint64(b[88:])
	s.DataExtents = le.Uint64(b[96:])
	s.DevSize = le.Uint64(b[104:])
	s.Created = int64(le.Uint64(b[112:]))
	return s, nil
}

// Run is a contiguous range of extents.
type Run struct {
	Start uint32 `json:"start"`
	Count uint32 `json:"count"`
}

// Volume is one slot's record.
type Volume struct {
	Slot    uint32   `json:"slot"`
	Gen     uint64   `json:"gen"`
	State   uint32   `json:"state"`
	Name    string   `json:"name"`
	UUID    [16]byte `json:"-"`
	Size    uint64   `json:"size"`
	Created int64    `json:"created"`
	Flags   uint32   `json:"flags"`
	Runs    []Run    `json:"runs"`
}

// Extents returns the number of extents the runs cover.
func (v *Volume) Extents() uint64 {
	var n uint64
	for _, r := range v.Runs {
		n += uint64(r.Count)
	}
	return n
}

func (v *Volume) encode() ([]byte, error) {
	if len(v.Runs) > MaxRuns {
		return nil, fmt.Errorf("too many extent runs: %d", len(v.Runs))
	}
	if len(v.Name) >= NameLen {
		return nil, fmt.Errorf("name too long: %q", v.Name)
	}
	b := make([]byte, RecSize)
	copy(b[0:8], VolMagic)
	le := binary.LittleEndian
	le.PutUint32(b[8:], v.Slot)
	le.PutUint64(b[16:], v.Gen)
	le.PutUint32(b[24:], v.State)
	le.PutUint32(b[28:], v.Flags)
	copy(b[32:32+NameLen], v.Name)
	copy(b[96:112], v.UUID[:])
	le.PutUint64(b[112:], v.Size)
	le.PutUint64(b[120:], uint64(v.Created))
	le.PutUint32(b[128:], uint32(len(v.Runs)))
	off := 136
	for _, r := range v.Runs {
		le.PutUint32(b[off:], r.Start)
		le.PutUint32(b[off+4:], r.Count)
		off += 8
	}
	le.PutUint32(b[RecSize-4:], crc32.Checksum(b[:RecSize-4], castagnoli))
	return b, nil
}

func decodeVolume(b []byte) (*Volume, bool) {
	if len(b) < RecSize || string(b[0:8]) != VolMagic {
		return nil, false
	}
	le := binary.LittleEndian
	if crc32.Checksum(b[:RecSize-4], castagnoli) != le.Uint32(b[RecSize-4:]) {
		return nil, false
	}
	v := &Volume{}
	v.Slot = le.Uint32(b[8:])
	v.Gen = le.Uint64(b[16:])
	v.State = le.Uint32(b[24:])
	v.Flags = le.Uint32(b[28:])
	v.Name = string(bytes.TrimRight(b[32:32+NameLen], "\x00"))
	copy(v.UUID[:], b[96:112])
	v.Size = le.Uint64(b[112:])
	v.Created = int64(le.Uint64(b[120:]))
	n := int(le.Uint32(b[128:]))
	if n > MaxRuns {
		return nil, false
	}
	off := 136
	for i := 0; i < n; i++ {
		v.Runs = append(v.Runs, Run{le.Uint32(b[off:]), le.Uint32(b[off+4:])})
		off += 8
	}
	return v, true
}

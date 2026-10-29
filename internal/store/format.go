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
	VolMagicV1 = "NOXVOL01"
	VolMagicV2 = "NOXVOL02"
	VolMagic   = VolMagicV2
	Version    = 2 // newest format this code writes
	MinVersion = 1 // oldest format this code reads and writes
	SBSize     = 4096
	RecSize    = 4096
	MiB        = 1 << 20
	SectorSize = 512
	MaxRunsV1  = 440 // (4096 - 136 - 4) / 8
	MaxRuns    = 491 // (4096 - 160 - 4) / 8
	NameLen    = 64

	StateFree   = 0
	StateActive = 1

	// Volume formats recorded in v2 records.
	FormatRaw   = 0
	FormatQcow2 = 1

	// Feature bits in a v2 superblock. A daemon that does not know a bit
	// must refuse the pool.
	FeatMailbox = 1 << 0

	// Mailbox geometry: one block per host id, written by senders under the
	// mailbox lease, polled by its owner.
	MailboxBlock     = 4096
	DefaultHosts     = 2000
	MinHosts         = 16
	DefaultIOTimeout = 10

	defaultSlots      = 1024
	defaultExtentSize = 256 * MiB
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// Super is the superblock, stored at byte 0 and mirrored in the last 4 KiB.
type Super struct {
	Version      uint32
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
	// v2 fields; zero on a v1 pool.
	Features        uint64
	MailboxLeaseOff uint64 // paxos lease serialising mailbox writes
	MailboxOff      uint64 // MaxHosts blocks of MailboxBlock bytes
	IOTimeout       uint32 // sanlock io_timeout every host must use
	MaxHosts        uint32 // host ids 1..MaxHosts
	Tool            string // version of the tool that formatted or upgraded
}

// HasMailbox reports whether the pool carries the on-disk request mailbox.
func (s *Super) HasMailbox() bool {
	return s.Version >= 2 && s.Features&FeatMailbox != 0 && s.MailboxOff != 0
}

// MailboxBlockOff is the byte offset of host id h's mailbox block.
func (s *Super) MailboxBlockOff(h uint64) uint64 { return s.MailboxOff + (h-1)*MailboxBlock }

// MailboxSize is the byte length of the mailbox region.
func (s *Super) MailboxSize() uint64 { return uint64(s.MaxHosts) * MailboxBlock }

// IOTimeoutOrDefault returns the pool's io_timeout, or the sanlock default
// for pools formatted before it was recorded.
func (s *Super) IOTimeoutOrDefault() int {
	if s.IOTimeout == 0 {
		return DefaultIOTimeout
	}
	return int(s.IOTimeout)
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

// Layout computes region offsets for a device of the given size, in the
// newest format.
func Layout(devSize uint64, extentSize uint64, slots uint32) (*Super, error) {
	return LayoutVersion(devSize, extentSize, slots, Version)
}

// LayoutVersion computes a layout in a specific format version. Version 1
// is only kept so the upgrade path can be tested against a pool that looks
// exactly like one formatted by the first release.
func LayoutVersion(devSize uint64, extentSize uint64, slots uint32, version uint32) (*Super, error) {
	if extentSize == 0 {
		extentSize = defaultExtentSize
	}
	if slots == 0 {
		slots = defaultSlots
	}
	if extentSize%MiB != 0 || extentSize < 16*MiB {
		return nil, errors.New("extent size must be a multiple of 1 MiB and at least 16 MiB")
	}
	if version < MinVersion || version > Version {
		return nil, fmt.Errorf("unsupported format version %d", version)
	}
	s := &Super{Version: version, SectorSize: SectorSize, ExtentSize: extentSize, Slots: slots, DevSize: devSize, Created: time.Now().Unix()}
	if _, err := rand.Read(s.UUID[:]); err != nil {
		return nil, err
	}
	s.LockspaceOff = 1 * MiB
	s.MetaLeaseOff = 2 * MiB
	s.VolLeaseOff = 3 * MiB
	s.VolTableOff = s.VolLeaseOff + uint64(slots)*MiB
	end := s.VolTableOff + s.TableSize()
	if version >= 2 {
		s.IOTimeout = DefaultIOTimeout
		s.MaxHosts = DefaultHosts
		s.Features = FeatMailbox
		s.MailboxLeaseOff = (end + MiB - 1) / MiB * MiB
		s.MailboxOff = s.MailboxLeaseOff + MiB
		end = s.MailboxOff + s.MailboxSize()
	}
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

// UpgradeLayout returns the v2 superblock for a v1 pool, placing the
// mailbox lease and region in the gap between the volume table and the
// first data extent. Nothing that exists moves. It fails when the gap is
// too small, which only happens with very small extents.
func (s *Super) UpgradeLayout() (*Super, error) {
	if s.Version >= 2 {
		return nil, errors.New("pool is already format v2")
	}
	n := *s
	n.Version = 2
	n.IOTimeout = DefaultIOTimeout
	n.MaxHosts = DefaultHosts
	n.Features = FeatMailbox
	end := s.VolTableOff + s.TableSize()
	n.MailboxLeaseOff = (end + MiB - 1) / MiB * MiB
	n.MailboxOff = n.MailboxLeaseOff + MiB
	if n.MailboxOff >= s.DataOff {
		return nil, fmt.Errorf("no room for the mailbox lease: table ends at %d, data starts at %d", end, s.DataOff)
	}
	// With small extents the gap may not hold a block for every possible
	// host id; shrink the host count to what fits. The daemon refuses host
	// ids beyond it.
	if fit := (s.DataOff - n.MailboxOff) / MailboxBlock; fit < uint64(n.MaxHosts) {
		if fit < MinHosts {
			return nil, fmt.Errorf("no room for the mailbox: %d bytes free before the data, need %d for %d hosts", s.DataOff-n.MailboxOff, MinHosts*MailboxBlock, MinHosts)
		}
		n.MaxHosts = uint32(fit)
	}
	return &n, nil
}

func (s *Super) encode() []byte {
	b := make([]byte, SBSize)
	copy(b[0:8], Magic)
	le := binary.LittleEndian
	v := s.Version
	if v == 0 {
		v = Version
	}
	le.PutUint32(b[8:], v)
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
	if v >= 2 {
		le.PutUint64(b[120:], s.Features)
		le.PutUint64(b[128:], s.MailboxLeaseOff)
		le.PutUint64(b[136:], s.MailboxOff)
		le.PutUint32(b[144:], s.IOTimeout)
		le.PutUint32(b[148:], s.MaxHosts)
		copy(b[152:168], s.Tool)
	}
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
	v := le.Uint32(b[8:])
	if v < MinVersion || v > Version {
		return nil, fmt.Errorf("unsupported format version %d (this build handles %d to %d)", v, MinVersion, Version)
	}
	s := &Super{Version: v}
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
	if v >= 2 {
		s.Features = le.Uint64(b[120:])
		s.MailboxLeaseOff = le.Uint64(b[128:])
		s.MailboxOff = le.Uint64(b[136:])
		s.IOTimeout = le.Uint32(b[144:])
		s.MaxHosts = le.Uint32(b[148:])
		s.Tool = string(bytes.TrimRight(b[152:168], "\x00"))
		if s.Features&^FeatMailbox != 0 {
			return nil, fmt.Errorf("pool uses unknown features %#x", s.Features&^FeatMailbox)
		}
		if s.MaxHosts == 0 || s.MaxHosts > DefaultHosts {
			return nil, fmt.Errorf("bad max hosts %d", s.MaxHosts)
		}
		if s.Features&FeatMailbox != 0 && (s.MailboxOff == 0 || s.MailboxOff+s.MailboxSize() > s.DataOff) {
			return nil, errors.New("mailbox region outside the metadata area")
		}
	}
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
	// v2 fields. Parent is the slot of the volume this one is backed by,
	// plus one, so 0 means none. Format is FormatRaw or FormatQcow2.
	// Creator is the host id that created the volume.
	Parent  uint32 `json:"parent"`
	Format  uint32 `json:"format"`
	Creator uint32 `json:"creator"`
	// VirtSize is the guest visible size of a qcow2 volume; Size is the
	// space reserved for it on the pool. Zero means equal to Size.
	VirtSize uint64 `json:"virt_size"`
}

// GuestSize is the size the guest sees.
func (v *Volume) GuestSize() uint64 {
	if v.VirtSize != 0 {
		return v.VirtSize
	}
	return v.Size
}

// HasParent reports whether the volume is backed by another volume.
func (v *Volume) HasParent() bool { return v.Parent != 0 }

// ParentSlot returns the backing volume's slot; only valid when HasParent.
func (v *Volume) ParentSlot() uint32 { return v.Parent - 1 }

// FormatName renders the volume format.
func (v *Volume) FormatName() string {
	if v.Format == FormatQcow2 {
		return "qcow2"
	}
	return "raw"
}

// Extents returns the number of extents the runs cover.
func (v *Volume) Extents() uint64 {
	var n uint64
	for _, r := range v.Runs {
		n += uint64(r.Count)
	}
	return n
}

// encode renders the record in the given pool format version. A v1 pool
// gets a v1 record so daemons from the first release keep reading it; the
// v2 only fields must then be zero.
func (v *Volume) encode(version uint32) ([]byte, error) {
	maxRuns, runsOff, magic := MaxRuns, 160, VolMagicV2
	if version < 2 {
		maxRuns, runsOff, magic = MaxRunsV1, 136, VolMagicV1
		if v.Parent != 0 || v.Format != FormatRaw {
			return nil, errors.New("backing volumes and qcow2 need a format v2 pool (nox upgrade)")
		}
	}
	if len(v.Runs) > maxRuns {
		return nil, fmt.Errorf("too many extent runs: %d", len(v.Runs))
	}
	if len(v.Name) >= NameLen {
		return nil, fmt.Errorf("name too long: %q", v.Name)
	}
	b := make([]byte, RecSize)
	copy(b[0:8], magic)
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
	if version >= 2 {
		le.PutUint32(b[136:], v.Parent)
		le.PutUint32(b[140:], v.Format)
		le.PutUint32(b[144:], v.Creator)
		le.PutUint64(b[152:], v.VirtSize)
	} else if v.VirtSize != 0 && v.VirtSize != v.Size {
		return nil, errors.New("virtual size needs a format v2 pool (nox upgrade)")
	}
	off := runsOff
	for _, r := range v.Runs {
		le.PutUint32(b[off:], r.Start)
		le.PutUint32(b[off+4:], r.Count)
		off += 8
	}
	le.PutUint32(b[RecSize-4:], crc32.Checksum(b[:RecSize-4], castagnoli))
	return b, nil
}

func decodeVolume(b []byte) (*Volume, bool) {
	if len(b) < RecSize {
		return nil, false
	}
	var maxRuns, runsOff int
	switch string(b[0:8]) {
	case VolMagicV1:
		maxRuns, runsOff = MaxRunsV1, 136
	case VolMagicV2:
		maxRuns, runsOff = MaxRuns, 160
	default:
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
	if n > maxRuns {
		return nil, false
	}
	if runsOff == 160 {
		v.Parent = le.Uint32(b[136:])
		v.Format = le.Uint32(b[140:])
		v.Creator = le.Uint32(b[144:])
		v.VirtSize = le.Uint64(b[152:])
	}
	off := runsOff
	for i := 0; i < n; i++ {
		v.Runs = append(v.Runs, Run{le.Uint32(b[off:]), le.Uint32(b[off+4:])})
		off += 8
	}
	return v, true
}

// Package replay proves the on-disk format is crash safe: every mutation is
// recorded through dm-log-writes, then the device is rebuilt one completed
// write at a time (and, inside each write, one sector at a time) and the
// pool must be internally consistent and equal to a state the test knows at
// every step.
package replay

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
)

// On-disk layout of a dm-log-writes log device, from the kernel's
// Documentation/admin-guide/device-mapper/log-writes.rst: a super block in
// sector 0 and then one entry per logged bio, each a header padded to one
// sector followed by the data sectors (or the mark string for marks).
const (
	logMagic   = "rhswfsj\x00" // the kernel writes the whole 8 byte string
	flagFlush  = 1 << 0
	flagFUA    = 1 << 1
	flagDiscrd = 1 << 2
	flagMark   = 1 << 3
	flagMeta   = 1 << 4
)

// Entry is one logged bio.
type Entry struct {
	Sector    uint64
	NrSectors uint64
	Flags     uint64
	Data      []byte // NrSectors*sectorsize bytes for a write
	Mark      string // for marks
}

// IsMark reports whether the entry is a user mark (dmsetup message ... mark).
func (e Entry) IsMark() bool { return e.Flags&flagMark != 0 }

// IsWrite reports whether the entry carries data to apply.
func (e Entry) IsWrite() bool { return !e.IsMark() && e.Flags&flagDiscrd == 0 && e.NrSectors > 0 }

// ReadLog parses a log device (or a file copy of it).
func ReadLog(path string) ([]Entry, uint32, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	hdr := make([]byte, 4096)
	if _, err := f.ReadAt(hdr, 0); err != nil {
		return nil, 0, err
	}
	le := binary.LittleEndian
	if string(hdr[0:8]) != logMagic {
		return nil, 0, errors.New("not a dm-log-writes log")
	}
	n := le.Uint64(hdr[16:])
	ss := le.Uint32(hdr[24:])
	if ss == 0 || ss > 65536 {
		return nil, 0, fmt.Errorf("bad sector size %d", ss)
	}
	var out []Entry
	off := int64(ss)
	eh := make([]byte, ss)
	for i := uint64(0); i < n; i++ {
		if _, err := f.ReadAt(eh, off); err != nil {
			return nil, 0, fmt.Errorf("entry %d header: %w", i, err)
		}
		e := Entry{Sector: le.Uint64(eh[0:]), NrSectors: le.Uint64(eh[8:]), Flags: le.Uint64(eh[16:])}
		dataLen := le.Uint64(eh[24:])
		off += int64(ss)
		switch {
		case e.IsMark():
			if dataLen > uint64(ss) {
				return nil, 0, fmt.Errorf("entry %d: mark too long", i)
			}
			// the mark string follows the 32 byte header inside the same sector
			e.Mark = string(trimNul(eh[32 : 32+dataLen]))
		case e.IsWrite():
			sz := e.NrSectors * uint64(ss)
			e.Data = make([]byte, sz)
			if _, err := f.ReadAt(e.Data, off); err != nil {
				return nil, 0, fmt.Errorf("entry %d data: %w", i, err)
			}
			off += int64(sz)
		}
		out = append(out, e)
	}
	return out, ss, nil
}

func trimNul(b []byte) []byte {
	for i, c := range b {
		if c == 0 {
			return b[:i]
		}
	}
	return b
}

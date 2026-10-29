package store

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"sort"
)

// The mailbox replaces the single-slot sanlock host event as the channel a
// migration target uses to ask the current holder of a volume to drop to a
// shared lease. Each host id owns one 4 KiB block that only it reads;
// senders rewrite it under the pool's mailbox lease, so a block is always a
// whole valid image of the pending requests for that host.
//
// Block layout: 8 byte magic, receiver host id, entry count, block
// sequence, 8 reserved bytes, then up to MailboxCap 32 byte entries and a
// CRC32C over everything before it.

const (
	mailMagic  = "NOXMBOX1"
	mailHdr    = 32
	mailEntry  = 32
	MailboxCap = (MailboxBlock - mailHdr - 4) / mailEntry

	// Mail ops.
	MailConvertShared      = 1 // Arg = slot: drop that volume to shared
	MailConvertSharedGuest = 2 // Arg = guest id: drop every volume of the guest
)

// MailEntry is one request to the block's owner.
//
// Hosts' clocks are not trusted: the receiver acts on every (Sender, Seq)
// it has not handled yet, and a sender withdraws its own requests once it
// is done with them. Posted is the sender's wall clock and is only used to
// discard entries that are absurdly old or far in the future (an hour), so
// a dead sender's leftovers eventually disappear.
type MailEntry struct {
	Sender uint32
	Op     uint32
	Arg    uint64
	Seq    uint64 // sender's own counter, so a repeat can be told from a new request
	Posted int64  // unix seconds on the sender's clock
}

// MailMaxAge bounds how far an entry's Posted may differ from a reader's
// clock before it is dropped as a leftover.
const MailMaxAge = 3600

func encodeMailbox(host uint64, seq uint64, es []MailEntry) ([]byte, error) {
	if len(es) > MailboxCap {
		return nil, fmt.Errorf("mailbox full: %d entries", len(es))
	}
	b := make([]byte, MailboxBlock)
	le := binary.LittleEndian
	copy(b[0:8], mailMagic)
	le.PutUint32(b[8:], uint32(host))
	le.PutUint32(b[12:], uint32(len(es)))
	le.PutUint64(b[16:], seq)
	off := mailHdr
	for _, e := range es {
		le.PutUint32(b[off:], e.Sender)
		le.PutUint32(b[off+4:], e.Op)
		le.PutUint64(b[off+8:], e.Arg)
		le.PutUint64(b[off+16:], e.Seq)
		le.PutUint64(b[off+24:], uint64(e.Posted))
		off += mailEntry
	}
	le.PutUint32(b[MailboxBlock-4:], crc32.Checksum(b[:MailboxBlock-4], castagnoli))
	return b, nil
}

// decodeMailbox returns the entries, the block sequence, and whether the
// block was valid. An all-zero block is valid and empty.
func decodeMailbox(b []byte, host uint64) ([]MailEntry, uint64, bool) {
	if len(b) < MailboxBlock {
		return nil, 0, false
	}
	if allZero(b[:MailboxBlock]) {
		return nil, 0, true
	}
	le := binary.LittleEndian
	if string(b[0:8]) != mailMagic || le.Uint32(b[8:]) != uint32(host) {
		return nil, 0, false
	}
	if crc32.Checksum(b[:MailboxBlock-4], castagnoli) != le.Uint32(b[MailboxBlock-4:]) {
		return nil, 0, false
	}
	n := int(le.Uint32(b[12:]))
	if n > MailboxCap {
		return nil, 0, false
	}
	seq := le.Uint64(b[16:])
	es := make([]MailEntry, 0, n)
	off := mailHdr
	for i := 0; i < n; i++ {
		es = append(es, MailEntry{le.Uint32(b[off:]), le.Uint32(b[off+4:]), le.Uint64(b[off+8:]), le.Uint64(b[off+16:]), int64(le.Uint64(b[off+24:]))})
		off += mailEntry
	}
	return es, seq, true
}

// ErrNoMailbox is returned on a pool formatted without the mailbox.
var ErrNoMailbox = errors.New("pool has no mailbox (format v1)")

func (p *Pool) mailCheck(host uint64) error {
	if !p.Super.HasMailbox() {
		return ErrNoMailbox
	}
	if host == 0 || host > uint64(p.Super.MaxHosts) {
		return fmt.Errorf("host id %d out of range", host)
	}
	return nil
}

// ReadMailbox returns the pending requests for host. The owner polls this
// without any lease: a block is rewritten whole, so a reader sees either
// the old or the new image.
func (p *Pool) ReadMailbox(host uint64) ([]MailEntry, uint64, error) {
	if err := p.mailCheck(host); err != nil {
		return nil, 0, err
	}
	b, err := p.Dev.ReadAt(p.Super.MailboxBlockOff(host), MailboxBlock)
	if err != nil {
		return nil, 0, err
	}
	es, seq, ok := decodeMailbox(b, host)
	if !ok {
		return nil, 0, fmt.Errorf("mailbox block of host %d is corrupt", host)
	}
	return es, seq, nil
}

func (p *Pool) writeMailbox(host uint64, seq uint64, es []MailEntry) error {
	b, err := encodeMailbox(host, seq, es)
	if err != nil {
		return err
	}
	return p.Dev.WriteAt(p.Super.MailboxBlockOff(host), b)
}

func stale(x MailEntry, now int64) bool {
	return x.Posted < now-MailMaxAge || x.Posted > now+MailMaxAge
}

// PostMail adds a request to host's block. The caller holds the mailbox
// lease. Leftovers older than MailMaxAge are dropped, and an older request
// from the same sender for the same op and argument is replaced. A corrupt
// block is replaced rather than preserved: its owner would ignore it
// anyway. When the block is full the oldest entries make room.
func (p *Pool) PostMail(host uint64, e MailEntry, now int64) error {
	if err := p.mailCheck(host); err != nil {
		return err
	}
	b, err := p.Dev.ReadAt(p.Super.MailboxBlockOff(host), MailboxBlock)
	if err != nil {
		return err
	}
	es, seq, _ := decodeMailbox(b, host)
	kept := es[:0]
	for _, x := range es {
		if stale(x, now) {
			continue
		}
		if x.Sender == e.Sender && x.Op == e.Op && x.Arg == e.Arg {
			continue
		}
		kept = append(kept, x)
	}
	kept = append(kept, e)
	sort.SliceStable(kept, func(i, j int) bool { return kept[i].Posted < kept[j].Posted })
	if len(kept) > MailboxCap {
		kept = kept[len(kept)-MailboxCap:]
	}
	return p.writeMailbox(host, seq+1, kept)
}

// ClearMail removes every request sender posted to host. The caller holds
// the mailbox lease.
func (p *Pool) ClearMail(host uint64, sender uint32, now int64) error {
	if err := p.mailCheck(host); err != nil {
		return err
	}
	b, err := p.Dev.ReadAt(p.Super.MailboxBlockOff(host), MailboxBlock)
	if err != nil {
		return err
	}
	es, seq, ok := decodeMailbox(b, host)
	if !ok || len(es) == 0 {
		return nil
	}
	kept := es[:0]
	for _, x := range es {
		if x.Sender != sender && !stale(x, now) {
			kept = append(kept, x)
		}
	}
	if len(kept) == len(es) {
		return nil
	}
	return p.writeMailbox(host, seq+1, kept)
}

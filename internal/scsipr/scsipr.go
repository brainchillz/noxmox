// Package scsipr drives SCSI-3 persistent reservations on a block device
// through the SG_IO ioctl, with no external tools. nox uses it as an
// optional fence layer where the target enforces PR: every node registers a
// key, the pool holds one Write Exclusive - Registrants Only reservation so
// any registrant may write, and a survivor preempts a dead node's key on
// takeover so the array itself rejects that node's writes, independent of
// the watchdog. Targets that do not implement PR (common with fileio NAS
// backends) make the primitives fail, and the caller falls back to the
// node-local fence.
package scsipr

/*
#include <sys/ioctl.h>
#include <scsi/sg.h>
#include <stdlib.h>
#include <string.h>

// one SG_IO round trip; returns 0 on an ioctl+status success, else a
// negative errno or a positive packed status. sense[0..31] is filled.
// returns packed status: (driver_status<<16)|(host_status<<8)|scsi_status,
// or -1 if the ioctl itself failed. sense[0..31] is filled on CHECK CONDITION.
static int nox_sgio(int fd, const unsigned char *cdb, int cdb_len, int dir,
                    unsigned char *data, int data_len, unsigned char *sense) {
    sg_io_hdr_t h;
    memset(&h, 0, sizeof(h));
    h.interface_id = 'S';
    h.cmd_len = cdb_len;
    h.mx_sb_len = 32;
    h.dxfer_direction = dir;              // -1 none, -2 to dev, -3 from dev
    h.dxfer_len = data_len;
    h.dxferp = data_len ? data : 0;
    h.cmdp = (unsigned char *)cdb;
    h.sbp = sense;
    h.timeout = 15000;                    // ms
    if (ioctl(fd, SG_IO, &h) < 0) return -1;
    return ((int)h.driver_status << 16) | ((int)h.host_status << 8) | (int)h.status;
}
*/
import "C"

import (
	"encoding/binary"
	"fmt"
	"os"
	"time"
	"unsafe"
)

// PR OUT service actions.
const (
	saRegisterIgnore = 0x06
	saReserve        = 0x01
	saRelease        = 0x02
	saPreempt        = 0x04
	saClear          = 0x03
)

// PR IN service actions.
const (
	saReadKeys        = 0x00
	saReadReservation = 0x01
)

// TypeWEARAll is Write Exclusive - All Registrants: every registered node
// may write, a node whose key is removed (preempted) may not, and reads stay
// open to all. That is nox's model: each node writes its own volumes on the
// shared LUN, and a fenced node is blocked at the array. Verified enforcing
// on a zvol-backed LIO target.
const TypeWEARAll = 0x07

const scopeLU = 0x00

// debugSense makes sgio errors include raw sense bytes (set via NOX_PR_DEBUG).
var debugSense = os.Getenv("NOX_PR_DEBUG") != ""

// Dev is an open handle for SG_IO on a block device.
type Dev struct {
	f     *os.File
	aptpl bool
}

// Open opens the device for SG_IO.
func Open(path string) (*Dev, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	return &Dev{f: f}, nil
}

// Close releases the handle.
func (d *Dev) Close() error { return d.f.Close() }

func (d *Dev) sgio(cdb []byte, dir C.int, data []byte) error {
	var sense [32]C.uchar
	var dp *C.uchar
	if len(data) > 0 {
		dp = (*C.uchar)(unsafe.Pointer(&data[0]))
	}
	// Retry on transient CHECK CONDITIONs the way the sg3-utils library
	// does: NOT READY and UNIT ATTENTION are raised by LIO targets (seen on
	// both fileio and zvol-backed LUNs) even when the PR operation itself
	// takes effect, and clear on a retry.
	var scsi, host, drv, key, asc, ascq byte
	for attempt := 0; attempt < 6; attempt++ {
		rv := C.nox_sgio(C.int(d.f.Fd()), (*C.uchar)(unsafe.Pointer(&cdb[0])), C.int(len(cdb)),
			dir, dp, C.int(len(data)), &sense[0])
		if rv == -1 {
			return fmt.Errorf("SG_IO ioctl failed")
		}
		scsi = byte(rv & 0xff)
		host = byte((rv >> 8) & 0xff)
		drv = byte((rv >> 16) & 0xff)
		if scsi == 0 && host == 0 {
			return nil
		}
		var b [32]byte
		for i := range b {
			b[i] = byte(sense[i])
		}
		key, asc, ascq = parseSense(b[:])
		if scsi == 0x02 && (key == 0x02 || key == 0x06) { // NOT READY / UNIT ATTENTION
			time.Sleep(200 * time.Millisecond)
			continue
		}
		break
	}
	if debugSense {
		return fmt.Errorf("scsi=%#x host=%#x drv=%#x (key=%#x asc=%#x ascq=%#x) after retries", scsi, host, drv, key, asc, ascq)
	}
	return fmt.Errorf("scsi status %#x (sense key=%#x asc=%#x ascq=%#x)", scsi, key, asc, ascq)
}

const (
	dirNone = C.int(-1)
	dirTo   = C.int(-2)
	dirFrom = C.int(-3)
)

// prOut sends a PERSISTENT RESERVE OUT with a 24-byte parameter list.
func (d *Dev) prOut(sa byte, prType byte, key, sark uint64, aptpl bool) error {
	cdb := make([]byte, 10)
	cdb[0] = 0x5f
	cdb[1] = sa & 0x1f
	cdb[2] = (scopeLU << 4) | (prType & 0x0f)
	binary.BigEndian.PutUint32(cdb[5:], 24)
	p := make([]byte, 24)
	binary.BigEndian.PutUint64(p[0:], key)
	binary.BigEndian.PutUint64(p[8:], sark)
	if aptpl {
		p[20] = 0x01 // APTPL: persist through power loss
	}
	return d.sgio(cdb, dirTo, p)
}

// prIn sends a PERSISTENT RESERVE IN and returns the parameter data.
func (d *Dev) prIn(sa byte, alloc int) ([]byte, error) {
	cdb := make([]byte, 10)
	cdb[0] = 0x5e
	cdb[1] = sa & 0x1f
	binary.BigEndian.PutUint16(cdb[7:], uint16(alloc))
	data := make([]byte, alloc)
	if err := d.sgio(cdb, dirFrom, data); err != nil {
		return nil, err
	}
	return data, nil
}

// parseSense returns the sense key, ASC and ASCQ for both fixed (0x70/0x71)
// and descriptor (0x72/0x73) sense formats.
func parseSense(s []byte) (key, asc, ascq byte) {
	if len(s) < 14 {
		return 0, 0, 0
	}
	switch s[0] & 0x7f {
	case 0x72, 0x73: // descriptor
		return s[1] & 0x0f, s[2], s[3]
	default: // fixed
		return s[2] & 0x0f, s[12], s[13]
	}
}

// APTPL reports whether the last successful Register used persist-through-
// power-loss. It is informational; fencing does not require APTPL.
func (d *Dev) APTPL() bool { return d.aptpl }

// Register registers key (register and ignore existing key), idempotent
// across restarts. LIO targets return a transient NOT READY on PR OUT even
// as the operation takes effect, so success is judged by reading the keys
// back, not by the command status. APTPL (persist through power loss) is
// requested but not required.
func (d *Dev) Register(key uint64) error {
	e1 := d.prOut(saRegisterIgnore, 0, 0, key, true)
	d.aptpl = e1 == nil
	if !d.registered(key) {
		// retry without APTPL, which some backstores need
		_ = d.prOut(saRegisterIgnore, 0, 0, key, false)
		d.aptpl = false
	}
	if d.registered(key) {
		return nil
	}
	if e1 != nil {
		return fmt.Errorf("register key %#x: %w", key, e1)
	}
	return fmt.Errorf("register key %#x: key not present after register", key)
}

func (d *Dev) registered(key uint64) bool {
	keys, err := d.Keys()
	if err != nil {
		return false
	}
	for _, k := range keys {
		if k == key {
			return true
		}
	}
	return false
}

// Unregister removes key (register and ignore with a zero new key).
func (d *Dev) Unregister(key uint64) error {
	return d.prOut(saRegisterIgnore, 0, key, 0, d.aptpl)
}

// Reserve creates the pool's Write Exclusive - All Registrants reservation,
// which activates enforcement. Harmless if one already exists. Judged by
// reading the reservation back.
func (d *Dev) Reserve(key uint64) error {
	err := d.prOut(saReserve, TypeWEARAll, key, 0, d.aptpl)
	if _, _, held, rerr := d.Reservation(); rerr == nil && held {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reserve: %w", err)
	}
	return fmt.Errorf("reserve: no reservation present afterward")
}

// Preempt removes victim's registration using this node's key, so the
// victim loses write access at the array. This is the fence. Judged by
// confirming the victim's key is gone.
func (d *Dev) Preempt(key, victim uint64) error {
	err := d.prOut(saPreempt, TypeWEARAll, key, victim, d.aptpl)
	if d.registered(victim) {
		if err != nil {
			return fmt.Errorf("preempt %#x: %w", victim, err)
		}
		return fmt.Errorf("preempt %#x: victim still registered", victim)
	}
	return nil
}

// Keys reads the registered reservation keys.
func (d *Dev) Keys() ([]uint64, error) {
	hdr, err := d.prIn(saReadKeys, 8)
	if err != nil {
		return nil, err
	}
	n := int(binary.BigEndian.Uint32(hdr[4:]))
	data, err := d.prIn(saReadKeys, 8+n)
	if err != nil {
		return nil, err
	}
	n = int(binary.BigEndian.Uint32(data[4:]))
	var keys []uint64
	for off := 8; off+8 <= 8+n && off+8 <= len(data); off += 8 {
		keys = append(keys, binary.BigEndian.Uint64(data[off:]))
	}
	return keys, nil
}

// Reservation reads the current reservation key and type, or (0,0,false)
// when there is none.
func (d *Dev) Reservation() (key uint64, prType byte, held bool, err error) {
	hdr, err := d.prIn(saReadReservation, 8)
	if err != nil {
		return 0, 0, false, err
	}
	n := int(binary.BigEndian.Uint32(hdr[4:]))
	if n < 16 {
		return 0, 0, false, nil // no reservation
	}
	data, err := d.prIn(saReadReservation, 8+n)
	if err != nil {
		return 0, 0, false, err
	}
	if int(binary.BigEndian.Uint32(data[4:])) < 16 {
		return 0, 0, false, nil
	}
	return binary.BigEndian.Uint64(data[8:]), data[21] & 0x0f, true, nil
}

// Supported reports whether the device answers PR IN READ KEYS, i.e. it
// implements persistent reservations at all. It does not prove enforcement;
// use the two-node self-test for that.
func (d *Dev) Supported() bool {
	_, err := d.prIn(saReadKeys, 8)
	return err == nil
}

// KeyForHost derives a stable 8-byte PR key for a sanlock host id.
func KeyForHost(hostID uint64) uint64 {
	return 0x6e6f78_0000_0000 | (hostID & 0xffffffff) // "nox" + host id
}

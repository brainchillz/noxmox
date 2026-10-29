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
    if (h.status != 0 || h.host_status != 0 || h.driver_status != 0)
        return ((int)h.status << 1) | 1;  // non-zero, distinguishes from clean 0
    return 0;
}
*/
import "C"

import (
	"encoding/binary"
	"fmt"
	"os"
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

// TypeWEAR is Write Exclusive - Registrants Only: any registered node may
// write, a node whose key is removed may not. This is the fence_scsi model.
const TypeWEAR = 0x05

const scopeLU = 0x00

// Dev is an open handle for SG_IO on a block device.
type Dev struct{ f *os.File }

// Open opens the device for SG_IO.
func Open(path string) (*Dev, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	return &Dev{f}, nil
}

// Close releases the handle.
func (d *Dev) Close() error { return d.f.Close() }

func (d *Dev) sgio(cdb []byte, dir C.int, data []byte) error {
	var sense [32]C.uchar
	var dp *C.uchar
	if len(data) > 0 {
		dp = (*C.uchar)(unsafe.Pointer(&data[0]))
	}
	rv := C.nox_sgio(C.int(d.f.Fd()), (*C.uchar)(unsafe.Pointer(&cdb[0])), C.int(len(cdb)),
		dir, dp, C.int(len(data)), &sense[0])
	if rv != 0 {
		return fmt.Errorf("sg_io failed (rv=%d, sense %02x %02x %02x)", int(rv), sense[2]&0x0f, sense[12], sense[13])
	}
	return nil
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

// Register registers key on the device (register and ignore existing key),
// so the call is idempotent across daemon restarts.
func (d *Dev) Register(key uint64) error {
	return d.prOut(saRegisterIgnore, 0, 0, key, true)
}

// Unregister removes key (register and ignore with a zero new key).
func (d *Dev) Unregister(key uint64) error {
	return d.prOut(saRegisterIgnore, 0, key, 0, true)
}

// Reserve creates the pool's Write Exclusive - Registrants Only reservation
// held by key. Harmless if a reservation already exists.
func (d *Dev) Reserve(key uint64) error {
	return d.prOut(saReserve, TypeWEAR, key, 0, true)
}

// Preempt removes victim's registration using this node's key, so the
// victim loses write access at the array. This is the fence.
func (d *Dev) Preempt(key, victim uint64) error {
	return d.prOut(saPreempt, TypeWEAR, key, victim, true)
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

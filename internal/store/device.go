package store

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// Device is an O_DIRECT handle on the shared LUN. Every host must bypass its
// page cache or it will read stale metadata written by its peers.
type Device struct {
	f    *os.File
	Path string
	Size uint64
	// Record, when set, sees every completed write in order. The crash
	// replay suite uses it where dm-log-writes is unavailable.
	Record func(off uint64, data []byte)
}

// OpenDevice opens the LUN for direct I/O.
func OpenDevice(path string) (*Device, error) {
	f, err := os.OpenFile(path, os.O_RDWR|syscall.O_DIRECT|syscall.O_DSYNC, 0)
	if err != nil {
		return nil, err
	}
	var sz uint64
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), 0x80081272 /* BLKGETSIZE64 */, uintptr(unsafe.Pointer(&sz))); e != 0 {
		st, err := f.Stat()
		if err != nil {
			f.Close()
			return nil, err
		}
		sz = uint64(st.Size()) // regular file (tests)
	}
	return &Device{f: f, Path: path, Size: sz}, nil
}

// Close releases the handle.
func (d *Device) Close() error { return d.f.Close() }

// alignedBuf returns a 4 KiB aligned buffer of n bytes (n multiple of 4096).
func alignedBuf(n int) []byte {
	raw := make([]byte, n+4096)
	off := 4096 - int(uintptr(unsafe.Pointer(&raw[0]))&4095)
	return raw[off : off+n]
}

// ReadAt reads n bytes at off (both 4 KiB aligned) bypassing the cache.
func (d *Device) ReadAt(off uint64, n int) ([]byte, error) {
	if off%4096 != 0 || n%4096 != 0 {
		return nil, fmt.Errorf("unaligned read off=%d n=%d", off, n)
	}
	buf := alignedBuf(n)
	done := 0
	for done < n {
		k, err := syscall.Pread(int(d.f.Fd()), buf[done:], int64(off)+int64(done))
		if err != nil {
			return nil, fmt.Errorf("pread %s@%d: %w", d.Path, off, err)
		}
		if k == 0 {
			return nil, errors.New("short read")
		}
		done += k
	}
	return buf, nil
}

// WriteAt writes an aligned block at off and waits for it to reach media.
func (d *Device) WriteAt(off uint64, b []byte) error {
	if off%4096 != 0 || len(b)%4096 != 0 {
		return fmt.Errorf("unaligned write off=%d n=%d", off, len(b))
	}
	buf := alignedBuf(len(b))
	copy(buf, b)
	done := 0
	for done < len(buf) {
		k, err := syscall.Pwrite(int(d.f.Fd()), buf[done:], int64(off)+int64(done))
		if err != nil {
			return fmt.Errorf("pwrite %s@%d: %w", d.Path, off, err)
		}
		done += k
	}
	if err := syscall.Fdatasync(int(d.f.Fd())); err != nil {
		return err
	}
	if d.Record != nil {
		d.Record(off, append([]byte(nil), b...))
	}
	return nil
}

// Zero writes zeros over [off, off+n).
func (d *Device) Zero(off uint64, n uint64) error {
	chunk := alignedBuf(1 * MiB)
	for n > 0 {
		k := uint64(len(chunk))
		if k > n {
			k = n
		}
		if err := d.WriteAt(off, chunk[:k]); err != nil {
			return err
		}
		off += k
		n -= k
	}
	return nil
}

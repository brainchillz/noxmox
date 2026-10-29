// Package fence stops the local guests that write a pool's volumes, so a
// node that has lost the LUN cannot corrupt it after a survivor takes over.
// It is used two ways: as the body of the `nox fence` sanlock killpath, and
// by noxd's own SIGTERM handler, which refuses to let sanlock clear the
// daemon while any guest still holds a pool volume (so the watchdog fences
// the host instead). It touches only /proc, device-mapper and a marker
// file, never the LUN, which by fence time is gone.
package fence

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func markerPath(pool string) string { return filepath.Join("/run", "noxd-"+pool+".fence") }

// Mark records that a fence is in progress for the pool.
func Mark(pool string) {
	_ = os.WriteFile(markerPath(pool), []byte(strconv.FormatInt(time.Now().Unix(), 10)), 0o600)
}

// Clear removes the fence marker.
func Clear(pool string) { _ = os.Remove(markerPath(pool)) }

// InProgress reports whether a fence was marked within ttl.
func InProgress(pool string, ttl time.Duration) bool {
	b, err := os.ReadFile(markerPath(pool))
	if err != nil {
		return false
	}
	ts, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return false
	}
	return time.Since(time.Unix(ts, 0)) < ttl
}

// Devices returns the device-mapper names with the pool's prefix mapped to
// their device numbers (the kernel st_rdev encoding).
func Devices(pool string) (map[uint64]string, error) {
	prefix := pool + "-"
	out := map[uint64]string{}
	ents, err := os.ReadDir("/sys/block")
	if err != nil {
		return nil, err
	}
	for _, e := range ents {
		if !strings.HasPrefix(e.Name(), "dm-") {
			continue
		}
		name, err := os.ReadFile(filepath.Join("/sys/block", e.Name(), "dm", "name"))
		if err != nil || !strings.HasPrefix(strings.TrimSpace(string(name)), prefix) {
			continue
		}
		devb, err := os.ReadFile(filepath.Join("/sys/block", e.Name(), "dev"))
		if err != nil {
			continue
		}
		var maj, min uint64
		if _, err := fmt.Sscanf(strings.TrimSpace(string(devb)), "%d:%d", &maj, &min); err != nil {
			continue
		}
		out[mkrdev(maj, min)] = strings.TrimSpace(string(name))
	}
	return out, nil
}

// mkrdev packs major/minor the way the kernel encodes st_rdev.
func mkrdev(maj, min uint64) uint64 {
	return ((maj & 0xfff) << 8) | (min & 0xff) | ((min &^ 0xff) << 12) | ((maj &^ 0xfff) << 32)
}

// Holders returns the pids with an open block device whose rdev is one of
// devs, found by open fd. The caller's own pid is skipped.
func Holders(devs map[uint64]string) map[int]bool {
	holders := map[int]bool{}
	if len(devs) == 0 {
		return holders
	}
	procs, err := os.ReadDir("/proc")
	if err != nil {
		return holders
	}
	self := os.Getpid()
	for _, p := range procs {
		pid, err := strconv.Atoi(p.Name())
		if err != nil || pid == self {
			continue
		}
		if holdsAny(pid, devs) {
			holders[pid] = true
		}
	}
	return holders
}

func holdsAny(pid int, devs map[uint64]string) bool {
	fdDir := fmt.Sprintf("/proc/%d/fd", pid)
	fds, err := os.ReadDir(fdDir)
	if err != nil {
		return false
	}
	for _, fd := range fds {
		var st syscall.Stat_t
		if err := syscall.Stat(filepath.Join(fdDir, fd.Name()), &st); err != nil {
			continue
		}
		if st.Mode&syscall.S_IFMT == syscall.S_IFBLK && devs[uint64(st.Rdev)] != "" {
			return true
		}
	}
	return false
}

// Name reads a process's comm.
func Name(pid int) string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return "?"
	}
	return strings.TrimSpace(string(b))
}

// Result reports what one fence pass did.
type Result struct {
	Devices   int
	Killed    []int
	Remaining []int // pids still holding a volume after the pass
}

// Cleared reports whether no process holds a pool volume any more.
func (r Result) Cleared() bool { return len(r.Remaining) == 0 }

// KillGuests marks the fence, SIGKILLs every process holding a pool volume,
// waits briefly, and reports who remains. It does not touch the LUN or the
// daemon. One idempotent pass, meant to be called repeatedly.
func KillGuests(pool string, log func(string, ...any)) Result {
	Mark(pool)
	devs, _ := Devices(pool)
	r := Result{Devices: len(devs)}
	for pid := range Holders(devs) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		r.Killed = append(r.Killed, pid)
		if log != nil {
			log("fence: SIGKILL %d (%s) holding a %s volume", pid, Name(pid), pool)
		}
	}
	if len(r.Killed) > 0 {
		time.Sleep(500 * time.Millisecond)
	}
	for pid := range Holders(devs) {
		r.Remaining = append(r.Remaining, pid)
	}
	return r
}

// Teardown removes the pool's device-mapper volumes once nothing holds them.
func Teardown(pool string) {
	devs, _ := Devices(pool)
	for _, name := range devs {
		_ = exec.Command("dmsetup", "remove", "--retry", name).Run()
	}
}

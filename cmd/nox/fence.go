package main

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

// runFence is the sanlock killpath for a pool. sanlock runs it when the
// host loses the lockspace, i.e. it can no longer reach the LUN, so this
// code must not touch the LUN. It stops every local process that still has
// one of the pool's device-mapper volumes open (the guests' QEMU, which do
// the actual I/O), tears the volumes down, and then stops the pool's daemon
// so its leases release and a survivor can take over safely.
//
// sanlock calls it about once a second until the holder pids clear or the
// watchdog fires, so each call is one idempotent pass:
//   - some guest still holds a pool volume -> SIGKILL it, exit non-zero,
//     leave the daemon and the watchdog alone (fence not complete yet).
//   - nothing holds a pool volume -> remove the volumes, stop the daemon,
//     exit zero (host is clean, guests are stopped, no reset needed).
//
// A guest wedged in uninterruptible I/O on the dead LUN cannot be reaped
// until that I/O errors out; until then this keeps returning non-zero and
// the watchdog remains the backstop.
func runFence(pool string, dryRun bool) int {
	if pool == "" {
		fmt.Fprintln(os.Stderr, "fence: --pool required")
		return 2
	}
	prefix := pool + "-"
	devs, err := poolDMDevices(prefix)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fence: listing %s devices: %v\n", pool, err)
		return 2
	}

	holders := processHolders(devs)
	if dryRun {
		fmt.Printf("pool %s: %d mapped volume(s)\n", pool, len(devs))
		for _, n := range devs {
			fmt.Printf("  volume %s\n", n)
		}
		if len(holders) == 0 {
			fmt.Println("no process holds a pool volume")
		}
		for pid := range holders {
			fmt.Printf("  would SIGKILL %d (%s)\n", pid, procName(pid))
		}
		if pid := daemonPID(pool); pid > 0 {
			fmt.Printf("  would then stop noxd pid %d\n", pid)
		}
		return 0
	}
	for pid := range holders {
		// SIGKILL is queued even for a process in uninterruptible sleep; it
		// takes effect when the dead-storage I/O finally errors out.
		_ = syscall.Kill(pid, syscall.SIGKILL)
		fmt.Fprintf(os.Stderr, "fence: SIGKILL %d (%s) holding a %s volume\n", pid, procName(pid), pool)
	}
	// Give just-signalled, non-wedged processes a moment to go.
	if len(holders) > 0 {
		time.Sleep(500 * time.Millisecond)
	}
	if left := processHolders(devs); len(left) > 0 {
		pids := make([]string, 0, len(left))
		for pid := range left {
			pids = append(pids, strconv.Itoa(pid))
		}
		fmt.Fprintf(os.Stderr, "fence: %s volumes still held by %s; fence incomplete, watchdog remains the backstop\n", pool, strings.Join(pids, ","))
		return 1
	}

	// Nothing holds the pool's volumes any more: tear them down and stop the
	// daemon so its leases release. dmsetup remove --retry rides out udev.
	for _, name := range devs {
		_ = exec.Command("dmsetup", "remove", "--retry", name).Run()
	}
	if pid := daemonPID(pool); pid > 0 {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		fmt.Fprintf(os.Stderr, "fence: stopped noxd for pool %s (pid %d); leases will expire for the survivor\n", pool, pid)
	}
	fmt.Fprintf(os.Stderr, "fence: pool %s fenced on this host\n", pool)
	return 0
}

// poolDMDevices returns the device-mapper names with the pool prefix mapped
// to their device numbers (major<<8|minor packed as the kernel st_rdev).
func poolDMDevices(prefix string) (map[uint64]string, error) {
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
		if err != nil {
			continue
		}
		if !strings.HasPrefix(strings.TrimSpace(string(name)), prefix) {
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

// processHolders scans every process for an open block device whose rdev is
// one of devs, by open fd or by memory mapping. Returns the set of pids.
func processHolders(devs map[uint64]string) map[int]bool {
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
		if procHoldsAny(pid, devs) {
			holders[pid] = true
		}
	}
	return holders
}

func procHoldsAny(pid int, devs map[uint64]string) bool {
	fdDir := fmt.Sprintf("/proc/%d/fd", pid)
	fds, err := os.ReadDir(fdDir)
	if err != nil {
		return false // process gone or not ours to see
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

func procName(pid int) string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return "?"
	}
	return strings.TrimSpace(string(b))
}

// daemonPID finds the running noxd for a pool by its command line, so the
// helper does not depend on systemctl (which can hang when the node is
// degraded).
func daemonPID(pool string) int {
	procs, _ := os.ReadDir("/proc")
	for _, p := range procs {
		pid, err := strconv.Atoi(p.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if err != nil {
			continue
		}
		args := strings.Split(string(b), "\x00")
		if len(args) < 2 || !strings.HasSuffix(args[0], "nox") || args[1] != "daemon" {
			continue
		}
		// match --pool <pool>, defaulting to "nox" when absent
		mp := "nox"
		for i := 2; i < len(args)-1; i++ {
			if args[i] == "--pool" {
				mp = args[i+1]
			}
		}
		if mp == pool {
			return pid
		}
	}
	return 0
}

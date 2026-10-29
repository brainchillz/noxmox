package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"noxmox/internal/fence"
)

// runFence is the sanlock killpath for a pool. sanlock runs it when the host
// loses the lockspace, so it must not touch the LUN. It SIGKILLs every local
// process holding one of the pool's volumes (the guests' QEMU, which do the
// I/O) and, once none remain, tears the volumes down. It does NOT stop the
// daemon: noxd decides for itself when it is safe to let go (see its SIGTERM
// handler), so that while a guest is still alive the daemon keeps holding
// its leases and the watchdog fences the host. sanlock calls this about once
// a second, so each call is one idempotent pass.
func runFence(pool string, dryRun bool) int {
	if pool == "" {
		fmt.Fprintln(os.Stderr, "fence: --pool required")
		return 2
	}
	if dryRun {
		devs, _ := fence.Devices(pool)
		fmt.Printf("pool %s: %d mapped volume(s)\n", pool, len(devs))
		for _, n := range devs {
			fmt.Printf("  volume %s\n", n)
		}
		h := fence.Holders(devs)
		if len(h) == 0 {
			fmt.Println("no process holds a pool volume")
		}
		for pid := range h {
			fmt.Printf("  would SIGKILL %d (%s)\n", pid, fence.Name(pid))
		}
		return 0
	}

	r := fence.KillGuests(pool, func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) })
	if !r.Cleared() {
		pids := make([]string, 0, len(r.Remaining))
		for _, pid := range r.Remaining {
			pids = append(pids, strconv.Itoa(pid))
		}
		fmt.Fprintf(os.Stderr, "fence: %s volumes still held by %s; the daemon keeps its leases and the watchdog is the backstop\n", pool, strings.Join(pids, ","))
		return 1
	}
	fence.Teardown(pool)
	fmt.Fprintf(os.Stderr, "fence: pool %s has no live writers; volumes torn down\n", pool)
	return 0
}

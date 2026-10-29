package main

import (
	"fmt"
	"os"

	"noxmox/internal/scsipr"
)

// runPRProbe reports whether the device implements SCSI persistent
// reservations, by exercising the primitives from this one node. It does
// not prove the target enforces them under concurrent access; that needs
// the two-node self-test. It leaves a test key registered only briefly.
func runPRProbe(device string, hostID uint64) int {
	if device == "" {
		fmt.Fprintln(os.Stderr, "pr-probe: --device required")
		return 2
	}
	d, err := scsipr.Open(device)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pr-probe: open: %v\n", err)
		return 2
	}
	defer d.Close()

	if !d.Supported() {
		fmt.Println("PR: NOT supported (PR IN READ KEYS failed) - the target does not implement persistent reservations; nox will use the node-local fence only")
		return 1
	}
	fmt.Println("PR: READ KEYS ok")
	key := scsipr.KeyForHost(hostID)
	if err := d.Register(key); err != nil {
		fmt.Printf("PR: register failed: %v\n", err)
		return 1
	}
	fmt.Printf("PR: registered key %#x\n", key)
	keys, err := d.Keys()
	if err != nil {
		fmt.Printf("PR: read keys after register failed: %v\n", err)
		return 1
	}
	found := false
	for _, k := range keys {
		if k == key {
			found = true
		}
	}
	fmt.Printf("PR: keys now registered: %d (ours present: %v)\n", len(keys), found)
	if err := d.Reserve(key); err != nil {
		fmt.Printf("PR: reserve failed: %v\n", err)
	} else {
		rk, rt, held, _ := d.Reservation()
		fmt.Printf("PR: reservation held=%v key=%#x type=%#x\n", held, rk, rt)
	}
	_ = d.Unregister(key)
	fmt.Println("PR: primitives work from this node (register/reserve/read). Run the two-node self-test to prove enforcement.")
	if !found {
		fmt.Println("PR: WARNING our key did not read back - the target may be faking PR; treat as unsupported")
		return 1
	}
	return 0
}

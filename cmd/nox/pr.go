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
	fmt.Printf("PR: registered key %#x (APTPL=%v)\n", key, d.APTPL())
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

// runPR is a small PR ops command for validation and oper: register,
// reserve, preempt, keys, clear. Standalone, no daemon.
func runPR(op, device string, hostID, victim uint64) int {
	if device == "" {
		fmt.Fprintln(os.Stderr, "pr: --device required")
		return 2
	}
	d, err := scsipr.Open(device)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pr: open: %v\n", err)
		return 2
	}
	defer d.Close()
	key := scsipr.KeyForHost(hostID)
	switch op {
	case "register":
		if err := d.Register(key); err != nil {
			fmt.Fprintf(os.Stderr, "pr register: %v\n", err)
			return 1
		}
		fmt.Printf("registered %#x\n", key)
	case "reserve":
		if err := d.Register(key); err != nil {
			fmt.Fprintf(os.Stderr, "pr reserve (register): %v\n", err)
			return 1
		}
		if err := d.Reserve(key); err != nil {
			fmt.Fprintf(os.Stderr, "pr reserve: %v\n", err)
			return 1
		}
		fmt.Printf("registered %#x and reserved\n", key)
	case "preempt":
		vk := scsipr.KeyForHost(victim)
		if err := d.Preempt(key, vk); err != nil {
			fmt.Fprintf(os.Stderr, "pr preempt: %v\n", err)
			return 1
		}
		fmt.Printf("preempted %#x (host %d)\n", vk, victim)
	case "keys":
		keys, err := d.Keys()
		if err != nil {
			fmt.Fprintf(os.Stderr, "pr keys: %v\n", err)
			return 1
		}
		for _, k := range keys {
			fmt.Printf("%#x\n", k)
		}
	case "clear":
		keys, _ := d.Keys()
		for _, k := range keys {
			_ = d.Unregister(k)
		}
		fmt.Println("cleared")
	default:
		fmt.Fprintf(os.Stderr, "pr: unknown op %q\n", op)
		return 2
	}
	return 0
}

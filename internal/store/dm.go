package store

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// DMName is the device-mapper name for a volume.
func DMName(name string) string { return "nox-" + name }

// DMPath is the block device path QEMU opens.
func DMPath(name string) string { return "/dev/mapper/" + DMName(name) }

// DMTable renders the dm-linear table for a volume.
func (p *Pool) DMTable(v *Volume) string {
	var sb strings.Builder
	var sector, remaining uint64
	remaining = (v.Size + SectorSize - 1) / SectorSize
	for _, r := range v.Runs {
		if remaining == 0 {
			break
		}
		off := (p.Super.DataOff + uint64(r.Start)*p.Super.ExtentSize) / SectorSize
		n := uint64(r.Count) * p.Super.ExtentSize / SectorSize
		if n > remaining {
			n = remaining
		}
		fmt.Fprintf(&sb, "%d %d linear %s %d\n", sector, n, p.Dev.Path, off)
		sector += n
		remaining -= n
	}
	return sb.String()
}

func dmsetup(stdin string, args ...string) error {
	cmd := exec.Command("dmsetup", args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("dmsetup %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(out.String()))
	}
	return nil
}

// DMExists reports whether the mapped device is present.
func DMExists(name string) bool {
	_, err := os.Stat(DMPath(name))
	return err == nil
}

// DMCreate loads the table and waits for the device node.
func (p *Pool) DMCreate(v *Volume) error {
	if DMExists(v.Name) {
		return nil
	}
	if err := dmsetup(p.DMTable(v), "create", DMName(v.Name)); err != nil {
		return err
	}
	for i := 0; i < 100; i++ {
		if DMExists(v.Name) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("%s did not appear", DMPath(v.Name))
}

// DMReload swaps in a new table for a live device (resize).
func (p *Pool) DMReload(v *Volume) error {
	n := DMName(v.Name)
	if err := dmsetup("", "suspend", n); err != nil {
		return err
	}
	if err := dmsetup(p.DMTable(v), "reload", n); err != nil {
		_ = dmsetup("", "resume", n)
		return err
	}
	return dmsetup("", "resume", n)
}

// DMRemove tears the device down. Fails if it is open.
func DMRemove(name string) error {
	if !DMExists(name) {
		return nil
	}
	return dmsetup("", "remove", DMName(name))
}

// DMList returns the names of all nox mapped devices on this host.
func DMList() ([]string, error) {
	out, err := exec.Command("dmsetup", "ls", "--target", "linear").Output()
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) > 0 && strings.HasPrefix(f[0], "nox-") {
			names = append(names, strings.TrimPrefix(f[0], "nox-"))
		}
	}
	return names, nil
}

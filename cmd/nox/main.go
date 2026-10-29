// nox is the CLI and daemon entry point for the shared block pool.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"noxmox/internal/daemon"
	"noxmox/internal/proto"
	"noxmox/internal/sanlock"
	"noxmox/internal/store"
)

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  nox format  --device DEV [--extent-size 256M] [--slots 1024] [--io-timeout 10] [--force]
  nox daemon  --device DEV --host-id N [--socket PATH]
  nox status | list                       [--socket PATH] [--json]
  nox info NAME                           [--socket PATH] [--json]
  nox create NAME SIZE                    [--socket PATH]
  nox delete NAME                         [--socket PATH]
  nox activate NAME [--shared] [--timeout SEC]
  nox deactivate NAME
  nox resize NAME SIZE
  nox convert NAME ex|sh`)
	os.Exit(2)
}

func parseSize(s string) (uint64, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	mult := uint64(1)
	for _, u := range []struct {
		suf string
		m   uint64
	}{{"TIB", 1 << 40}, {"GIB", 1 << 30}, {"MIB", 1 << 20}, {"KIB", 1 << 10}, {"T", 1 << 40}, {"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10}, {"B", 1}} {
		if strings.HasSuffix(s, u.suf) {
			s, mult = strings.TrimSuffix(s, u.suf), u.m
			break
		}
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("bad size %q", s)
	}
	return uint64(n * float64(mult)), nil
}

func human(n uint64) string {
	f := float64(n)
	for _, u := range []string{"B", "K", "M", "G", "T"} {
		if f < 1024 || u == "T" {
			return fmt.Sprintf("%.1f%s", f, u)
		}
		f /= 1024
	}
	return ""
}

func die(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	cmd, args := os.Args[1], os.Args[2:]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	device := fs.String("device", "", "shared block device")
	socket := fs.String("socket", proto.DefaultSocket, "daemon socket")
	hostID := fs.Uint64("host-id", 0, "sanlock host id (1..2000), unique per host")
	extent := fs.String("extent-size", "256M", "extent size")
	slots := fs.Uint("slots", 1024, "volume slots")
	ioTimeout := fs.Int("io-timeout", 10, "sanlock io timeout seconds")
	force := fs.Bool("force", false, "overwrite an existing pool")
	shared := fs.Bool("shared", false, "activate in shared mode (migration target)")
	timeout := fs.Int("timeout", 0, "seconds to wait for a lease")
	asJSON := fs.Bool("json", false, "raw JSON output")
	// allow flags after positionals
	var pos []string
	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "-") {
			_ = fs.Parse(args[i:])
			pos = append(pos, fs.Args()...)
			break
		}
		pos = append(pos, args[i])
	}

	call := func(req proto.Request, wait time.Duration) proto.Response {
		resp, err := proto.Call(*socket, req, wait)
		if err != nil {
			die(err)
		}
		return resp
	}
	printJSON := func(v any) {
		b, _ := json.MarshalIndent(v, "", "  ")
		fmt.Println(string(b))
	}

	switch cmd {
	case "format":
		if *device == "" {
			usage()
		}
		dev, err := store.OpenDevice(*device)
		if err != nil {
			die(err)
		}
		if _, err := store.Open(dev); err == nil && !*force {
			die(fmt.Errorf("%s already holds a nox pool; use --force to overwrite", *device))
		}
		es, err := parseSize(*extent)
		if err != nil {
			die(err)
		}
		p, err := store.Format(dev, es, uint32(*slots))
		if err != nil {
			die(err)
		}
		ls := sanlock.Lockspace{Name: p.Super.LockspaceName(), HostID: 1, Path: *device, Offset: p.Super.LockspaceOff}
		if err := sanlock.WriteLockspace(ls, 0, *ioTimeout); err != nil {
			die(fmt.Errorf("init lockspace (is the sanlock daemon running?): %w", err))
		}
		if err := sanlock.WriteResource(sanlock.Resource{Lockspace: ls.Name, Name: "meta", Path: *device, Offset: p.Super.MetaLeaseOff}); err != nil {
			die(err)
		}
		s := p.Super
		fmt.Printf("formatted %s\n  uuid        %s\n  lockspace   %s\n  extent size %s\n  slots       %d\n  data offset %d\n  capacity    %s (%d extents)\n",
			*device, s.UUIDString(), s.LockspaceName(), human(s.ExtentSize), s.Slots, s.DataOff, human(s.DataExtents*s.ExtentSize), s.DataExtents)
	case "daemon":
		if *device == "" || *hostID == 0 {
			usage()
		}
		die(daemon.Run(daemon.Config{Device: *device, HostID: *hostID, Socket: *socket, IOTimeout: *ioTimeout}))
	case "status":
		r := call(proto.Request{Op: "status"}, 30*time.Second)
		if *asJSON {
			printJSON(r)
			return
		}
		fmt.Printf("pool      %v\nlockspace %v (host id %v, ready %v)\ncapacity  %s total, %s used, %s free, %v volumes\n",
			r["uuid"], r["lockspace"], r["host_id"], r["ready"], human(uint64(r["total"].(float64))), human(uint64(r["used"].(float64))), human(uint64(r["free"].(float64))), r["volumes"])
		if hs, ok := r["hosts"].([]any); ok {
			for _, h := range hs {
				m := h.(map[string]any)
				fmt.Printf("host %v gen %v %v\n", m["host_id"], m["gen"], m["state"])
			}
		}
		if hl, ok := r["held"].([]any); ok {
			for _, h := range hl {
				m := h.(map[string]any)
				fmt.Printf("held %v (%v)\n", m["name"], m["mode"])
			}
		}
	case "list":
		r := call(proto.Request{Op: "list"}, 30*time.Second)
		if *asJSON {
			printJSON(r["volumes"])
			return
		}
		fmt.Printf("%-24s %5s %10s %6s %-4s %s\n", "NAME", "SLOT", "SIZE", "RUNS", "MODE", "PATH")
		for _, x := range r["volumes"].([]any) {
			m := x.(map[string]any)
			runs, _ := m["runs"].([]any)
			mode, _ := m["mode"].(string)
			path, _ := m["path"].(string)
			fmt.Printf("%-24v %5v %10s %6d %-4s %s\n", m["name"], m["slot"], human(uint64(m["size"].(float64))), len(runs), mode, path)
		}
	case "info":
		if len(pos) < 1 {
			usage()
		}
		printJSON(call(proto.Request{Op: "info", Name: pos[0]}, 30*time.Second)["volume"])
	case "create", "resize":
		if len(pos) < 2 {
			usage()
		}
		sz, err := parseSize(pos[1])
		if err != nil {
			die(err)
		}
		r := call(proto.Request{Op: cmd, Name: pos[0], Size: sz}, 60*time.Second)
		if *asJSON {
			printJSON(r["volume"])
		} else {
			fmt.Printf("%s %s %s\n", cmd+"d", pos[0], human(sz))
		}
	case "delete", "deactivate":
		if len(pos) < 1 {
			usage()
		}
		call(proto.Request{Op: cmd, Name: pos[0]}, 60*time.Second)
	case "activate":
		if len(pos) < 1 {
			usage()
		}
		mode := "ex"
		if *shared {
			mode = "sh"
		}
		r := call(proto.Request{Op: "activate", Name: pos[0], Mode: mode, Timeout: *timeout}, 10*time.Minute)
		fmt.Printf("%s %s\n", r["path"], r["mode"])
	case "convert":
		if len(pos) < 2 {
			usage()
		}
		r := call(proto.Request{Op: "convert", Name: pos[0], Mode: pos[1]}, 60*time.Second)
		fmt.Println(r["mode"])
	case "path":
		if len(pos) < 1 {
			usage()
		}
		fmt.Println(call(proto.Request{Op: "path", Name: pos[0]}, 10*time.Second)["path"])
	default:
		usage()
	}
}

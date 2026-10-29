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

// version is set at build time: go build -ldflags "-X main.version=v0.1.0".
var version = "dev"

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  nox format  --device DEV [--extent-size 256M] [--slots 1024] [--io-timeout 10] [--force]
  nox daemon  --device DEV --host-id N [--pool NAME] [--socket PATH]
  every other command takes [--pool NAME] or [--socket PATH]
  nox status | list [--json]
  nox metrics                             prometheus text format
  nox info NAME [--json]
  nox create NAME SIZE [--format qcow2] [--parent NAME] [--virt-size SIZE]
  nox delete NAME [--reparent]
  nox activate NAME [--shared] [--timeout SEC]
  nox deactivate NAME
  nox resize NAME SIZE
  nox convert NAME ex|sh
  nox rename NAME NEWNAME
  nox check [--full] [--json]             table consistency and lease holders
  nox repair                              drop stale devices and leases, restore damaged record copies
  nox upgrade                             move a format v1 pool to v2 (all volumes inactive)
  nox version`)
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
	if cmd == "version" {
		fmt.Println("nox", version)
		return
	}
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	device := fs.String("device", "", "shared block device")
	pool := fs.String("pool", proto.DefaultPool, "pool name (selects the socket and the dm name prefix)")
	socket := fs.String("socket", "", "daemon socket (default /run/noxd-<pool>.sock)")
	hostID := fs.Uint64("host-id", 0, "sanlock host id (1..2000), unique per host")
	extent := fs.String("extent-size", "256M", "extent size")
	slots := fs.Uint("slots", 1024, "volume slots")
	ioTimeout := fs.Int("io-timeout", 10, "sanlock io timeout seconds")
	force := fs.Bool("force", false, "overwrite an existing pool")
	shared := fs.Bool("shared", false, "activate in shared mode (migration target)")
	timeout := fs.Int("timeout", 0, "seconds to wait for a lease")
	asJSON := fs.Bool("json", false, "raw JSON output")
	full := fs.Bool("full", false, "check: read every free slot's lease too")
	format := fs.String("format", "raw", "create: raw or qcow2")
	parent := fs.String("parent", "", "create: backing volume")
	virtSize := fs.String("virt-size", "", "create/resize: guest visible size of a qcow2 volume")
	reparent := fs.Bool("reparent", false, "delete: chain member whose data was merged; re-point its children")
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

	if *socket == "" {
		*socket = proto.SocketPath(*pool)
		if _, err := os.Stat(*socket); err != nil && *pool == proto.DefaultPool {
			*socket = proto.DefaultSocket // a daemon from before pool names
		}
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
		p, err := store.FormatVersion(dev, es, uint32(*slots), store.Version, version)
		if err != nil {
			die(err)
		}
		if *ioTimeout != store.DefaultIOTimeout {
			p.Super.IOTimeout = uint32(*ioTimeout)
			if err := p.WriteSuper(p.Super); err != nil {
				die(err)
			}
		}
		ls := sanlock.Lockspace{Name: p.Super.LockspaceName(), HostID: 1, Path: *device, Offset: p.Super.LockspaceOff}
		if err := sanlock.WriteLockspace(ls, 0, *ioTimeout); err != nil {
			die(fmt.Errorf("init lockspace (is the sanlock daemon running?): %w", err))
		}
		if err := sanlock.WriteResource(sanlock.Resource{Lockspace: ls.Name, Name: "meta", Path: *device, Offset: p.Super.MetaLeaseOff}); err != nil {
			die(err)
		}
		if err := sanlock.WriteResource(sanlock.Resource{Lockspace: ls.Name, Name: "mbox", Path: *device, Offset: p.Super.MailboxLeaseOff}); err != nil {
			die(err)
		}
		s := p.Super
		fmt.Printf("formatted %s\n  format      v%d\n  uuid        %s\n  lockspace   %s (io_timeout %d, %d host ids)\n  extent size %s\n  slots       %d\n  data offset %d\n  capacity    %s (%d extents)\n",
			*device, s.Version, s.UUIDString(), s.LockspaceName(), s.IOTimeout, s.MaxHosts, human(s.ExtentSize), s.Slots, s.DataOff, human(s.DataExtents*s.ExtentSize), s.DataExtents)
	case "daemon":
		if *device == "" || *hostID == 0 {
			usage()
		}
		daemon.Version = version
		die(daemon.Run(daemon.Config{Device: *device, HostID: *hostID, Pool: *pool, Socket: *socket, IOTimeout: *ioTimeout}))
	case "status":
		r := call(proto.Request{Op: "status"}, 30*time.Second)
		if *asJSON {
			printJSON(r)
			return
		}
		fmt.Printf("nox       %s (daemon %v)\npool      %v %v (format v%v, mailbox %v, io_timeout %v)\nlockspace %v (host id %v, ready %v)\ncapacity  %s total, %s used, %s free, %v volumes\n", version, r["version"],
			r["pool"], r["uuid"], r["format"], r["mailbox"], r["io_timeout"], r["lockspace"], r["host_id"], r["ready"], human(uint64(r["total"].(float64))), human(uint64(r["used"].(float64))), human(uint64(r["free"].(float64))), r["volumes"])
		if hs, ok := r["hosts"].([]any); ok {
			for _, h := range hs {
				m := h.(map[string]any)
				fmt.Printf("host %v gen %v %v\n", m["host_id"], m["gen"], m["state"])
			}
		}
		if hl, ok := r["held"].([]any); ok {
			for _, h := range hl {
				m := h.(map[string]any)
				kind := m["mode"]
				if b, _ := m["backing"].(bool); b {
					kind = "sh, backing"
				}
				fmt.Printf("held %v (%v)\n", m["name"], kind)
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
		req := proto.Request{Op: cmd, Name: pos[0], Size: sz}
		if cmd == "create" {
			req.Format, req.Parent = *format, *parent
		}
		if *virtSize != "" {
			if req.VirtSize, err = parseSize(*virtSize); err != nil {
				die(err)
			}
		}
		r := call(req, 60*time.Second)
		if *asJSON {
			printJSON(r["volume"])
		} else {
			fmt.Printf("%s %s %s\n", cmd+"d", pos[0], human(sz))
		}
	case "delete", "deactivate":
		if len(pos) < 1 {
			usage()
		}
		call(proto.Request{Op: cmd, Name: pos[0], Reparent: *reparent}, 60*time.Second)
	case "rename":
		if len(pos) < 2 {
			usage()
		}
		call(proto.Request{Op: "rename", Name: pos[0], NewName: pos[1]}, 60*time.Second)
		fmt.Printf("renamed %s to %s\n", pos[0], pos[1])
	case "upgrade":
		r := call(proto.Request{Op: "upgrade"}, 10*time.Minute)
		fmt.Printf("pool is now format v%v (%v host ids)\n", r["format"], r["max_hosts"])
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
	case "metrics":
		fmt.Print(call(proto.Request{Op: "metrics"}, 30*time.Second)["text"])
	case "repair":
		r := call(proto.Request{Op: "repair"}, 10*time.Minute)
		acts, _ := r["actions"].([]any)
		for _, a := range acts {
			fmt.Println(a)
		}
		if len(acts) == 0 {
			fmt.Println("nothing to repair")
		}
	case "check":
		r := call(proto.Request{Op: "check", Full: *full}, 10*time.Minute)
		if *asJSON {
			printJSON(r)
		} else {
			for _, x := range r["volumes"].([]any) {
				m := x.(map[string]any)
				var hs []string
				if hl, ok := m["holders"].([]any); ok {
					for _, h := range hl {
						hm := h.(map[string]any)
						hs = append(hs, fmt.Sprintf("host %v", hm["host_id"]))
					}
				}
				mode := "ex"
				if sh, _ := m["shared"].(bool); sh {
					mode = "sh"
				}
				if len(hs) == 0 {
					fmt.Printf("%-24v slot %4v  free\n", m["name"], m["slot"])
				} else {
					fmt.Printf("%-24v slot %4v  %s %s\n", m["name"], m["slot"], mode, strings.Join(hs, ", "))
				}
			}
			probs, _ := r["problems"].([]any)
			for _, p := range probs {
				fmt.Println("PROBLEM:", p)
			}
			if len(probs) == 0 {
				fmt.Println("clean")
			}
		}
		if ok, _ := r["clean"].(bool); !ok {
			os.Exit(1)
		}
	default:
		usage()
	}
}

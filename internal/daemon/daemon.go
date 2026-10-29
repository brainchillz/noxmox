// Package daemon is noxd: one process per host per pool. It joins the sanlock
// lockspace, holds volume leases on behalf of running VMs (as orphan leases,
// so a daemon restart never drops them), loads device-mapper tables, and
// coordinates the shared-lease handoff used by live migration.
package daemon

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"noxmox/internal/proto"
	"noxmox/internal/sanlock"
	"noxmox/internal/store"
)

// Event codes carried through the lockspace.
const (
	EvConvertShared      = 0x4e4f5853 // "NOXS": please drop slot <data> to shared
	EvConvertSharedGuest = 0x4e4f5847 // "NOXG": please drop every vm-<data>-* volume to shared
)

// Version is the build version, set by the command.
var Version = "dev"

// Config for one daemon.
type Config struct {
	Device    string
	HostID    uint64
	Socket    string
	IOTimeout int
}

type held struct {
	vol         *store.Volume
	mode        int
	sharedUntil time.Time // no self-upgrade before this (migration window)
	backing     bool      // held shared because a chain on top of it is active here; never upgraded
}

// Daemon state.
type Daemon struct {
	cfg  Config
	pool *store.Pool
	ls   sanlock.Lockspace
	sock int

	mu       sync.Mutex // serializes all metadata and lease operations
	held     map[string]*held
	pending  map[string]bool             // activations in progress (lease wait runs unlocked)
	seen     map[sanlock.Event]time.Time // recently handled events (sanlock redelivers each renewal)
	mailSeen map[[2]uint64]time.Time     // (sender, seq) of handled mailbox requests
	mailSeq  uint64
	ready    atomic.Bool
	lg       *log.Logger
}

// Run starts the daemon and blocks until the listener fails.
func Run(cfg Config) error {
	lg := log.New(os.Stderr, "", log.LstdFlags|log.Lmsgprefix)
	dev, err := store.OpenDevice(cfg.Device)
	if err != nil {
		return err
	}
	pool, err := store.Open(dev)
	if err != nil {
		return fmt.Errorf("open pool: %w", err)
	}
	d := &Daemon{cfg: cfg, pool: pool, held: map[string]*held{}, pending: map[string]bool{}, seen: map[sanlock.Event]time.Time{}, mailSeen: map[[2]uint64]time.Time{}, lg: lg}
	d.ls = sanlock.Lockspace{Name: pool.Super.LockspaceName(), HostID: cfg.HostID, Path: cfg.Device, Offset: pool.Super.LockspaceOff}
	if pool.Super.Version >= 2 && cfg.HostID > uint64(pool.Super.MaxHosts) {
		return fmt.Errorf("host id %d exceeds the pool's %d host ids", cfg.HostID, pool.Super.MaxHosts)
	}
	if d.sock, err = sanlock.Register(); err != nil {
		return err
	}
	_ = sanlock.RestrictSigkill(d.sock)
	lg.Printf("pool %s (format v%d) on %s, host id %d, lockspace %s, io_timeout %d", pool.Super.UUIDString(), pool.Super.Version, cfg.Device, cfg.HostID, d.ls.Name, d.ioTimeout())

	_ = os.Remove(cfg.Socket)
	l, err := net.Listen("unix", cfg.Socket)
	if err != nil {
		return err
	}
	_ = os.Chmod(cfg.Socket, 0o600)

	go d.join()
	for {
		c, err := l.Accept()
		if err != nil {
			return err
		}
		go d.serve(c)
	}
}

// ioTimeout is the sanlock io_timeout for this pool: recorded in a v2
// superblock, otherwise whatever the daemon was started with.
func (d *Daemon) ioTimeout() int {
	if d.pool.Super.Version >= 2 && d.pool.Super.IOTimeout != 0 {
		return int(d.pool.Super.IOTimeout)
	}
	if d.cfg.IOTimeout > 0 {
		return d.cfg.IOTimeout
	}
	return store.DefaultIOTimeout
}

func (d *Daemon) join() {
	for {
		t0 := time.Now()
		err := sanlock.AddLockspaceTimeout(d.ls, d.ioTimeout())
		if err == nil {
			d.lg.Printf("joined lockspace in %s", time.Since(t0).Round(time.Second))
			break
		}
		d.lg.Printf("add_lockspace: %v (retrying)", err)
		time.Sleep(5 * time.Second)
	}
	if err := sanlock.SetUsedByOrphans(d.ls.Name); err != nil {
		d.lg.Printf("set used_by_orphans: %v", err)
	}
	d.adopt()
	d.ready.Store(true)
	go d.events()
	go d.mailbox()
	go d.upgrader()
}

func (d *Daemon) mboxRes() sanlock.Resource {
	return sanlock.Resource{Lockspace: d.ls.Name, Name: "mbox", Path: d.cfg.Device, Offset: d.pool.Super.MailboxLeaseOff}
}

// withMbox runs fn under the mailbox lease, which only serialises the
// read-modify-write of mailbox blocks and is held for milliseconds.
func (d *Daemon) withMbox(fn func() error) error {
	r := d.mboxRes()
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := sanlock.Acquire(d.sock, r, sanlock.Exclusive, false)
		if err == nil {
			break
		}
		if (!errors.Is(err, sanlock.ErrHeld) && !errors.Is(err, sanlock.ErrAgain)) || time.Now().After(deadline) {
			return fmt.Errorf("mailbox lease: %w", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	defer func() { _ = sanlock.Release(d.sock, r) }()
	return fn()
}

// post writes a request into host's mailbox block.
func (d *Daemon) post(host uint64, op uint32, arg uint64) error {
	d.mailSeq++
	now := time.Now().Unix()
	e := store.MailEntry{Sender: uint32(d.cfg.HostID), Op: op, Arg: arg, Seq: d.mailSeq, Deadline: now + 60}
	return d.withMbox(func() error { return d.pool.PostMail(host, e, now) })
}

// clearMail withdraws this host's requests from the given hosts' blocks.
func (d *Daemon) clearMail(hosts map[uint64]bool) {
	if len(hosts) == 0 || !d.pool.Super.HasMailbox() {
		return
	}
	now := time.Now().Unix()
	_ = d.withMbox(func() error {
		for h := range hosts {
			if err := d.pool.ClearMail(h, uint32(d.cfg.HostID), now); err != nil {
				d.lg.Printf("clear mail at host %d: %v", h, err)
			}
		}
		return nil
	})
}

// mailbox polls this host's block once a second and acts on new requests.
// It also re-reads the superblock now and then so a pool upgraded by
// another host gains its mailbox without a restart.
func (d *Daemon) mailbox() {
	tick := 0
	for {
		time.Sleep(time.Second)
		tick++
		if tick%10 == 0 {
			if changed, err := d.pool.ReloadSuper(); err == nil && changed {
				d.lg.Printf("superblock changed: format v%d, mailbox %v", d.pool.Super.Version, d.pool.Super.HasMailbox())
			}
		}
		if !d.pool.Super.HasMailbox() {
			continue
		}
		es, _, err := d.pool.ReadMailbox(d.cfg.HostID)
		if err != nil {
			if tick%60 == 0 {
				d.lg.Printf("mailbox: %v", err)
			}
			continue
		}
		now := time.Now()
		for k, t := range d.mailSeen {
			if now.Sub(t) > 5*time.Minute {
				delete(d.mailSeen, k)
			}
		}
		for _, e := range es {
			if e.Deadline <= now.Unix() {
				continue
			}
			k := [2]uint64{uint64(e.Sender), e.Seq}
			if _, dup := d.mailSeen[k]; dup {
				continue
			}
			d.mailSeen[k] = now
			d.shareRequest(uint64(e.Sender), e.Op, e.Arg)
		}
	}
}

func (d *Daemon) res(slot uint32) sanlock.Resource {
	return sanlock.Resource{Lockspace: d.ls.Name, Name: fmt.Sprintf("s%05d", slot), Path: d.cfg.Device, Offset: d.pool.Super.SlotLeaseOff(slot)}
}

func (d *Daemon) metaRes() sanlock.Resource {
	return sanlock.Resource{Lockspace: d.ls.Name, Name: "meta", Path: d.cfg.Device, Offset: d.pool.Super.MetaLeaseOff}
}

// adopt re-attaches leases for volumes whose dm devices survived a daemon
// restart.
func (d *Daemon) adopt() {
	d.mu.Lock()
	defer d.mu.Unlock()
	names, err := store.DMList()
	if err != nil {
		d.lg.Printf("dmsetup ls: %v", err)
		return
	}
	if len(names) == 0 {
		return
	}
	t, err := d.pool.ReadTable()
	if err != nil {
		d.lg.Printf("read table: %v", err)
		return
	}
	for _, n := range names {
		v := t.Lookup(n)
		if v == nil {
			d.lg.Printf("adopt: dm device for unknown volume %q left alone", n)
			continue
		}
		r := d.res(v.Slot)
		var lastErr error
		backing := len(t.Children(v)) > 0
		for attempt := 0; attempt < 10 && d.held[n] == nil; attempt++ {
			for _, mode := range []int{sanlock.Exclusive, sanlock.Shared} {
				if err := sanlock.Adopt(d.sock, r, mode); err == nil {
					d.held[n] = &held{vol: v, mode: mode, backing: backing}
					d.lg.Printf("adopted orphan lease for %s (%s)", n, modeName(mode))
					break
				} else if !errors.Is(err, sanlock.ErrNoOrphan) {
					lastErr = err
				}
			}
			if d.held[n] != nil {
				break
			}
			if err := sanlock.Acquire(d.sock, r, sanlock.Exclusive, true); err == nil {
				d.held[n] = &held{vol: v, mode: sanlock.Exclusive, backing: backing}
				d.lg.Printf("re-acquired lease for %s (was not orphaned)", n)
				break
			} else {
				lastErr = err
			}
			time.Sleep(time.Second)
		}
		if d.held[n] == nil {
			d.lg.Printf("WARNING: %s is mapped but no lease could be taken: %v", n, lastErr)
		}
	}
}

func modeName(m int) string {
	if m == sanlock.Shared {
		return "sh"
	}
	return "ex"
}

func parseMode(s string) (int, error) {
	switch s {
	case "", "ex", "exclusive":
		return sanlock.Exclusive, nil
	case "sh", "shared":
		return sanlock.Shared, nil
	}
	return 0, fmt.Errorf("bad mode %q", s)
}

// events receives lockspace mailbox events from peers.
func (d *Daemon) events() {
	for {
		fd, err := sanlock.RegEvent(d.ls.Name)
		if err != nil {
			d.lg.Printf("reg_event: %v", err)
			time.Sleep(5 * time.Second)
			continue
		}
		for {
			if !sanlock.WaitEvent(fd, 1000) {
				continue
			}
			ev, err := sanlock.GetEvent(fd)
			if err != nil {
				if errors.Is(err, sanlock.ErrAgain) {
					continue
				}
				d.lg.Printf("get_event: %v", err)
				break
			}
			d.handleEvent(ev)
		}
		sanlock.EndEvent(fd, d.ls.Name)
		time.Sleep(time.Second)
	}
}

// guestOf extracts the guest id from PVE-style names (vm-<id>-...), 0 if none.
func guestOf(name string) uint64 {
	var id uint64
	if n, _ := fmt.Sscanf(name, "vm-%d-", &id); n == 1 && strings.HasPrefix(name, "vm-") {
		return id
	}
	return 0
}

func (d *Daemon) handleEvent(ev sanlock.Event) {
	// sanlock keeps delivering the same event on every delta-lease renewal
	// until the sender clears it; act on each distinct request once.
	now := time.Now()
	for k, t := range d.seen {
		if now.Sub(t) > 90*time.Second {
			delete(d.seen, k)
		}
	}
	if _, dup := d.seen[ev]; dup {
		return
	}
	d.seen[ev] = now
	switch ev.Event {
	case EvConvertShared:
		d.shareRequest(ev.HostID, store.MailConvertShared, ev.Data)
	case EvConvertSharedGuest:
		d.shareRequest(ev.HostID, store.MailConvertSharedGuest, ev.Data)
	default:
		d.lg.Printf("event from host %d: unknown 0x%x data %d", ev.HostID, ev.Event, ev.Data)
	}
}

// shareRequest handles "please drop to shared" from a peer, whichever
// channel it arrived on: op and arg are mailbox ops (slot, or guest id).
func (d *Daemon) shareRequest(from uint64, op uint32, arg uint64) {
	var match func(h *held) bool
	switch op {
	case store.MailConvertShared:
		match = func(h *held) bool { return uint64(h.vol.Slot) == arg }
	case store.MailConvertSharedGuest:
		match = func(h *held) bool { return arg != 0 && guestOf(h.vol.Name) == arg }
	default:
		d.lg.Printf("request from host %d: unknown op %d arg %d", from, op, arg)
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for name, h := range d.held {
		if !match(h) {
			continue
		}
		n++
		if h.mode == sanlock.Shared {
			h.sharedUntil = time.Now().Add(90 * time.Second)
			continue
		}
		if err := sanlock.Convert(d.sock, d.res(h.vol.Slot), sanlock.Shared); err != nil {
			d.lg.Printf("host %d asked for shared on %s: convert failed: %v", from, name, err)
			continue
		}
		h.mode = sanlock.Shared
		h.sharedUntil = time.Now().Add(90 * time.Second)
		d.lg.Printf("host %d asked for shared on %s: converted", from, name)
	}
	if n == 0 {
		d.lg.Printf("host %d asked for shared (op %d arg %d): nothing matching held here", from, op, arg)
	}
}

// upgrader opportunistically converts shared leases back to exclusive once
// the peer has released, which is how a migration target ends up exclusive.
func (d *Daemon) upgrader() {
	for {
		time.Sleep(2 * time.Second)
		d.mu.Lock()
		for name, h := range d.held {
			if h.mode != sanlock.Shared || h.backing || time.Now().Before(h.sharedUntil) {
				continue
			}
			if err := sanlock.Convert(d.sock, d.res(h.vol.Slot), sanlock.Exclusive); err == nil {
				h.mode = sanlock.Exclusive
				d.lg.Printf("%s upgraded to exclusive", name)
			} else if !errors.Is(err, sanlock.ErrHeld) && !errors.Is(err, sanlock.ErrAgain) {
				d.lg.Printf("%s upgrade: %v", name, err)
			}
		}
		d.mu.Unlock()
	}
}

// withMeta runs fn under the pool's metadata lease.
func (d *Daemon) withMeta(fn func(t *store.Table) error) error {
	r := d.metaRes()
	deadline := time.Now().Add(20 * time.Second)
	for {
		err := sanlock.Acquire(d.sock, r, sanlock.Exclusive, false)
		if err == nil {
			break
		}
		if (!errors.Is(err, sanlock.ErrHeld) && !errors.Is(err, sanlock.ErrAgain)) || time.Now().After(deadline) {
			return fmt.Errorf("metadata lease: %w", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
	defer func() {
		if err := sanlock.Release(d.sock, r); err != nil {
			d.lg.Printf("release meta: %v", err)
		}
	}()
	if changed, err := d.pool.ReloadSuper(); err != nil {
		return fmt.Errorf("superblock: %w", err)
	} else if changed {
		d.lg.Printf("superblock changed: format v%d", d.pool.Super.Version)
	}
	t, err := d.pool.ReadTable()
	if err != nil {
		return err
	}
	return fn(t)
}

func (d *Daemon) serve(c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Minute))
	line, err := bufio.NewReader(c).ReadBytes('\n')
	if err != nil {
		return
	}
	var req proto.Request
	resp := proto.Response{}
	if err := json.Unmarshal(line, &req); err != nil {
		resp["ok"], resp["error"] = false, "bad request: "+err.Error()
	} else if err := d.dispatch(req, resp); err != nil {
		resp["ok"], resp["error"] = false, err.Error()
		d.lg.Printf("%s %s: %v", req.Op, req.Name, err)
	} else {
		resp["ok"] = true
	}
	b, _ := json.Marshal(resp)
	_, _ = c.Write(append(b, '\n'))
}

func (d *Daemon) dispatch(req proto.Request, resp proto.Response) error {
	if req.Op == "status" {
		return d.status(resp)
	}
	if !d.ready.Load() {
		return errors.New("lockspace not joined yet")
	}
	switch req.Op {
	case "list":
		return d.list(resp)
	case "info":
		return d.info(req.Name, resp)
	case "create":
		return d.create(req, resp)
	case "rename":
		return d.rename(req.Name, req.NewName, resp)
	case "upgrade":
		return d.upgrade(resp)
	case "delete":
		return d.delete(req.Name)
	case "activate":
		return d.activate(req, resp)
	case "deactivate":
		return d.deactivate(req.Name)
	case "resize":
		return d.resize(req.Name, req.Size, resp)
	case "convert":
		return d.convert(req.Name, req.Mode, resp)
	case "prepare":
		return d.prepare(req.Names)
	case "path":
		resp["path"] = store.DMPath(req.Name)
		return nil
	case "check":
		return d.check(req.Full, resp)
	}
	return fmt.Errorf("unknown op %q", req.Op)
}

func (d *Daemon) heldInfo() []map[string]any {
	var out []map[string]any
	for n, h := range d.held {
		out = append(out, map[string]any{"name": n, "slot": h.vol.Slot, "mode": modeName(h.mode), "backing": h.backing})
	}
	return out
}

func (d *Daemon) status(resp proto.Response) error {
	s := d.pool.Super
	resp["device"] = d.cfg.Device
	resp["uuid"] = s.UUIDString()
	resp["lockspace"] = d.ls.Name
	resp["host_id"] = d.cfg.HostID
	resp["ready"] = d.ready.Load()
	resp["extent_size"] = s.ExtentSize
	resp["slots"] = s.Slots
	resp["format"] = s.Version
	resp["mailbox"] = s.HasMailbox()
	resp["io_timeout"] = d.ioTimeout()
	resp["max_hosts"] = s.MaxHosts
	total := s.DataExtents * s.ExtentSize
	resp["total"] = total
	t, err := d.pool.ReadTable()
	if err != nil {
		return err
	}
	free := t.FreeExtents(s.DataExtents) * s.ExtentSize
	resp["free"] = free
	resp["used"] = total - free
	resp["volumes"] = len(t.Active())
	d.mu.Lock()
	resp["held"] = d.heldInfo()
	d.mu.Unlock()
	if d.ready.Load() {
		if hosts, err := sanlock.Hosts(d.ls.Name, 0); err == nil {
			var hs []map[string]any
			for _, h := range hosts {
				hs = append(hs, map[string]any{"host_id": h.HostID, "gen": h.Generation, "state": sanlock.HostStateName(sanlock.HostState(h))})
			}
			resp["hosts"] = hs
		}
	}
	return nil
}

func (d *Daemon) volInfo(v *store.Volume) map[string]any {
	m := map[string]any{"name": v.Name, "slot": v.Slot, "size": v.Size, "created": v.Created, "extents": v.Extents(), "runs": v.Runs, "gen": v.Gen, "format": v.FormatName(), "creator": v.Creator}
	if v.HasParent() {
		m["parent_slot"] = v.ParentSlot()
	}
	if h, ok := d.held[v.Name]; ok {
		m["mode"] = modeName(h.mode)
		m["path"] = store.DMPath(v.Name)
	}
	return m
}

func (d *Daemon) list(resp proto.Response) error {
	t, err := d.pool.ReadTable()
	if err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	vols := []map[string]any{}
	for _, v := range t.Active() {
		vols = append(vols, d.volInfo(v))
	}
	resp["volumes"] = vols
	return nil
}

func (d *Daemon) info(name string, resp proto.Response) error {
	t, err := d.pool.ReadTable()
	if err != nil {
		return err
	}
	v := t.Lookup(name)
	if v == nil {
		return store.ErrNotFound
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	resp["volume"] = d.volInfo(v)
	return nil
}

func (d *Daemon) create(req proto.Request, resp proto.Response) error {
	name, size := req.Name, req.Size
	opts := store.CreateOpts{Creator: uint32(d.cfg.HostID)}
	switch req.Format {
	case "", "raw":
	case "qcow2":
		opts.Format = store.FormatQcow2
	default:
		return fmt.Errorf("unknown format %q", req.Format)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.withMeta(func(t *store.Table) error {
		if t.Lookup(name) != nil {
			return fmt.Errorf("volume %q already exists", name)
		}
		if req.Parent != "" {
			opts.Parent = t.Lookup(req.Parent)
			if opts.Parent == nil {
				return fmt.Errorf("parent %q: %w", req.Parent, store.ErrNotFound)
			}
		}
		// Pick a free slot whose lease nobody holds: a host may briefly hold
		// the lease of a slot that was just freed under it, and the lease
		// is re-initialised below.
		slot, err := t.FreeSlot()
		for err == nil {
			owners, _, oerr := sanlock.Owners(d.res(slot))
			if oerr != nil || len(owners) == 0 {
				break
			}
			d.lg.Printf("slot %d is free but its lease is held by host %d, skipping", slot, owners[0].HostID)
			slot, err = t.FreeSlotAfter(slot)
		}
		if err != nil {
			return err
		}
		// Initialize the slot's lease before the record exists so a crash
		// in between leaves a free slot, never a volume without a lease.
		if err := sanlock.WriteResource(d.res(slot)); err != nil {
			return fmt.Errorf("init lease for slot %d: %w", slot, err)
		}
		v, err := d.pool.CreateWith(t, slot, name, size, opts)
		if err != nil {
			return err
		}
		resp["volume"] = d.volInfo(v)
		d.lg.Printf("created %s slot %d size %d %s runs %v", name, slot, size, v.FormatName(), v.Runs)
		return nil
	})
}

// rename rewrites the record and, when the volume is active here, renames
// the dm device so an open handle (a running guest) keeps working.
func (d *Daemon) rename(name, newName string, resp proto.Response) error {
	if newName == "" {
		return errors.New("new name required")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	h, isHeld := d.held[name]
	if isHeld && h.mode != sanlock.Exclusive {
		return errors.New("cannot rename during migration")
	}
	return d.withMeta(func(t *store.Table) error {
		v := t.Lookup(name)
		if v == nil {
			return store.ErrNotFound
		}
		if !isHeld {
			r := d.res(v.Slot)
			if err := sanlock.Acquire(d.sock, r, sanlock.Exclusive, false); err != nil {
				return fmt.Errorf("volume in use elsewhere: %w", err)
			}
			defer func() { _ = sanlock.Release(d.sock, r) }()
		}
		nv, err := d.pool.Rename(t, v, newName)
		if err != nil {
			return err
		}
		if isHeld {
			if store.DMExists(name) {
				if err := store.DMRename(name, newName); err != nil {
					return err
				}
			}
			h.vol = nv
			delete(d.held, name)
			d.held[newName] = h
		}
		resp["volume"] = d.volInfo(nv)
		d.lg.Printf("renamed %s to %s", name, newName)
		return nil
	})
}

// upgrade moves a v1 pool to v2 in place. Offline: every volume must be
// inactive everywhere, proved by taking each lease exclusively.
func (d *Daemon) upgrade(resp proto.Response) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.held) > 0 {
		return errors.New("volumes are active on this host")
	}
	return d.withMeta(func(t *store.Table) error {
		if d.pool.Super.Version >= 2 {
			resp["format"] = d.pool.Super.Version
			return errors.New("pool is already format v2")
		}
		var taken []sanlock.Resource
		defer func() {
			for _, r := range taken {
				_ = sanlock.Release(d.sock, r)
			}
		}()
		for _, v := range t.Active() {
			r := d.res(v.Slot)
			if err := sanlock.Acquire(d.sock, r, sanlock.Exclusive, false); err != nil {
				return fmt.Errorf("%s is in use: %w", v.Name, err)
			}
			taken = append(taken, r)
		}
		n, err := d.pool.PrepareUpgrade(Version)
		if err != nil {
			return err
		}
		mbox := sanlock.Resource{Lockspace: d.ls.Name, Name: "mbox", Path: d.cfg.Device, Offset: n.MailboxLeaseOff}
		if err := sanlock.WriteResource(mbox); err != nil {
			return fmt.Errorf("init mailbox lease: %w", err)
		}
		if err := d.pool.WriteSuper(n); err != nil {
			return err
		}
		d.lg.Printf("upgraded pool to format v2: mailbox at %d, %d host ids, io_timeout %d", n.MailboxOff, n.MaxHosts, n.IOTimeout)
		resp["format"] = n.Version
		resp["max_hosts"] = n.MaxHosts
		return nil
	})
}

func (d *Daemon) delete(name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if store.DMExists(name) {
		return fmt.Errorf("%s is active on this host", name)
	}
	if h, ok := d.held[name]; ok {
		_ = sanlock.Release(d.sock, d.res(h.vol.Slot))
		delete(d.held, name)
	}
	t, err := d.pool.ReadTable()
	if err != nil {
		return err
	}
	v := t.Lookup(name)
	if v == nil {
		return store.ErrNotFound
	}
	r := d.res(v.Slot)
	// Taking the volume lease exclusively proves no host is using it.
	if err := sanlock.Acquire(d.sock, r, sanlock.Exclusive, false); err != nil {
		if errors.Is(err, sanlock.ErrHeld) {
			if owners, _, oerr := sanlock.Owners(r); oerr == nil && len(owners) > 0 {
				return fmt.Errorf("%s is in use by host %d", name, owners[0].HostID)
			}
			return fmt.Errorf("%s is in use", name)
		}
		return err
	}
	defer func() { _ = sanlock.Release(d.sock, r) }()
	return d.withMeta(func(t *store.Table) error {
		v := t.Lookup(name)
		if v == nil {
			return store.ErrNotFound
		}
		if err := d.pool.Delete(t, v); err != nil {
			return err
		}
		d.lg.Printf("deleted %s slot %d", name, v.Slot)
		return nil
	})
}

func (d *Daemon) activate(req proto.Request, resp proto.Response) error {
	mode, err := parseMode(req.Mode)
	if err != nil {
		return err
	}
	timeout := time.Duration(req.Timeout) * time.Second
	if timeout == 0 {
		timeout = 120 * time.Second
	}
	d.mu.Lock()
	if h, ok := d.held[req.Name]; ok {
		defer d.mu.Unlock()
		if h.backing {
			// a backing volume activated in its own right (qemu-img on a
			// snapshot, a clone being made from a base): it stays shared
			resp["path"], resp["mode"] = store.DMPath(req.Name), modeName(h.mode)
			return nil
		}
		if mode == sanlock.Exclusive && h.mode == sanlock.Shared {
			// Try, but a failure just leaves it shared; the upgrader will
			// finish the job when the peer lets go.
			if err := sanlock.Convert(d.sock, d.res(h.vol.Slot), sanlock.Exclusive); err == nil {
				h.mode = sanlock.Exclusive
			}
		}
		if err := d.pool.DMCreate(h.vol); err != nil {
			return err
		}
		if err := d.activateChain(h.vol); err != nil {
			return err
		}
		resp["path"], resp["mode"] = store.DMPath(req.Name), modeName(h.mode)
		return nil
	}
	if d.pending[req.Name] {
		d.mu.Unlock()
		return fmt.Errorf("%s: activation already in progress", req.Name)
	}
	d.pending[req.Name] = true
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.pending, req.Name)
		d.mu.Unlock()
	}()

	t, err := d.pool.ReadTable()
	if err != nil {
		return err
	}
	v := t.Lookup(req.Name)
	if v == nil {
		return store.ErrNotFound
	}
	if len(t.Children(v)) > 0 {
		// A volume with children is a backing volume: it is only ever
		// opened read-only, by qemu through a chain or by qemu-img, so it
		// is taken shared regardless of what was asked.
		mode = sanlock.Shared
	}
	r := d.res(v.Slot)
	deadline := time.Now().Add(timeout)
	lastAsk := time.Time{}
	asked := map[uint64]bool{}
	t0 := time.Now()
	// The lease wait runs without the daemon lock so status, list and
	// unrelated activations keep working during a migration handoff.
	for {
		err = sanlock.Acquire(d.sock, r, mode, true)
		if err == nil {
			break
		}
		if !errors.Is(err, sanlock.ErrHeld) && !errors.Is(err, sanlock.ErrAgain) {
			return err
		}
		if mode == sanlock.Shared && time.Since(lastAsk) > 15*time.Second {
			d.askShare(v, asked)
			lastAsk = time.Now()
		}
		if time.Now().After(deadline) {
			d.withdraw(asked)
			if owners, _, oerr := sanlock.Owners(r); oerr == nil && len(owners) > 0 {
				return fmt.Errorf("%s: lease held by host %d after %s: %v", req.Name, owners[0].HostID, timeout, err)
			}
			return fmt.Errorf("%s: %w", req.Name, err)
		}
		time.Sleep(time.Second)
	}
	d.withdraw(asked)
	// The record may have changed while we waited for the lease: a delete
	// on another host takes the lease, frees the slot and releases it, and
	// we would now hold the lease of a free slot. Re-read and compare.
	t, err = d.pool.ReadTable()
	if err != nil {
		_ = sanlock.Release(d.sock, r)
		return err
	}
	if cur := t.Lookup(req.Name); cur == nil || cur.UUID != v.UUID || cur.Slot != v.Slot {
		_ = sanlock.Release(d.sock, r)
		return fmt.Errorf("%s: %w (deleted while waiting for its lease)", req.Name, store.ErrNotFound)
	} else {
		v = cur
	}
	// No grace on the acquiring side: an early upgrade attempt simply fails
	// while the peer still holds its shared lease.
	h := &held{vol: v, mode: mode, backing: len(t.Children(v)) > 0}
	if err := d.pool.DMCreate(v); err != nil {
		_ = sanlock.Release(d.sock, r)
		return err
	}
	d.mu.Lock()
	d.held[req.Name] = h
	err = d.activateChain(v)
	d.mu.Unlock()
	if err != nil {
		return err
	}
	d.lg.Printf("activated %s %s in %s", req.Name, modeName(mode), time.Since(t0).Round(time.Millisecond))
	resp["path"], resp["mode"] = store.DMPath(req.Name), modeName(mode)
	return nil
}

// activateChain makes every backing volume beneath v available: taken
// shared (a backing volume is read-only for everyone, so any number of
// hosts may hold it) and mapped. A backing volume this host still holds
// exclusively, which happens right after a snapshot renamed the old top
// underneath a running guest, is converted to shared. Caller holds d.mu.
func (d *Daemon) activateChain(v *store.Volume) error {
	if !v.HasParent() {
		return nil
	}
	t, err := d.pool.ReadTable()
	if err != nil {
		return err
	}
	seen := map[uint32]bool{v.Slot: true}
	for cur := v; cur.HasParent(); {
		ps := cur.ParentSlot()
		if seen[ps] || ps >= d.pool.Super.Slots || t.Vols[ps] == nil {
			return fmt.Errorf("%s: broken backing chain at slot %d", v.Name, ps)
		}
		seen[ps] = true
		p := t.Vols[ps]
		if h, ok := d.held[p.Name]; ok {
			if h.mode == sanlock.Exclusive {
				if err := sanlock.Convert(d.sock, d.res(p.Slot), sanlock.Shared); err != nil {
					return fmt.Errorf("%s: convert backing %s to shared: %w", v.Name, p.Name, err)
				}
				h.mode = sanlock.Shared
			}
			h.backing = true
		} else {
			if err := sanlock.Acquire(d.sock, d.res(p.Slot), sanlock.Shared, true); err != nil {
				return fmt.Errorf("%s: acquire backing %s shared: %w", v.Name, p.Name, err)
			}
			d.held[p.Name] = &held{vol: p, mode: sanlock.Shared, backing: true}
		}
		if err := d.pool.DMCreate(p); err != nil {
			return err
		}
		cur = p
	}
	return nil
}

// releaseUnusedBacking drops backing volumes that no held top still needs.
// Caller holds d.mu.
func (d *Daemon) releaseUnusedBacking() {
	t, err := d.pool.ReadTable()
	if err != nil {
		return
	}
	needed := map[uint32]bool{}
	for _, h := range d.held {
		if h.backing {
			continue
		}
		for cur := h.vol; cur.HasParent(); {
			ps := cur.ParentSlot()
			if needed[ps] || ps >= d.pool.Super.Slots || t.Vols[ps] == nil {
				break
			}
			needed[ps] = true
			cur = t.Vols[ps]
		}
	}
	for name, h := range d.held {
		if !h.backing || needed[h.vol.Slot] {
			continue
		}
		if err := store.DMRemove(name); err != nil {
			d.lg.Printf("backing %s still open, keeping it: %v", name, err)
			continue
		}
		if err := sanlock.Release(d.sock, d.res(h.vol.Slot)); err != nil {
			d.lg.Printf("release backing %s: %v", name, err)
		}
		delete(d.held, name)
		d.lg.Printf("released backing %s", name)
	}
}

// withdraw retracts share requests once they are no longer needed, so
// peers stop seeing them.
func (d *Daemon) withdraw(asked map[uint64]bool) {
	if len(asked) == 0 {
		return
	}
	if d.pool.Super.HasMailbox() {
		d.clearMail(asked)
		return
	}
	if err := sanlock.ClearEvent(d.ls.Name); err != nil {
		d.lg.Printf("clear_event: %v", err)
	}
}

// askShare asks whoever holds the lease exclusively to drop to shared,
// through the mailbox when the pool has one, else a sanlock host event.
// Hosts asked are recorded in asked so the request can be withdrawn.
func (d *Daemon) askShare(v *store.Volume, asked map[uint64]bool) bool {
	owners, shared, err := sanlock.Owners(d.res(v.Slot))
	if err != nil || shared {
		return false
	}
	sent := false
	for _, o := range owners {
		if o.HostID == d.cfg.HostID {
			continue
		}
		if d.pool.Super.HasMailbox() {
			if err := d.post(o.HostID, store.MailConvertShared, uint64(v.Slot)); err != nil {
				d.lg.Printf("mail to host %d: %v", o.HostID, err)
				continue
			}
		} else {
			ev := sanlock.Event{HostID: o.HostID, Generation: o.Generation, Event: EvConvertShared, Data: uint64(v.Slot)}
			if err := sanlock.SetEvent(d.ls.Name, ev); err != nil {
				d.lg.Printf("set_event to host %d: %v", o.HostID, err)
				continue
			}
		}
		asked[o.HostID] = true
		d.lg.Printf("asked host %d to share %s", o.HostID, v.Name)
		sent = true
	}
	return sent
}

// prepare fires share requests for several volumes at once. sanlock carries
// one event per host per renewal cycle, so volumes that belong to the same
// guest are folded into a single guest-wide event per holding host; anything
// else gets a per-slot event and may take an extra cycle.
func (d *Daemon) prepare(names []string) error {
	t, err := d.pool.ReadTable()
	if err != nil {
		return err
	}
	d.mu.Lock()
	heldHere := map[string]bool{}
	for n := range d.held {
		heldHere[n] = true
	}
	d.mu.Unlock()
	asked := map[uint64]bool{}
	sentGuest := map[[2]uint64]bool{} // (holder host, guest) already asked
	for _, n := range names {
		if heldHere[n] {
			continue
		}
		v := t.Lookup(n)
		if v == nil {
			continue
		}
		guest := guestOf(n)
		if guest == 0 || d.pool.Super.HasMailbox() {
			// with a mailbox every volume gets its own request; they all
			// land in one block and are handled in one poll
			d.askShare(v, asked)
			continue
		}
		owners, shared, err := sanlock.Owners(d.res(v.Slot))
		if err != nil || shared {
			continue
		}
		for _, o := range owners {
			if o.HostID == d.cfg.HostID || sentGuest[[2]uint64{o.HostID, guest}] {
				continue
			}
			ev := sanlock.Event{HostID: o.HostID, Generation: o.Generation, Event: EvConvertSharedGuest, Data: guest}
			if err := sanlock.SetEvent(d.ls.Name, ev); err != nil {
				d.lg.Printf("set_event to host %d: %v", o.HostID, err)
				continue
			}
			sentGuest[[2]uint64{o.HostID, guest}] = true
			d.lg.Printf("asked host %d to share all volumes of guest %d", o.HostID, guest)
		}
	}
	return nil
}

func (d *Daemon) deactivate(name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	h, ok := d.held[name]
	if ok && h.backing {
		// Backing volumes go when the last top above them goes.
		d.releaseUnusedBacking()
		return nil
	}
	if err := store.DMRemove(name); err != nil {
		return err
	}
	if !ok {
		return nil
	}
	if err := sanlock.Release(d.sock, d.res(h.vol.Slot)); err != nil {
		return err
	}
	delete(d.held, name)
	d.lg.Printf("deactivated %s", name)
	d.releaseUnusedBacking()
	return nil
}

func (d *Daemon) resize(name string, size uint64, resp proto.Response) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	h, isHeld := d.held[name]
	if isHeld && h.backing {
		return errors.New("cannot resize a backing volume")
	}
	if isHeld && h.mode != sanlock.Exclusive {
		return errors.New("cannot resize during migration")
	}
	return d.withMeta(func(t *store.Table) error {
		v := t.Lookup(name)
		if v == nil {
			return store.ErrNotFound
		}
		if !isHeld {
			// Not active here: make sure nobody else has it open either.
			r := d.res(v.Slot)
			if err := sanlock.Acquire(d.sock, r, sanlock.Exclusive, false); err != nil {
				return fmt.Errorf("volume in use elsewhere: %w", err)
			}
			defer func() { _ = sanlock.Release(d.sock, r) }()
		}
		nv, err := d.pool.Grow(t, v, size)
		if err != nil {
			return err
		}
		if isHeld {
			h.vol = nv
			if store.DMExists(name) {
				if err := d.pool.DMReload(nv); err != nil {
					return err
				}
			}
		}
		resp["volume"] = d.volInfo(nv)
		d.lg.Printf("resized %s to %d", name, size)
		return nil
	})
}

func (d *Daemon) convert(name, modeStr string, resp proto.Response) error {
	mode, err := parseMode(modeStr)
	if err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	h, ok := d.held[name]
	if !ok {
		return fmt.Errorf("%s not held", name)
	}
	if err := sanlock.Convert(d.sock, d.res(h.vol.Slot), mode); err != nil {
		return err
	}
	h.mode = mode
	if mode == sanlock.Shared {
		h.sharedUntil = time.Now().Add(90 * time.Second)
	}
	resp["mode"] = modeName(mode)
	return nil
}

// check verifies the volume table and reports who holds each lease. With
// full set it also reads the lease of every free slot to find orphaned
// holders, which costs one lease read per slot.
func (d *Daemon) check(full bool, resp proto.Response) error {
	probs, err := d.pool.Check()
	if err != nil {
		return err
	}
	problems := []string{}
	for _, p := range probs {
		problems = append(problems, p.String())
	}
	t, err := d.pool.ReadTable()
	if err != nil {
		return err
	}
	owners := func(slot uint32) ([]map[string]any, bool) {
		os, shared, err := sanlock.Owners(d.res(slot))
		if err != nil {
			return nil, false
		}
		var out []map[string]any
		for _, o := range os {
			out = append(out, map[string]any{"host_id": o.HostID, "gen": o.Generation})
		}
		return out, shared
	}
	vols := []map[string]any{}
	for _, v := range t.Active() {
		hs, shared := owners(v.Slot)
		m := map[string]any{"name": v.Name, "slot": v.Slot, "holders": hs, "shared": shared}
		if len(hs) > 1 && !shared {
			problems = append(problems, fmt.Sprintf("slot %d: %s has %d exclusive holders", v.Slot, v.Name, len(hs)))
		}
		vols = append(vols, m)
	}
	resp["volumes"] = vols
	if full {
		orphans := []map[string]any{}
		for slot := uint32(0); slot < d.pool.Super.Slots; slot++ {
			if t.Vols[slot] != nil {
				continue
			}
			if hs, _ := owners(slot); len(hs) > 0 {
				orphans = append(orphans, map[string]any{"slot": slot, "holders": hs})
				problems = append(problems, fmt.Sprintf("slot %d: free but lease held by %v", slot, hs))
			}
		}
		resp["orphans"] = orphans
	}
	resp["problems"] = problems
	resp["clean"] = len(problems) == 0
	return nil
}

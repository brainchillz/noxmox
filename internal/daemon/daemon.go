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
}

// Daemon state.
type Daemon struct {
	cfg  Config
	pool *store.Pool
	ls   sanlock.Lockspace
	sock int

	mu    sync.Mutex // serializes all metadata and lease operations
	held  map[string]*held
	seen  map[sanlock.Event]time.Time // recently handled events (sanlock redelivers each renewal)
	ready atomic.Bool
	lg    *log.Logger
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
	d := &Daemon{cfg: cfg, pool: pool, held: map[string]*held{}, seen: map[sanlock.Event]time.Time{}, lg: lg}
	d.ls = sanlock.Lockspace{Name: pool.Super.LockspaceName(), HostID: cfg.HostID, Path: cfg.Device, Offset: pool.Super.LockspaceOff}
	if d.sock, err = sanlock.Register(); err != nil {
		return err
	}
	_ = sanlock.RestrictSigkill(d.sock)
	lg.Printf("pool %s on %s, host id %d, lockspace %s", pool.Super.UUIDString(), cfg.Device, cfg.HostID, d.ls.Name)

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

func (d *Daemon) join() {
	for {
		t0 := time.Now()
		err := sanlock.AddLockspace(d.ls)
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
	go d.upgrader()
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
		for attempt := 0; attempt < 10 && d.held[n] == nil; attempt++ {
			for _, mode := range []int{sanlock.Exclusive, sanlock.Shared} {
				if err := sanlock.Adopt(d.sock, r, mode); err == nil {
					d.held[n] = &held{vol: v, mode: mode}
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
				d.held[n] = &held{vol: v, mode: sanlock.Exclusive}
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
	var match func(h *held) bool
	switch ev.Event {
	case EvConvertShared:
		match = func(h *held) bool { return uint64(h.vol.Slot) == ev.Data }
	case EvConvertSharedGuest:
		match = func(h *held) bool { return ev.Data != 0 && guestOf(h.vol.Name) == ev.Data }
	default:
		d.lg.Printf("event from host %d: unknown 0x%x data %d", ev.HostID, ev.Event, ev.Data)
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
			d.lg.Printf("host %d asked for shared on %s: convert failed: %v", ev.HostID, name, err)
			continue
		}
		h.mode = sanlock.Shared
		h.sharedUntil = time.Now().Add(90 * time.Second)
		d.lg.Printf("host %d asked for shared on %s: converted", ev.HostID, name)
	}
	if n == 0 {
		d.lg.Printf("host %d asked for shared (0x%x data %d): nothing matching held here", ev.HostID, ev.Event, ev.Data)
	}
}

// upgrader opportunistically converts shared leases back to exclusive once
// the peer has released, which is how a migration target ends up exclusive.
func (d *Daemon) upgrader() {
	for {
		time.Sleep(2 * time.Second)
		d.mu.Lock()
		for name, h := range d.held {
			if h.mode != sanlock.Shared || time.Now().Before(h.sharedUntil) {
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
		return d.create(req.Name, req.Size, resp)
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
	}
	return fmt.Errorf("unknown op %q", req.Op)
}

func (d *Daemon) heldInfo() []map[string]any {
	var out []map[string]any
	for n, h := range d.held {
		out = append(out, map[string]any{"name": n, "slot": h.vol.Slot, "mode": modeName(h.mode)})
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
	m := map[string]any{"name": v.Name, "slot": v.Slot, "size": v.Size, "created": v.Created, "extents": v.Extents(), "runs": v.Runs, "gen": v.Gen}
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

func (d *Daemon) create(name string, size uint64, resp proto.Response) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.withMeta(func(t *store.Table) error {
		if t.Lookup(name) != nil {
			return fmt.Errorf("volume %q already exists", name)
		}
		slot, err := t.FreeSlot()
		if err != nil {
			return err
		}
		// Initialize the slot's lease before the record exists so a crash
		// in between leaves a free slot, never a volume without a lease.
		if err := sanlock.WriteResource(d.res(slot)); err != nil {
			return fmt.Errorf("init lease for slot %d: %w", slot, err)
		}
		v, err := d.pool.Create(t, slot, name, size)
		if err != nil {
			return err
		}
		resp["volume"] = d.volInfo(v)
		d.lg.Printf("created %s slot %d size %d runs %v", name, slot, size, v.Runs)
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
	defer d.mu.Unlock()
	if h, ok := d.held[req.Name]; ok {
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
		resp["path"], resp["mode"] = store.DMPath(req.Name), modeName(h.mode)
		return nil
	}
	t, err := d.pool.ReadTable()
	if err != nil {
		return err
	}
	v := t.Lookup(req.Name)
	if v == nil {
		return store.ErrNotFound
	}
	r := d.res(v.Slot)
	deadline := time.Now().Add(timeout)
	lastAsk := time.Time{}
	t0 := time.Now()
	for {
		err = sanlock.Acquire(d.sock, r, mode, true)
		if err == nil {
			break
		}
		if !errors.Is(err, sanlock.ErrHeld) && !errors.Is(err, sanlock.ErrAgain) {
			return err
		}
		if mode == sanlock.Shared && time.Since(lastAsk) > 15*time.Second {
			d.askShare(v)
			lastAsk = time.Now()
		}
		if time.Now().After(deadline) {
			if owners, _, oerr := sanlock.Owners(r); oerr == nil && len(owners) > 0 {
				return fmt.Errorf("%s: lease held by host %d after %s: %v", req.Name, owners[0].HostID, timeout, err)
			}
			return fmt.Errorf("%s: %w", req.Name, err)
		}
		time.Sleep(time.Second)
	}
	if !lastAsk.IsZero() {
		if err := sanlock.ClearEvent(d.ls.Name); err != nil {
			d.lg.Printf("clear_event: %v", err)
		}
	}
	// No grace on the acquiring side: an early upgrade attempt simply fails
	// while the peer still holds its shared lease.
	h := &held{vol: v, mode: mode}
	if err := d.pool.DMCreate(v); err != nil {
		_ = sanlock.Release(d.sock, r)
		return err
	}
	d.held[req.Name] = h
	d.lg.Printf("activated %s %s in %s", req.Name, modeName(mode), time.Since(t0).Round(time.Millisecond))
	resp["path"], resp["mode"] = store.DMPath(req.Name), modeName(mode)
	return nil
}

// askShare sends the convert-to-shared event to whoever holds the lease
// exclusively. Returns true if a request went out.
func (d *Daemon) askShare(v *store.Volume) bool {
	owners, shared, err := sanlock.Owners(d.res(v.Slot))
	if err != nil || shared {
		return false
	}
	sent := false
	for _, o := range owners {
		if o.HostID == d.cfg.HostID {
			continue
		}
		ev := sanlock.Event{HostID: o.HostID, Generation: o.Generation, Event: EvConvertShared, Data: uint64(v.Slot)}
		if err := sanlock.SetEvent(d.ls.Name, ev); err != nil {
			d.lg.Printf("set_event to host %d: %v", o.HostID, err)
			continue
		}
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
	defer d.mu.Unlock()
	sentGuest := map[[2]uint64]bool{} // (holder host, guest) already asked
	for _, n := range names {
		if _, ok := d.held[n]; ok {
			continue
		}
		v := t.Lookup(n)
		if v == nil {
			continue
		}
		guest := guestOf(n)
		if guest == 0 {
			d.askShare(v)
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
	if err := store.DMRemove(name); err != nil {
		return err
	}
	h, ok := d.held[name]
	if !ok {
		return nil
	}
	if err := sanlock.Release(d.sock, d.res(h.vol.Slot)); err != nil {
		return err
	}
	delete(d.held, name)
	d.lg.Printf("deactivated %s", name)
	return nil
}

func (d *Daemon) resize(name string, size uint64, resp proto.Response) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	h, isHeld := d.held[name]
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

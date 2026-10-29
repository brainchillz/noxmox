// Package sanlock is a thin cgo binding over libsanlock, covering only what
// the nox daemon needs: lockspace join/leave, lease init, acquire/convert/
// release in exclusive or shared mode, orphan handling, resource owner
// inspection, and host events (the on-disk mailbox used to ask a peer to
// drop to a shared lease during live migration).
package sanlock

/*
#cgo LDFLAGS: -lsanlock
#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include <sanlock.h>
#include <sanlock_resource.h>
#include <sanlock_admin.h>
#include <sanlock_rv.h>

static struct sanlk_resource *nox_res_new(const char *ls, const char *name, const char *path, uint64_t offset, uint32_t flags) {
	struct sanlk_resource *r = calloc(1, sizeof(struct sanlk_resource) + sizeof(struct sanlk_disk));
	if (!r) return NULL;
	strncpy(r->lockspace_name, ls, SANLK_NAME_LEN);
	strncpy(r->name, name, SANLK_NAME_LEN);
	r->num_disks = 1;
	r->flags = flags;
	strncpy(r->disks[0].path, path, SANLK_PATH_LEN - 1);
	r->disks[0].offset = offset;
	return r;
}

static int nox_acquire(int sock, int pid, uint32_t flags, struct sanlk_resource *r) {
	struct sanlk_resource *rs[1] = { r };
	return sanlock_acquire(sock, pid, flags, 1, rs, NULL);
}

static int nox_release(int sock, int pid, uint32_t flags, struct sanlk_resource *r) {
	struct sanlk_resource *rs[1] = { r };
	return sanlock_release(sock, pid, flags, 1, rs);
}

static int nox_convert(int sock, int pid, uint32_t flags, struct sanlk_resource *r) {
	return sanlock_convert(sock, pid, flags, r);
}

static void nox_ls_fill(struct sanlk_lockspace *ls, const char *name, uint64_t host_id, const char *path, uint64_t offset) {
	memset(ls, 0, sizeof(*ls));
	strncpy(ls->name, name, SANLK_NAME_LEN);
	ls->host_id = host_id;
	strncpy(ls->host_id_disk.path, path, SANLK_PATH_LEN - 1);
	ls->host_id_disk.offset = offset;
}

static int nox_read_owners(struct sanlk_resource *r, struct sanlk_host **hosts, int *count) {
	return sanlock_read_resource_owners(r, 0, hosts, count);
}

static struct sanlk_host nox_host_at(struct sanlk_host *hosts, int i) { return hosts[i]; }
*/
import "C"

import (
	"errors"
	"fmt"
	"syscall"
	"unsafe"
)

// Lease modes.
const (
	Exclusive = iota
	Shared
)

// Error codes worth distinguishing. sanlock returns negative errno-style
// values; these are the ones callers branch on.
var (
	ErrHeld     = errors.New("lease held by another host") // -243 IDLIVE, -245 OTHER, -11 EAGAIN
	ErrAgain    = errors.New("try again")                  // -11 EAGAIN: contended convert, or no event pending
	ErrNoOrphan = errors.New("no orphan lease to adopt")   // -ENOENT on ORPHAN_ONLY
	ErrNotReady = errors.New("lockspace not ready")        // -241
)

// Err wraps a raw sanlock return value.
type Err struct {
	Op string
	RV int
}

func (e *Err) Error() string { return fmt.Sprintf("sanlock %s: rv %d", e.Op, e.RV) }

func wrap(op string, rv C.int) error {
	if rv >= 0 {
		return nil
	}
	e := &Err{Op: op, RV: int(rv)}
	switch rv {
	case -11:
		return fmt.Errorf("%w: %w", ErrAgain, e)
	case C.SANLK_ACQUIRE_IDLIVE, C.SANLK_ACQUIRE_OTHER, C.SANLK_ACQUIRE_OWNED, C.SANLK_ACQUIRE_OWNED_RETRY, C.SANLK_ACQUIRE_SHRETRY:
		return fmt.Errorf("%w: %w", ErrHeld, e)
	case -2:
		return fmt.Errorf("%w: %w", ErrNoOrphan, e)
	case C.SANLK_ACQUIRE_LOCKSPACE:
		return fmt.Errorf("%w: %w", ErrNotReady, e)
	}
	return e
}

// Lockspace identifies the delta-lease region on the shared device.
type Lockspace struct {
	Name   string
	HostID uint64
	Path   string
	Offset uint64
}

// Resource identifies one paxos lease on the shared device.
type Resource struct {
	Lockspace string
	Name      string
	Path      string
	Offset    uint64
}

func (r Resource) c(mode int, persistent bool) *C.struct_sanlk_resource {
	ls := C.CString(r.Lockspace)
	nm := C.CString(r.Name)
	pt := C.CString(r.Path)
	defer C.free(unsafe.Pointer(ls))
	defer C.free(unsafe.Pointer(nm))
	defer C.free(unsafe.Pointer(pt))
	var flags C.uint32_t
	if mode == Shared {
		flags |= C.SANLK_RES_SHARED
	}
	if persistent {
		flags |= C.SANLK_RES_PERSISTENT
	}
	return C.nox_res_new(ls, nm, pt, C.uint64_t(r.Offset), flags)
}

func (l Lockspace) c() *C.struct_sanlk_lockspace {
	ls := (*C.struct_sanlk_lockspace)(C.calloc(1, C.sizeof_struct_sanlk_lockspace))
	nm := C.CString(l.Name)
	pt := C.CString(l.Path)
	defer C.free(unsafe.Pointer(nm))
	defer C.free(unsafe.Pointer(pt))
	C.nox_ls_fill(ls, nm, C.uint64_t(l.HostID), pt, C.uint64_t(l.Offset))
	return ls
}

// Register connects this process to the sanlock daemon. Leases acquired on
// the returned socket belong to this pid; sanlock kills the pid if they are
// lost.
func Register() (int, error) {
	rv := C.sanlock_register()
	return int(rv), wrap("register", rv)
}

// RestrictSigkill asks sanlock to use SIGTERM only, never SIGKILL, when it
// must kill this process for a lost lease.
func RestrictSigkill(sock int) error {
	return wrap("restrict", C.sanlock_restrict(C.int(sock), C.SANLK_RESTRICT_SIGKILL))
}

// RestrictKillSignals forbids sanlock from killing this process with either
// SIGTERM or SIGKILL when a lease is lost. With no signal it is allowed to
// use, sanlock can never clear the daemon's pids on a lockspace renewal
// failure, so wdmd lets the watchdog reset the whole host. That is what we
// want: the data-path writers are the guests' QEMU processes, not the
// daemon, so clearing the daemon would let sanlock call the host clean and
// disarm the watchdog while the guests keep writing. Resetting the host is
// the only thing that stops them. systemd still stops the daemon with its
// own SIGTERM for restarts; that is unaffected, and its leases persist as
// orphans. SANLK_RESTRICT_ALL is deliberately not used: it refuses every
// further command on the connection, including the acquires the daemon
// makes on this same socket.
func RestrictKillSignals(sock int) error {
	return wrap("restrict", C.sanlock_restrict(C.int(sock), C.SANLK_RESTRICT_SIGKILL|C.SANLK_RESTRICT_SIGTERM))
}

// WriteLockspace initializes the delta-lease area on disk. maxHosts 0 means
// the sanlock default (2000 for 512-byte sectors).
func WriteLockspace(l Lockspace, maxHosts int, ioTimeout int) error {
	ls := l.c()
	defer C.free(unsafe.Pointer(ls))
	return wrap("write_lockspace", C.sanlock_write_lockspace(ls, C.int(maxHosts), 0, C.uint32_t(ioTimeout)))
}

// WriteResource initializes one paxos lease on disk.
func WriteResource(r Resource) error {
	res := r.c(Exclusive, false)
	defer C.free(unsafe.Pointer(res))
	return wrap("write_resource", C.sanlock_write_resource(res, 0, 0, 0))
}

// AddLockspace joins the lockspace. Blocks for roughly 2x io_timeout on a
// fresh join.
func AddLockspace(l Lockspace) error {
	ls := l.c()
	defer C.free(unsafe.Pointer(ls))
	rv := C.sanlock_add_lockspace(ls, 0)
	if rv == -17 { // EEXIST: already joined
		return nil
	}
	return wrap("add_lockspace", rv)
}

// AddLockspaceTimeout joins the lockspace with an explicit io_timeout,
// which every host of a pool must share; the pool records it.
func AddLockspaceTimeout(l Lockspace, ioTimeout int) error {
	ls := l.c()
	defer C.free(unsafe.Pointer(ls))
	rv := C.sanlock_add_lockspace_timeout(ls, 0, C.uint32_t(ioTimeout))
	if rv == -17 { // EEXIST: already joined
		return nil
	}
	return wrap("add_lockspace_timeout", rv)
}

// RemLockspace leaves the lockspace, releasing all leases held in it.
func RemLockspace(l Lockspace) error {
	ls := l.c()
	defer C.free(unsafe.Pointer(ls))
	return wrap("rem_lockspace", C.sanlock_rem_lockspace(ls, 0))
}

// InqLockspace reports whether the lockspace is joined and ready.
func InqLockspace(l Lockspace) (bool, error) {
	ls := l.c()
	defer C.free(unsafe.Pointer(ls))
	rv := C.sanlock_inq_lockspace(ls, 0)
	switch rv {
	case 0:
		return true, nil
	case -2, -115: // ENOENT, EINPROGRESS
		return false, nil
	}
	return false, wrap("inq_lockspace", rv)
}

// SetUsedByOrphans tells sanlock to keep the lockspace alive while orphan
// leases exist in it.
func SetUsedByOrphans(lsName string) error {
	nm := C.CString(lsName)
	defer C.free(unsafe.Pointer(nm))
	return wrap("set_config", C.sanlock_set_config(nm, 0, C.SANLK_CONFIG_USED_BY_ORPHANS, nil))
}

// Acquire takes the lease for this process in the given mode. With
// persistent set, the lease is marked SANLK_RES_PERSISTENT: if this process
// exits without releasing, sanlock keeps it as an orphan instead of
// releasing it, and an existing orphan is adopted rather than fought over.
func Acquire(sock int, r Resource, mode int, persistent bool) error {
	res := r.c(mode, persistent)
	defer C.free(unsafe.Pointer(res))
	var flags C.uint32_t = C.SANLK_ACQUIRE_OWNER_NOWAIT
	if persistent {
		flags |= C.SANLK_ACQUIRE_ORPHAN
	}
	return wrap("acquire", C.nox_acquire(C.int(sock), -1, flags, res))
}

// Adopt takes over an existing orphan lease for this process. Returns
// ErrNoOrphan when none exists.
func Adopt(sock int, r Resource, mode int) error {
	res := r.c(mode, true)
	defer C.free(unsafe.Pointer(res))
	return wrap("adopt", C.nox_acquire(C.int(sock), -1, C.SANLK_ACQUIRE_ORPHAN|C.SANLK_ACQUIRE_ORPHAN_ONLY, res))
}

// Convert changes the mode of a lease this process holds.
func Convert(sock int, r Resource, mode int) error {
	res := r.c(mode, true)
	defer C.free(unsafe.Pointer(res))
	return wrap("convert", C.nox_convert(C.int(sock), -1, 0, res))
}

// Release drops a lease this process holds.
func Release(sock int, r Resource) error {
	res := r.c(Exclusive, false)
	defer C.free(unsafe.Pointer(res))
	return wrap("release", C.nox_release(C.int(sock), -1, 0, res))
}

// ReleaseOrphan drops an orphan lease that no process holds.
func ReleaseOrphan(r Resource) error {
	res := r.c(Exclusive, false)
	defer C.free(unsafe.Pointer(res))
	return wrap("release_orphan", C.nox_release(-1, -1, C.SANLK_REL_ORPHAN, res))
}

// Owner describes a host that holds a lease.
type Owner struct {
	HostID     uint64
	Generation uint64
	Timestamp  uint64
	Flags      uint32
}

// Owners reads the on-disk lease and returns the current holders and whether
// the lease is in shared mode.
func Owners(r Resource) ([]Owner, bool, error) {
	res := r.c(Exclusive, false)
	defer C.free(unsafe.Pointer(res))
	var hosts *C.struct_sanlk_host
	var count C.int
	if err := wrap("read_resource_owners", C.nox_read_owners(res, &hosts, &count)); err != nil {
		return nil, false, err
	}
	defer C.free(unsafe.Pointer(hosts))
	out := make([]Owner, 0, int(count))
	for i := 0; i < int(count); i++ {
		h := C.nox_host_at(hosts, C.int(i))
		out = append(out, Owner{uint64(h.host_id), uint64(h.generation), uint64(h.timestamp), uint32(h.flags)})
	}
	return out, res.flags&C.SANLK_RES_SHARED != 0, nil
}

// Event is a 128-bit message delivered through the lockspace to one host.
type Event struct {
	HostID     uint64 // destination when sending, source when receiving
	Generation uint64
	Event      uint64
	Data       uint64
}

// RegEvent subscribes to events addressed to this host. Returns a fd that
// becomes readable when an event arrives.
func RegEvent(lsName string) (int, error) {
	nm := C.CString(lsName)
	defer C.free(unsafe.Pointer(nm))
	rv := C.sanlock_reg_event(nm, nil, 0)
	return int(rv), wrap("reg_event", rv)
}

// GetEvent reads one pending event from a RegEvent fd.
func GetEvent(fd int) (Event, error) {
	var he C.struct_sanlk_host_event
	var fromHost, fromGen C.uint64_t
	if err := wrap("get_event", C.sanlock_get_event(C.int(fd), 0, &he, &fromHost, &fromGen)); err != nil {
		return Event{}, err
	}
	return Event{HostID: uint64(fromHost), Generation: uint64(fromGen), Event: uint64(he.event), Data: uint64(he.data)}, nil
}

// EndEvent closes an event subscription.
func EndEvent(fd int, lsName string) {
	nm := C.CString(lsName)
	defer C.free(unsafe.Pointer(nm))
	C.sanlock_end_event(C.int(fd), nm, 0)
}

// SetEvent writes an event addressed to ev.HostID into the lockspace.
func SetEvent(lsName string, ev Event) error {
	nm := C.CString(lsName)
	defer C.free(unsafe.Pointer(nm))
	he := C.struct_sanlk_host_event{host_id: C.uint64_t(ev.HostID), generation: C.uint64_t(ev.Generation), event: C.uint64_t(ev.Event), data: C.uint64_t(ev.Data)}
	return wrap("set_event", C.sanlock_set_event(nm, &he, 0))
}

// ClearEvent removes this host's pending outgoing event so peers stop
// receiving it on every renewal.
func ClearEvent(lsName string) error {
	nm := C.CString(lsName)
	defer C.free(unsafe.Pointer(nm))
	var he C.struct_sanlk_host_event
	return wrap("clear_event", C.sanlock_set_event(nm, &he, C.SANLK_SETEV_CLEAR_EVENT|C.SANLK_SETEV_CLEAR_HOSTID))
}

// Hosts returns the live-host view of the lockspace. hostID 0 means all.
func Hosts(lsName string, hostID uint64) ([]Owner, error) {
	nm := C.CString(lsName)
	defer C.free(unsafe.Pointer(nm))
	var hosts *C.struct_sanlk_host
	var count C.int
	if err := wrap("get_hosts", C.sanlock_get_hosts(nm, C.uint64_t(hostID), &hosts, &count, 0)); err != nil {
		return nil, err
	}
	defer C.free(unsafe.Pointer(hosts))
	out := make([]Owner, 0, int(count))
	for i := 0; i < int(count); i++ {
		h := C.nox_host_at(hosts, C.int(i))
		out = append(out, Owner{uint64(h.host_id), uint64(h.generation), uint64(h.timestamp), uint32(h.flags)})
	}
	return out, nil
}

// Host liveness values from sanlock_get_hosts (low nibble of Flags).
const (
	HostUnknown = 1
	HostFree    = 2
	HostLive    = 3
	HostFail    = 4
	HostDead    = 5
)

// HostStateName renders a liveness value.
func HostStateName(st int) string {
	switch st {
	case HostFree:
		return "free"
	case HostLive:
		return "live"
	case HostFail:
		return "fail"
	case HostDead:
		return "dead"
	}
	return "unknown"
}

type pollFd struct {
	fd      int32
	events  int16
	revents int16
}

// WaitEvent blocks up to ms milliseconds for an event fd to become readable.
func WaitEvent(fd int, ms int) bool {
	p := pollFd{fd: int32(fd), events: 1 /* POLLIN */}
	n, _, _ := syscall.Syscall(syscall.SYS_POLL, uintptr(unsafe.Pointer(&p)), 1, uintptr(ms))
	return n > 0
}

// HostState extracts the liveness state from an Owner's flags.
func HostState(o Owner) int { return int(o.Flags & 0xf) }

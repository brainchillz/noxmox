// Package proto is the JSON-over-Unix-socket protocol between the nox daemon
// and its clients (the nox CLI and the Proxmox storage plugin). One request
// per connection, newline terminated, one JSON response back.
package proto

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"time"
)

// DefaultPool is the pool name used when none is given.
const DefaultPool = "nox"

// SocketPath is where the daemon for a pool listens. The default pool also
// answers on DefaultSocket for configurations that predate pool names.
func SocketPath(pool string) string { return "/run/noxd-" + pool + ".sock" }

// DefaultSocket is the legacy socket of the default pool.
const DefaultSocket = "/run/noxd.sock"

// Request is a client command.
type Request struct {
	Op        string   `json:"op"`
	Name      string   `json:"name,omitempty"`
	Size      uint64   `json:"size,omitempty"`
	Mode      string   `json:"mode,omitempty"`       // "ex" or "sh"
	Timeout   int      `json:"timeout,omitempty"`    // seconds
	Names     []string `json:"names,omitempty"`      // prepare: volumes about to be activated shared
	Full      bool     `json:"full,omitempty"`       // check: also read every free slot's lease
	Format    string   `json:"format,omitempty"`     // create: "raw" (default) or "qcow2"
	Parent    string   `json:"parent,omitempty"`     // create: name of the backing volume
	NewName   string   `json:"new_name,omitempty"`   // rename: the new name
	VirtSize  uint64   `json:"virt_size,omitempty"`  // create/resize: guest visible size of a qcow2 volume
	Reparent  bool     `json:"reparent,omitempty"`   // delete: volume is a merged chain member, re-point its children
	IOTimeout int      `json:"io_timeout,omitempty"` // set-io-timeout: new sanlock io_timeout in seconds
}

// Response carries ok/error plus op-specific fields.
type Response map[string]any

// Call sends one request and returns the parsed response.
func Call(socket string, req Request, timeout time.Duration) (Response, error) {
	c, err := net.DialTimeout("unix", socket, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("noxd not reachable at %s: %w", socket, err)
	}
	defer c.Close()
	if timeout > 0 {
		_ = c.SetDeadline(time.Now().Add(timeout))
	}
	b, _ := json.Marshal(req)
	if _, err := c.Write(append(b, '\n')); err != nil {
		return nil, err
	}
	line, err := bufio.NewReaderSize(c, 1<<20).ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("reading reply: %w", err)
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, err
	}
	if ok, _ := resp["ok"].(bool); !ok {
		msg, _ := resp["error"].(string)
		return resp, fmt.Errorf("%s", msg)
	}
	return resp, nil
}

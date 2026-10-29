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

// DefaultSocket is where noxd listens.
const DefaultSocket = "/run/noxd.sock"

// Request is a client command.
type Request struct {
	Op      string   `json:"op"`
	Name    string   `json:"name,omitempty"`
	Size    uint64   `json:"size,omitempty"`
	Mode    string   `json:"mode,omitempty"`    // "ex" or "sh"
	Timeout int      `json:"timeout,omitempty"` // seconds
	Names   []string `json:"names,omitempty"`   // prepare: volumes about to be activated shared
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

// Package health does a quick TCP reachability check against a host's
// address:port, so you can see which saved machines are actually up before
// you try to connect.
package health

import (
	"fmt"
	"net"
	"time"
)

// DefaultTimeout is used when callers don't need a specific budget.
const DefaultTimeout = 2 * time.Second

// Check reports whether address:port accepts a TCP connection within
// timeout. It only proves the port is listening — not that SSH auth would
// succeed.
func Check(address string, port int, timeout time.Duration) bool {
	if port == 0 {
		port = 22
	}
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", address, port), timeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

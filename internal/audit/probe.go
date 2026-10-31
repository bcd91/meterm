package audit

import (
	"context"
	"net"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Dialer is the subset of net.Dialer that Probe needs; tests substitute a
// fake so nothing touches the network.
type Dialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

// commonPorts are always tried in addition to whatever the host reported:
// the ones worth knowing about even if the host itself doesn't list them.
var commonPorts = []int{22, 80, 443}

// Probe tries a plain TCP connect to each port on addr and reports which
// accepted. It sends no data and does nothing beyond connecting and hanging
// up. What it measures is reachability from the machine running meterm, so
// run it from outside the network you're auditing — from inside a LAN or
// VPN, "reachable" says nothing about the internet.
func Probe(ctx context.Context, d Dialer, addr string, ports []int, timeout time.Duration) map[int]bool {
	want := map[int]bool{}
	for _, p := range commonPorts {
		want[p] = true
	}
	for _, p := range ports {
		want[p] = true
	}
	list := make([]int, 0, len(want))
	for p := range want {
		list = append(list, p)
	}
	sort.Ints(list)

	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		out = make(map[int]bool, len(list))
	)
	for _, p := range list {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			conn, err := d.DialContext(cctx, "tcp", net.JoinHostPort(addr, strconv.Itoa(p)))
			if err == nil {
				conn.Close()
			}
			mu.Lock()
			out[p] = err == nil
			mu.Unlock()
		}(p)
	}
	wg.Wait()
	return out
}

// PortsToProbe lists the ports worth probing for a host: everything bound to
// all interfaces (so the exposed-ports finding can be confirmed or cleared).
func PortsToProbe(f Facts) []int {
	var ports []int
	for _, l := range f.Listeners {
		if l.Wildcard {
			ports = append(ports, l.Port)
		}
	}
	return ports
}

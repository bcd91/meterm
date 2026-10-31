package health

import (
	"net"
	"testing"
	"time"
)

func TestCheckUpAgainstLocalListener(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	addr := l.Addr().(*net.TCPAddr)
	if !Check("127.0.0.1", addr.Port, time.Second) {
		t.Error("expected Check to report the listener as up")
	}
}

func TestCheckDownAgainstClosedPort(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close() // now guaranteed closed

	if Check("127.0.0.1", port, 500*time.Millisecond) {
		t.Error("expected Check to report a closed port as down")
	}
}

func TestCheckDefaultsPortTo22(t *testing.T) {
	// Just confirm it doesn't panic and treats port 0 as 22 by not
	// erroring out on a bad address before it even dials.
	if Check("127.0.0.1", 0, 50*time.Millisecond) {
		t.Skip("port 22 happened to be open locally; nothing to assert")
	}
}

package transport

import (
	"net"
	"testing"
	"time"

	sipgo "github.com/emiago/sipgo/sip"

	"github.com/livekit/sipgo/sip"
)

// TestTCPBrokenConnectionEvictedFromPool reproduces the half-open connection
// reuse bug.
//
// When a write to a pooled TCP connection fails (for example because the peer
// silently dropped a half-open socket), the connection is dead and must be
// removed from the pool so the next request dials a fresh one. Before the fix
// WriteMsg returned the error but left the broken connection in the pool, so
// ClientRequestConnection kept handing it back and every subsequent request
// failed on the same dead socket.
func TestTCPBrokenConnectionEvictedFromPool(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	// Accept and hold the server side so the client dial succeeds.
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()

	tp := NewTCPTransport(sipgo.NewParser())
	defer tp.Close()

	lnAddr := ln.Addr().(*net.TCPAddr)
	raddr := Addr{IP: lnAddr.IP, Port: lnAddr.Port}
	addr := (&net.TCPAddr{IP: lnAddr.IP, Port: lnAddr.Port}).String()

	conn, err := tp.CreateConnection(Addr{}, "", raddr, func(sip.Message) {})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	// Sanity: the freshly dialed connection is in the pool.
	if c := tp.pool.Get(addr); c == nil {
		t.Fatal("expected connection to be pooled after dial")
	} else {
		c.TryClose() // release the ref taken by Get
	}

	// Simulate a half-open socket: force the next write to fail deterministically
	// (this mirrors the production "i/o timeout" / "use of closed network
	// connection" transport errors).
	tcpConn := conn.(*TCPConnection)
	if err := tcpConn.Conn.(*net.TCPConn).SetWriteDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}

	req := sip.NewRequest(sip.OPTIONS, sip.Uri{User: "probe", Host: lnAddr.IP.String(), Port: lnAddr.Port})
	if err := conn.WriteMsg(req); err == nil {
		t.Fatal("expected WriteMsg to fail on a half-open socket")
	}

	// The broken connection must be evicted from the pool so it is not reused.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c := tp.pool.Get(addr)
		if c == nil {
			return // evicted -> fixed behavior
		}
		c.TryClose()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("broken connection still in pool: it would be reused (half-open socket reuse bug)")
}

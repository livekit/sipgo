package transport

import (
	"log/slog"
	"net"
	"os"
	"testing"

	"github.com/livekit/sipgo/fakes"
)

func TestConnectionPool(t *testing.T) {
	pool := NewConnectionPool()

	fakeConn := &fakes.TCPConn{
		LAddr:  net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 5060},
		RAddr:  net.TCPAddr{IP: net.ParseIP("127.0.0.2"), Port: 5060},
		Reader: nil,
		Writer: nil,
	}
	conn := &TCPConnection{Conn: fakeConn}

	pool.Add(fakeConn.RAddr.String(), conn)

	c := pool.Get(fakeConn.RAddr.String())
	if c != conn {
		t.Fatal("Not found connection")
	}
}

func TestConnectionPoolRetire(t *testing.T) {
	const addr = "127.0.0.2:5060"
	newConn := func() *TCPConnection {
		return &TCPConnection{Conn: &fakes.TCPConn{
			LAddr: net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 5060},
			RAddr: net.TCPAddr{IP: net.ParseIP("127.0.0.2"), Port: 5060},
		}}
	}
	pool := NewConnectionPool()

	c1 := newConn()
	pool.Add(addr, c1) // ref 1: the idle reference

	// Retiring a connection that is not the one mapped at addr is a no-op.
	if pool.Retire(addr, newConn()) {
		t.Fatal("retired a connection that was never pooled")
	}
	if got := pool.Get(addr); got != c1 {
		t.Fatalf("mapping changed by a no-op retire: %v", got)
	}
	c1.TryClose() // give back Get's reference

	// Retiring a connection must not close it.
	conn := pool.Get(addr) // ref 2
	if !pool.Retire(addr, c1) {
		t.Fatal("retire of the mapped connection failed")
	}
	if got := pool.Get(addr); got != nil {
		t.Fatalf("retired connection still mapped: %v", got)
	}
	if ref := c1.Ref(0); ref != 1 {
		t.Fatalf("retire should not remove reference to connection, got %d", ref)
	}

	// A replacement at the same address.
	c2 := newConn()
	pool.Add(addr, c2)

	// A stale retire of c1 must leave c2 alone.
	if pool.Retire(addr, c1) {
		t.Fatal("stale retire reported success")
	}
	if got := pool.Get(addr); got != c2 {
		t.Fatalf("stale retire touched the replacement: %v", got)
	}
	c2.TryClose()

	// Closing the last handle should close the underlying connection.
	if ref, _ := conn.TryClose(); ref != 0 {
		t.Fatalf("last holder should bring ref to 0, got %d", ref)
	}

	// c1's read loop cleans up. It must not unmap c2.
	pool.CloseAndDelete(c1, addr)
	if got := pool.Get(addr); got != c2 {
		t.Fatalf("CloseAndDelete of a retired connection unmapped its replacement: %v", got)
	}
	c2.TryClose()
	if n := pool.Size(); n != 1 {
		t.Fatalf("pool size %d, want 1", n)
	}

	// With nobody holding it, retire closes right away.
	if !pool.Retire(addr, c2) {
		t.Fatal("retire of the idle connection failed")
	}
	if ref := c2.Ref(0); ref != 0 {
		t.Fatalf("idle retire should close, ref %d", ref)
	}
	if n := pool.Size(); n != 0 {
		t.Fatalf("pool size %d, want 0", n)
	}
}

func BenchmarkConnectionPool(b *testing.B) {
	slog.SetDefault(slog.New(
		slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn}),
	))
	pool := NewConnectionPool()

	for i := 0; i < b.N; i++ {
		conn := &TCPConnection{Conn: &fakes.TCPConn{
			LAddr:  net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 5060},
			RAddr:  net.TCPAddr{IP: net.ParseIP("127.0.0.2"), Port: 5060},
			Reader: nil,
			Writer: nil,
		}}
		a := &net.TCPAddr{
			IP:   net.IPv4('1', '2', '3', byte(i)),
			Port: 1000,
		}
		pool.Add(a.String(), conn)
		c := pool.Get(a.String())
		if c != conn {
			b.Fatal("mismatched function")
		}
	}
}

package transport

import (
	"log/slog"
	"sync"
)

// TODO Connection pool with keeping active connections longer

type ConnectionPool struct {
	// TODO consider sync.Map way with atomic checks to reduce mutex contention
	sync.RWMutex
	m map[string]Connection
}

func NewConnectionPool() *ConnectionPool {
	return &ConnectionPool{
		m: make(map[string]Connection),
	}
}

// Add registers a connection for an address. One connection per address: the
// last one added wins, so a duplicate silently orphans the previous one. See
// the TODO in TCPTransport.createConnection for why duplicates can happen.
func (p *ConnectionPool) Add(a string, c Connection) {
	if c.Ref(0) < 1 {
		c.Ref(1) // Make 1 reference count by default
	}
	p.Lock()
	p.m[a] = c
	p.Unlock()
}

// Getting connection pool increases reference
// Make sure you TryClose after finish
func (p *ConnectionPool) Get(a string) (c Connection) {
	p.RLock()
	defer p.RUnlock()
	c, exists := p.m[a]
	if !exists {
		return nil
	}
	c.Ref(1)
	return c
}

// Retire removes a connection from the pool. Does not close the connection.
func (p *ConnectionPool) Retire(addr string, c Connection) bool {
	p.Lock()
	cur, ok := p.m[addr]
	if !ok || cur != c {
		p.Unlock()
		return false
	}
	delete(p.m, addr)
	p.Unlock()
	if _, err := c.TryClose(); err != nil {
		slog.Warn("Error closing retired connection", "err", err)
	}
	return true
}

// CloseAndDelete closes connection and deletes from pool
func (p *ConnectionPool) CloseAndDelete(c Connection, addr string) {
	p.Lock()
	defer p.Unlock()
	if c.Ref(0) > 0 {
		// Still referenced, so hard close. A connection that TryClose or
		// Retire already brought to zero is closed and must not be closed twice.
		if err := c.Close(); err != nil {
			slog.Warn("Closing conection return error", "err", err)
		}
	}
	// Only unmap if the entry is still ours (to avoid closing a newly
	// established connection on the same address).
	if cur, ok := p.m[addr]; ok && cur == c {
		delete(p.m, addr)
	}
}

// Clear will clear all connection from pool and close them
func (p *ConnectionPool) Clear() {
	p.Lock()
	defer p.Unlock()
	for _, c := range p.m {
		if c.Ref(0) <= 0 {
			continue
		}
		if err := c.Close(); err != nil {
			slog.Warn("Closing conection return error", "err", err)
		}
	}
	// Remove all
	p.m = make(map[string]Connection)
}

func (p *ConnectionPool) Size() int {
	p.RLock()
	l := len(p.m)
	p.RUnlock()
	return l
}

package transport

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	sipgo "github.com/emiago/sipgo/sip"

	"github.com/livekit/sipgo/sip"
)

type PortRange struct {
	Min, Max int
}

type TCPConfig struct {
	DialPorts PortRange
}

// TCP transport implementation
type TCPTransport struct {
	c         TCPConfig
	addr      string
	transport string
	parser    *sipgo.Parser
	log       *slog.Logger

	pool *ConnectionPool
}

func NewTCPTransport(log *slog.Logger, par *sipgo.Parser, c *TCPConfig) *TCPTransport {
	p := &TCPTransport{
		parser:    par,
		pool:      NewConnectionPool(),
		transport: TransportTCP,
	}
	if c != nil {
		p.c = *c
	}
	p.log = log.With("caller", "transport<TCP>")
	return p
}

func (t *TCPTransport) String() string {
	return "transport<TCP>"
}

func (t *TCPTransport) Network() string {
	// return "tcp"
	return t.transport
}

func (t *TCPTransport) Close() error {
	// return t.connections.Done()
	t.pool.Clear()
	return nil
}

// Serve is direct way to provide conn on which this worker will listen
func (t *TCPTransport) Serve(l net.Listener, handler sip.MessageHandler) error {
	t.log.Debug("begin listening on", "net", t.Network(), "addr", l.Addr())
	for {
		conn, err := l.Accept()
		if err != nil {
			t.log.Debug("Fail to accept conenction", "err", err)
			return err
		}

		t.initConnection(conn, conn.RemoteAddr().String(), handler)
	}
}

func (t *TCPTransport) ResolveAddr(addr string) (net.Addr, error) {
	return net.ResolveTCPAddr("tcp", addr)
}

func (t *TCPTransport) GetConnection(addr string) (Connection, error) {
	raddr, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		return nil, err
	}
	addr = raddr.String()

	t.log.Debug("Getting connection", "addr", addr)

	c := t.pool.Get(addr)
	return c, nil
}

func (t *TCPTransport) CreateConnection(laddr Addr, host string, raddr Addr, handler sip.MessageHandler) (Connection, error) {
	// We are letting transport layer to resolve our address
	// raddr, err := net.ResolveTCPAddr("tcp", addr)
	// if err != nil {
	// 	return nil, err
	// }

	traddr := &net.TCPAddr{
		IP:   raddr.IP,
		Port: raddr.Port,
	}
	return t.createConnection(traddr, handler)
}

func (t *TCPTransport) createConnection(raddr *net.TCPAddr, handler sip.MessageHandler) (Connection, error) {
	addr := raddr.String()
	t.log.Debug("Dialing new connection", "raddr", addr)

	conn, err := bindRange(t.c.DialPorts.Min, t.c.DialPorts.Max, func(lport int) (*net.TCPConn, error) {
		return net.DialTCP("tcp", &net.TCPAddr{
			IP:   nil, // let os decide
			Port: lport,
		}, raddr)
	})
	if err != nil {
		return nil, fmt.Errorf("%s dial err=%w", t, err)
	}

	// if err := conn.SetKeepAlive(true); err != nil {
	// 	return nil, fmt.Errorf("%s keepalive err=%w", t, err)
	// }

	// if err := conn.SetKeepAlivePeriod(30 * time.Second); err != nil {
	// 	return nil, fmt.Errorf("%s keepalive period err=%w", t, err)
	// }

	c := t.initConnection(conn, addr, handler)
	return c, nil
}

func (t *TCPTransport) initConnection(conn net.Conn, addr string, handler sip.MessageHandler) Connection {
	// // conn.SetKeepAlive(true)
	// conn.SetKeepAlivePeriod(3 * time.Second)

	t.log.Debug("New connection", "raddr", addr)
	c := &TCPConnection{
		Conn:     conn,
		refcount: 1 + IdleConnection,
	}
	t.pool.Add(addr, c)
	go t.readConnection(c, addr, handler)
	return c
}

// This should performe better to avoid any interface allocation
func (t *TCPTransport) readConnection(conn *TCPConnection, raddr string, handler sip.MessageHandler) {
	buf := make([]byte, transportBufferSize)

	defer t.pool.CloseAndDelete(conn, raddr)

	// Create stream parser context
	par := t.parser.NewSIPStream()
	defer par.Close()

	// partial is true while a message is still being assembled. The parser
	// cannot be asked: it drains complete header lines as it parses them, so
	// its buffer stays near empty however far along the message is.
	partial := false

	for {
		num, err := conn.Read(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF) {
				t.log.Debug("connection was closed", "err", err)
				connectionClosed.WithLabelValues("tcp", "peer_close").Inc()
				return
			}

			if errors.Is(err, os.ErrDeadlineExceeded) {
				// Nothing arrived for a whole TCPReadTimeout. The connection is
				// left open: some peers dial once, cache the connection and never
				// rebuild it, so closing an idle one can cost us every later
				// request from that peer.
				//
				// If a message was still incomplete, the rest of it is not
				// coming. Drop it, otherwise the next bytes to arrive are
				// appended to a fragment that can never complete and are lost
				// with it.
				if partial {
					t.log.Info("discarding incomplete sip message, nothing followed it",
						"raddr", raddr, "buffered", par.Buffer().Len())
					par.Reset()
					partial = false
					parserReset.WithLabelValues("tcp", "stale_partial").Inc()
				}
				continue
			}

			t.log.Debug("Read error", "err", err)
			connectionClosed.WithLabelValues("tcp", "read_error").Inc()
			return
		}

		data := buf[:num]
		if len(bytes.Trim(data, "\x00")) == 0 {
			continue
		}

		// Check is keep alive
		if len(data) <= 4 {
			//One or 2 CRLF
			if len(bytes.Trim(data, "\r\n")) == 0 {
				t.log.Debug("Keep alive CRLF received")
				continue
			}
		}

		// TODO fallback to parseFull if message size limit is set

		// t.log.Debug().Str("raddr", raddr).Str("data", string(data)).Msg("new message")
		// A partial message is normal on a stream transport. Anything else means
		// the parser cannot frame this stream any more, and there is no reliable
		// way to find the next message boundary in a byte stream, so the only way
		// back to a known one is a new connection. Reads keep succeeding either
		// way, so nothing else would notice.
		//
		// The parser bounds an incomplete message itself and reports
		// ErrMessageTooLarge, but only from the path that knows the message is
		// incomplete. A CR with no LF takes a different path, consumes nothing and
		// returns the same error on every later read, so it has to be caught here.
		err = t.parseStream(par, data, raddr, handler)
		partial = errors.Is(err, sipgo.ErrParseSipPartial)
		if err != nil && !partial {
			reason := closeReason(err)
			t.log.Info("closing connection, sip stream cannot be framed",
				"err", err, "raddr", raddr, "reason", reason)
			connectionClosed.WithLabelValues("tcp", reason).Inc()
			return
		}
	}
}

func (t *TCPTransport) parseStream(par *sipgo.ParserStream, data []byte, src string, handler sip.MessageHandler) error {
	bytesPacketSize.WithLabelValues("tcp", "read").Observe(float64(len(data)))
	return par.ParseSIPStream(data, func(msg sipgo.Message) {
		msg.SetTransport(t.Network())
		msg.SetSource(src)
		handler(msg)
	})
}

// closeReason maps a parse failure to a metric label.
func closeReason(err error) string {
	switch {
	case errors.Is(err, sipgo.ErrMessageTooLarge):
		return "message_too_large"
	case errors.Is(err, sipgo.ErrParseLineNoCRLF):
		return "no_crlf"
	default:
		return "parse_error"
	}
}

// TODO use this when message size limit is defined
func (t *TCPTransport) parseFull(data []byte, src string, handler sip.MessageHandler) {
	msg, err := t.parser.ParseSIP(data) //Very expensive operation
	if err != nil {
		t.log.Info("failed to parse", "err", err, "data", string(data))
		return
	}

	msg.SetTransport(t.Network())
	msg.SetSource(src)
	handler(msg)
}

type TCPConnection struct {
	net.Conn

	mu       sync.RWMutex
	refcount int
}

func (c *TCPConnection) Ref(i int) int {
	c.mu.Lock()
	c.refcount += i
	ref := c.refcount
	c.mu.Unlock()
	slog.Debug("TCP reference increment", "ip", c.LocalAddr(), "dst", c.RemoteAddr(), "ref", ref)
	return ref
}

func (c *TCPConnection) Close() error {
	c.mu.Lock()
	c.refcount = 0
	c.mu.Unlock()
	slog.Debug("TCP doing hard close", "ip", c.LocalAddr(), "dst", c.RemoteAddr(), "ref", 0)
	return c.Conn.Close()
}

func (c *TCPConnection) TryClose() (int, error) {
	c.mu.Lock()
	c.refcount--
	ref := c.refcount
	c.mu.Unlock()
	slog.Debug("TCP reference decrement", "ip", c.LocalAddr(), "dst", c.RemoteAddr().String(), "ref", ref)
	if ref > 0 {
		return ref, nil
	}

	if ref < 0 {
		slog.Warn("TCP ref went negative", "ip", c.LocalAddr(), "dst", c.RemoteAddr(), "ref", ref)
		return 0, nil
	}

	slog.Debug("TCP closing", "ip", c.LocalAddr(), "dst", c.RemoteAddr(), "ref", ref)
	return ref, c.Conn.Close()
}

func (c *TCPConnection) Read(b []byte) (n int, err error) {
	if TCPReadTimeout > 0 {
		if err := c.Conn.SetReadDeadline(time.Now().Add(TCPReadTimeout)); err != nil {
			return 0, err
		}
	}

	// Some debug hook. TODO move to proper way
	n, err = c.Conn.Read(b)
	if SIPDebug {
		slog.Debug("TCP read", "local", c.Conn.LocalAddr(), "remote", c.Conn.RemoteAddr(), "data", string(b[:n]))
	}
	return n, err
}

func (c *TCPConnection) Write(b []byte) (n int, err error) {
	// Some debug hook. TODO move to proper way
	c.Conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	n, err = c.Conn.Write(b)
	if SIPDebug {
		slog.Debug("TCP write", "local", c.Conn.LocalAddr(), "remote", c.Conn.RemoteAddr(), "data", string(b[:n]))
	}
	return n, err
}

func (c *TCPConnection) WriteMsg(msg sip.Message) error {
	buf := bufPool.Get().(*bytes.Buffer)
	defer bufPool.Put(buf)
	buf.Reset()
	msg.StringWrite(buf)
	data := buf.Bytes()

	bytesPacketSize.WithLabelValues("tcp", "write").Observe(float64(len(data)))

	n, err := c.Write(data)
	if err != nil {
		return fmt.Errorf("conn %s write err=%w", c.RemoteAddr().String(), err)
	}

	if n == 0 {
		return fmt.Errorf("wrote 0 bytes")
	}

	if n != len(data) {
		return fmt.Errorf("fail to write full message")
	}
	return nil
}

type bindFunc[T any] func(port int) (T, error)

var ErrCannotBindPort = errors.New("cannot allocate port")

func bindRange[T any](portMin, portMax int, create bindFunc[T]) (T, error) {
	var zero T
	if portMin <= 0 && portMax <= 0 {
		return create(0)
	} else if portMin == portMax {
		return create(portMin)
	}
	if portMin <= 0 {
		portMin = 1
	}
	if portMax <= 0 || portMax > 0xFFFF {
		portMax = 0xFFFF
	}
	if portMin > portMax {
		return zero, errors.New("invalid range")
	}

	ports := portMax - portMin + 1
	portCurrent := rand.Intn(ports) + portMin

	for try := 0; try < ports; try++ {
		c, err := create(portCurrent)
		if err == nil {
			return c, nil
		} else if !errors.Is(err, syscall.EADDRINUSE) {
			return c, err
		}
		portCurrent++
		if portCurrent > portMax {
			portCurrent = portMin
		}
	}
	return zero, ErrCannotBindPort
}

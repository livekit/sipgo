package transport

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	sipgo "github.com/emiago/sipgo/sip"

	"github.com/livekit/sipgo/sip"
)

var (
	ErrNetworkNotSuported = errors.New("protocol not supported")
)

// srvLabels returns the SRV service and protocol labels for a transport.
// RFC 3263 4.1 covers udp, tcp and tls; RFC 7118 6 covers ws and wss. The secure
// transports are always sips, whatever the URI scheme says.
func srvLabels(network, uriScheme string) (service, protocol string) {
	switch network {
	case "udp", "udp4", "udp6":
		return uriScheme, "udp"
	case "tls":
		return "sips", "tcp"
	case "ws":
		return uriScheme, "ws"
	case "wss":
		return "sips", "wss"
	default:
		return uriScheme, "tcp"
	}
}

// dnsIPPreference selects which family to take when a host resolves to both.
type dnsIPPreference int

const (
	// dnsPreferAny takes whichever address the resolver returned first.
	dnsPreferAny dnsIPPreference = iota
	dnsPreferIPv4
	dnsPreferIPv6
)

// Layer implementation.
type Layer struct {
	log *slog.Logger

	udp *UDPTransport
	tcp *TCPTransport
	tls *TLSTransport
	ws  *WSTransport
	wss *WSSTransport

	transports map[string]Transport

	listenPorts   map[string][]int
	listenPortsMu sync.Mutex
	dnsResolver   *net.Resolver

	handlers []sip.MessageHandler

	// dnsPreferSRV always does the SRV lookup first
	dnsPreferSRV bool
	dnsPreferIP  dnsIPPreference

	// ConnectionReuse will force connection reuse when passing request
	ConnectionReuse bool
}

// NewLayer creates transport layer.
// dns Resolver
// sip parser
// tls config - can be nil to use default tls
func NewLayer(
	log *slog.Logger,
	dnsResolver *net.Resolver,
	sipparser *sipgo.Parser,
	tcpConfig *TCPConfig,
	tlsConfig *tls.Config,
) *Layer {
	l := &Layer{
		log:             log.With("caller", "transportlayer"),
		transports:      make(map[string]Transport),
		listenPorts:     make(map[string][]int),
		dnsResolver:     dnsResolver,
		dnsPreferSRV:    true,
		dnsPreferIP:     dnsPreferIPv4,
		ConnectionReuse: true,
	}

	// Make some default transports available.
	l.udp = NewUDPTransport(log, sipparser)
	l.tcp = NewTCPTransport(log, sipparser, tcpConfig)
	// TODO. Using default dial tls, but it needs to configurable via client
	l.tls = NewTLSTransport(log, sipparser, tcpConfig, tlsConfig)
	l.ws = NewWSTransport(log, sipparser)
	// TODO. Using default dial tls, but it needs to configurable via client
	l.wss = NewWSSTransport(log, sipparser, tlsConfig)

	// Fill map for fast access
	l.transports["udp"] = l.udp
	l.transports["tcp"] = l.tcp
	l.transports["tls"] = l.tls
	l.transports["ws"] = l.ws
	l.transports["wss"] = l.wss

	return l
}

// OnMessage is main function which will be called on any new message by transport layer
func (l *Layer) OnMessage(h sip.MessageHandler) {
	// if l.handler != nil {
	// 	// Make sure appending
	// 	next := l.handler
	// 	l.handler = func(m sip.Message) {
	// 		h(m)
	// 		next(m)
	// 	}
	// 	return
	// }

	// l.handler = h

	l.handlers = append(l.handlers, h)
}

// handleMessage is transport layer for handling messages
func (l *Layer) handleMessage(msg sip.Message) {
	// We have to consider
	// https://datatracker.ietf.org/doc/html/rfc3261#section-18.2.1 for some message editing
	// Proxy further to other

	// 18.1.2 Receiving Responses
	// States that transport should find transaction and if not, it should still forward message to core
	// l.handler(msg)
	for _, h := range l.handlers {
		h(msg)
	}
}

// ServeUDP will listen on udp connection
func (l *Layer) ServeUDP(c net.PacketConn) error {
	_, port, err := sip.ParseAddr(c.LocalAddr().String())
	if err != nil {
		return err
	}

	l.addListenPort("udp", port)

	return l.udp.Serve(c, l.handleMessage)
}

// ServeTCP will listen on tcp connection
func (l *Layer) ServeTCP(c net.Listener) error {
	_, port, err := sip.ParseAddr(c.Addr().String())
	if err != nil {
		return err
	}

	l.addListenPort("tcp", port)

	return l.tcp.Serve(c, l.handleMessage)
}

// ServeWS will listen on ws connection
func (l *Layer) ServeWS(c net.Listener) error {
	_, port, err := sip.ParseAddr(c.Addr().String())
	if err != nil {
		return err
	}

	l.addListenPort("ws", port)

	return l.ws.Serve(c, l.handleMessage)
}

// ServeTLS will listen on tcp connection
func (l *Layer) ServeTLS(c net.Listener) error {
	_, port, err := sip.ParseAddr(c.Addr().String())
	if err != nil {
		return err
	}

	l.addListenPort("tls", port)
	return l.tls.Serve(c, l.handleMessage)
}

// ServeWSS will listen on wss connection
func (l *Layer) ServeWSS(c net.Listener) error {
	_, port, err := sip.ParseAddr(c.Addr().String())
	if err != nil {
		return err
	}

	l.addListenPort("wss", port)

	return l.wss.Serve(c, l.handleMessage)
}

func (l *Layer) addListenPort(network string, port int) {
	l.listenPortsMu.Lock()
	defer l.listenPortsMu.Unlock()

	if _, ok := l.listenPorts[network]; !ok {
		if l.listenPorts[network] == nil {
			l.listenPorts[network] = make([]int, 0)
		}
		l.listenPorts[network] = append(l.listenPorts[network], port)
	}
}

func (l *Layer) GetListenPort(network string) int {
	ports, _ := l.listenPorts[network]
	if len(ports) > 0 {
		return ports[0]
	}
	return 0
}

func (l *Layer) WriteMsg(msg sip.Message) error {
	network := msg.Transport()
	addr := msg.Destination()
	return l.WriteMsgTo(msg, addr, network)
}

func (l *Layer) WriteMsgTo(msg sip.Message, addr string, network string) error {
	/*s
	// Client sending request, or we are sending responses
	To consider
		18.2.1
		When the server transport receives a request over any transport, it
		MUST examine the value of the "sent-by" parameter in the top Via
		header field value.
		If the host portion of the "sent-by" parameter
	contains a domain name, or if it contains an IP address that differs
	from the packet source address, the server MUST add a "received"
	parameter to that Via header field value.  This parameter MUST
	contain the source address from which the packet was received.
	*/

	var conn Connection
	var err error

	switch m := msg.(type) {
	// RFC 3261 - 18.1.1.
	// 	TODO
	// 	If a request is within 200 bytes of the path MTU, or if it is larger
	//    than 1300 bytes and the path MTU is unknown, the request MUST be sent
	//    using an RFC 2914 [43] congestion controlled transport protocol, such
	//    as TCP. If this causes a change in the transport protocol from the
	//    one indicated in the top Via, the value in the top Via MUST be
	//    changed.
	case *sip.Request:
		//Every new request must be handled in seperate connection
		conn, err = l.ClientRequestConnection(m)
		if err != nil {
			return err
		}

		// Reference counting should prevent us closing connection too early
		defer conn.TryClose()

	case *sip.Response:

		conn, err = l.GetConnection(network, addr)
		if err != nil {
			return err
		}

		defer conn.TryClose()
	}

	if err := conn.WriteMsg(msg); err != nil {
		return err
	}

	// transport, ok := l.transports[network]
	// if !ok {
	// 	return fmt.Errorf("transport %s is not supported", network)
	// }

	// raddr, err := transport.ResolveAddr(addr)
	// if err != nil {
	// 	return err
	// }

	// err = transport.WriteMsg(msg, raddr)
	// if err != nil {
	// 	err = fmt.Errorf("send SIP message through %s protocol to %s: %w", network, addr, err)
	// }
	return err
}

// ClientRequestConnection is based on
// https://www.rfc-editor.org/rfc/rfc3261#section-18.1.1
// It is wrapper for getting and creating connection
//
// In case req destination is DNS resolved, destination will be cached or in
// other words SetDestination will be called
func (l *Layer) ClientRequestConnection(req *sip.Request) (c Connection, err error) {
	network := NetworkToLower(req.Transport())
	transport, ok := l.transports[network]
	if !ok {
		return nil, fmt.Errorf("transport %s is not supported", network)
	}

	// Resolve our remote address
	a := req.Destination()
	host, port, err := sip.ParseAddr(a)
	if err != nil {
		return nil, fmt.Errorf("build address target for %s: %w", a, err)
	}

	// dns srv lookup

	raddr := Addr{
		IP:   net.ParseIP(host),
		Port: port,
	}
	if raddr.IP == nil {
		// TODO: how to cache this address, for example reusing in dialog routing
		if err := l.resolveRequestAddr(context.Background(), network, host, req, &raddr); err != nil {
			return nil, err
		}
		// Save destination in request to avoid repeated resolving
		req.SetDestination(raddr.String())
	}

	// Now use Via header to determine our local address
	// Here is from RFC statement:
	//   Before a request is sent, the client transport MUST insert a value of
	//   the "sent-by" field into the Via header field.  This field contains
	//   an IP address or host name, and port.
	viaHop := req.Via()
	if viaHop == nil {
		// NOTE: We are enforcing that client creates this header
		return nil, fmt.Errorf("missing Via Header")
	}

	// TODO refactor code below
	if l.ConnectionReuse {
		viaHop.Params.Add("alias", "")
		addr := raddr.String()
		c, _ := transport.GetConnection(addr)
		if c != nil {
			// Update Via sent by
			// TODO avoid this parsing

			laddr := c.LocalAddr()
			network := laddr.Network()
			laddrStr := laddr.String()

			// TODO handle broadcast address
			host, port, err := sip.ParseAddr(laddrStr)
			if err != nil {
				return nil, fmt.Errorf("fail to parse local connection address network=%s addr=%s: %w", network, laddrStr, err)
			}

			// In case client forced some host (like external IP) we do not want to overwrite
			// Currently we always have this set as resolved IP
			if viaHop.Host == "" {
				viaHop.Host = host

				// Leaving this for fallback as UDP can return us listener with on broadcast address (unspecified)
				if network == "udp" {
					// TODO refactor this
					func() {
						ip := net.ParseIP(host)
						if ip == nil {
							return
						}
						switch {
						case ip.IsUnspecified():
							l.log.Warn("External Via IP address is unspecified for UDP. Using 127.0.0.1")
							viaHop.Host = "127.0.0.1" // TODO use resolve IP
						default:
							viaHop.Host = host
						}
					}()
				}
			}

			viaHop.Port = port
			return c, nil
		}
		l.log.Debug("Active connection not found", "addr", addr)
	}

	laddr := Addr{
		IP: net.ParseIP(viaHop.Host),
		// IP:   lIP,
		Port: viaHop.Port,
	}

	l.log.Debug("Via header used for creating connection", "host", viaHop.Host, "port", viaHop.Port)

	c, err = transport.CreateConnection(laddr, host, raddr, l.handleMessage)
	if err != nil {
		return nil, err
	}

	// TODO refactor this
	switch {
	case viaHop.Host == "" || laddr.IP == nil: // If not specified by UAC we will override Via sent-by
		fallthrough
	case viaHop.Port == 0: // We still may need to rewrite sent-by port
		// TODO avoid this parsing
		l := c.LocalAddr()
		laddrStr := l.String()

		host, port, err = sip.ParseAddr(laddrStr)
		if err != nil {
			return nil, fmt.Errorf("fail to parse local connection address network=%s addr=%s: %w", network, laddrStr, err)
		}

		if viaHop.Host == "" {
			viaHop.Host = host
		}
		viaHop.Port = port
	}
	return c, nil
}

// resolveRequestAddr resolves host into addr for req, choosing between SRV and
// a plain host lookup the way RFC 3263 4.2 requires.
//
// This has no counterpart on emiago/sipgo upstream. Upstream resolves through
// resolveRemoteAddr, which always calls resolveAddr and leaves the SRV-vs-A
// choice to dnsPreferSRV, a layer-wide setting. Its own comment notes that
// honoring the port is not implemented, which is what resolveRequestAddr does.
//
// resolveAddr, resolveAddrIP and resolveAddrSRV below started as upstream v1.4.0.
// Changed since: SRV labels come from srvLabels rather than being derived inside
// resolveAddrSRV, SRV misses logged at debug now that SRV runs first, bounds
// checks around the DNS answers, and no Addr.Zone since nothing here reads it back.
func (l *Layer) resolveRequestAddr(ctx context.Context, network string, host string, req *sip.Request, addr *Addr) error {
	// In order: explicitly set destination first, then the Route header, then the
	// request URI.
	if req.MessageData.Destination() != "" {
		// Whoever set it chose the port along with the host.
		return l.resolveAddrIP(ctx, host, addr)
	}
	uri := &req.Recipient
	if hdr := req.Route(); hdr != nil {
		uri = &hdr.Address
	}
	// An explicit port means the port was chosen for us, so resolve the host
	// only. Without one, SRV supplies both the host and the port that goes with
	// it; pairing a host from one SRV record with a port from another sends the
	// request to a port that host does not serve.
	if uri.Port > 0 {
		return l.resolveAddrIP(ctx, host, addr)
	}
	service, protocol := srvLabels(network, uri.Scheme)
	return l.resolveAddr(ctx, service, protocol, host, addr)
}

// slowResolveThreshold is when a lookup is slow enough to be worth a log line.
const slowResolveThreshold = 50 * time.Millisecond

func (l *Layer) resolveAddr(ctx context.Context, service, protocol string, host string, addr *Addr) error {
	defer func(start time.Time) {
		if dur := time.Since(start); dur > slowResolveThreshold {
			l.log.Warn("DNS resolution is slow", "host", host, "dur", dur)
		}
	}(time.Now())

	if l.dnsPreferSRV {
		err := l.resolveAddrSRV(ctx, service, protocol, host, addr)
		if err == nil {
			return nil
		}
		// Most hosts publish no SRV records, so this is expected, not a problem.
		l.log.Debug("SRV lookup failed, using host lookup", "host", host, "error", err)
		return l.resolveAddrIP(ctx, host, addr)
	}

	err := l.resolveAddrIP(ctx, host, addr)
	if err == nil {
		return nil
	}

	l.log.Debug("Host lookup failed, using SRV", "host", host, "error", err)
	return l.resolveAddrSRV(ctx, service, protocol, host, addr)
}

func (l *Layer) resolveAddrIP(ctx context.Context, hostname string, addr *Addr) error {
	ips, err := l.dnsResolver.LookupIPAddr(ctx, hostname)
	if err != nil {
		return err
	}
	if len(ips) == 0 {
		return fmt.Errorf("no addresses for %q", hostname)
	}

	if l.dnsPreferIP != dnsPreferAny {
		wantIPv4 := l.dnsPreferIP == dnsPreferIPv4
		for _, ip := range ips {
			// To4 returns nil for anything that is not IPv4.
			if (ip.IP.To4() != nil) == wantIPv4 {
				addr.IP = ip.IP
				return nil
			}
		}
		// Nothing in the preferred family, so fall through and take what we have.
	}

	addr.IP = ips[0].IP
	return nil
}

func (l *Layer) resolveAddrSRV(ctx context.Context, service, protocol string, hostname string, addr *Addr) error {
	// Records arrive sorted by priority and shuffled by weight within a priority,
	// so the first one is the one to use.
	_, records, err := l.dnsResolver.LookupSRV(ctx, service, protocol, hostname)
	if err != nil {
		return fmt.Errorf("failed to look up SRV for %q: %w", hostname, err)
	}
	if len(records) == 0 {
		return fmt.Errorf("no SRV records for %q", hostname)
	}

	record := records[0]
	// A lone "." target means the service is deliberately not offered here.
	if record.Target == "" || record.Target == "." {
		return fmt.Errorf("no SIP service at %q", hostname)
	}

	// An SRV record names a host, not an address, so it still needs resolving.
	ips, err := l.dnsResolver.LookupIP(ctx, "ip", record.Target)
	if err != nil {
		return err
	}
	if len(ips) == 0 || ips[0] == nil {
		return fmt.Errorf("SRV target %q did not resolve", record.Target)
	}

	// Write both halves only once both are known. A half-written addr would leave
	// this record's port paired with an address from the fallback lookup.
	addr.IP = ips[0]
	addr.Port = int(record.Port)
	return nil
}

// GetConnection gets existing or creates new connection based on addr
func (l *Layer) GetConnection(network, addr string) (Connection, error) {
	network = NetworkToLower(network)
	return l.getConnection(network, addr)
}

func (l *Layer) getConnection(network, addr string) (Connection, error) {
	transport, ok := l.transports[network]
	if !ok {
		return nil, fmt.Errorf("transport %s is not supported", network)
	}

	c, err := transport.GetConnection(addr)
	if err == nil && c == nil {
		return nil, fmt.Errorf("connection %q does not exist", addr)
	}

	return c, err
}

func (l *Layer) Close() error {
	l.log.Debug("Layer is closing")
	var werr error
	for _, t := range l.transports {
		if err := t.Close(); err != nil {
			// For now dump last error
			werr = err
		}
	}
	return werr
}

func IsReliable(network string) bool {
	switch network {
	case "tcp", "tls", "ws", "wss", "TCP", "TLS", "WS", "WSS":
		return true
	default:
		return false
	}
}

// NetworkToLower is faster function converting UDP, TCP to udp, tcp
func NetworkToLower(network string) string {
	// Switch is faster then lower
	switch network {
	case "UDP":
		return "udp"
	case "TCP":
		return "tcp"
	case "TLS":
		return "tls"
	case "WS":
		return "ws"
	case "WSS":
		return "wss"
	default:
		return sip.ASCIIToLower(network)
	}
}

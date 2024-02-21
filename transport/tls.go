package transport

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"

	"github.com/emiago/sipgo/parser"
	"github.com/emiago/sipgo/sip"
)

var ()

// TLS transport implementation
type TLSTransport struct {
	*TCPTransport

	// rootPool *x509.CertPool
	tlsConf *tls.Config
}

// NewTLSTransport needs dialTLSConf for creating connections when dialing
func NewTLSTransport(par *parser.Parser, dialTLSConf *tls.Config) *TLSTransport {
	tcptrans := NewTCPTransport(par)
	tcptrans.transport = TransportTLS //Override transport
	p := &TLSTransport{
		TCPTransport: tcptrans,
	}

	// p.rootPool = roots
	p.tlsConf = dialTLSConf
	p.log = slog.With("caller", "transport<TLS>")
	return p
}

func (t *TLSTransport) String() string {
	return "transport<TLS>"
}

// CreateConnection creates TLS connection for TCP transport
func (t *TLSTransport) CreateConnection(laddr Addr, raddr Addr, handler sip.MessageHandler) (Connection, error) {
	// raddr, err := net.ResolveTCPAddr("tcp", addr)
	// if err != nil {
	// 	return nil, err
	// }

	var tladdr *net.TCPAddr = nil
	if laddr.IP != nil {
		tladdr = &net.TCPAddr{
			IP:   laddr.IP,
			Port: laddr.Port,
		}
	}

	traddr := &net.TCPAddr{
		IP:   raddr.IP,
		Port: raddr.Port,
	}

	return t.createConnection(tladdr, traddr, handler)
}

func (t *TLSTransport) createConnection(laddr *net.TCPAddr, raddr *net.TCPAddr, handler sip.MessageHandler) (Connection, error) {
	addr := raddr.String()
	t.log.Debug("Dialing new connection", "raddr", addr)

	//TODO does this need to be each config
	// SHould we make copy of rootPool?
	// There is Clone of config

	dialer := tls.Dialer{
		NetDialer: &net.Dialer{
			LocalAddr: laddr,
		},
		Config: t.tlsConf,
	}

	conn, err := dialer.DialContext(context.TODO(), "tcp", raddr.String())
	if err != nil {
		return nil, fmt.Errorf("%s dial err=%w", t, err)
	}

	c := t.initConnection(conn, addr, handler)
	return c, nil
}

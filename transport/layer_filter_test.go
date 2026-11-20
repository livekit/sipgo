package transport

import (
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"

	sipgo "github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/require"

	"github.com/livekit/sipgo/fakes"
)

var errFilterTest = errors.New("refused by test filter")

type recordingFilter struct {
	mu    sync.Mutex
	deny  map[netip.Addr]bool
	calls []filterCall
}

type filterCall struct {
	network string
	addr    netip.AddrPort
}

func (f *recordingFilter) filter(network string, addr netip.AddrPort) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, filterCall{network, addr})
	if f.deny[addr.Addr()] {
		return errFilterTest
	}
	return nil
}

func (f *recordingFilter) Calls() []filterCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func newFilterTestLayer(t *testing.T, resolver *net.Resolver, f *recordingFilter) (*Layer, *dialCountingTransport) {
	t.Helper()
	l := NewLayer(slog.Default(), resolver, sipgo.NewParser(), nil, nil)
	t.Cleanup(func() { _ = l.Close() })
	l.AddressFilter = f.filter

	release := make(chan struct{})
	close(release)
	tr := &dialCountingTransport{pool: NewConnectionPool(), release: release}
	for _, network := range []string{"udp", "tcp", "tls", "ws", "wss"} {
		l.transports[network] = tr
	}
	return l, tr
}

// A refused address is not dialed on any transport.
func TestAddressFilterRefusesIPLiteral(t *testing.T) {
	for _, network := range []string{"udp", "tcp", "tls", "ws", "wss"} {
		t.Run(network, func(t *testing.T) {
			f := &recordingFilter{deny: map[netip.Addr]bool{netip.MustParseAddr("127.0.0.1"): true}}
			l, tr := newFilterTestLayer(t, net.DefaultResolver, f)

			req := newTestRequest("127.0.0.1", 5029)
			req.SetTransport(strings.ToUpper(network))
			_, err := l.ClientRequestConnection(req)

			require.ErrorIs(t, err, ErrDestinationRefused)
			require.ErrorIs(t, err, errFilterTest)
			require.EqualValues(t, 0, tr.dials.Load())
			require.Equal(t, []filterCall{{network, netip.MustParseAddrPort("127.0.0.1:5029")}}, f.Calls())
		})
	}
}

// The filter sees the address a hostname resolves to, so a public name that
// points at a refused address is refused.
func TestAddressFilterSeesResolvedAddress(t *testing.T) {
	d := newFakeDNS(t, map[string][]string{"trunk.example.com": {"127.0.0.1"}}, nil)
	f := &recordingFilter{deny: map[netip.Addr]bool{netip.MustParseAddr("127.0.0.1"): true}}
	l, tr := newFilterTestLayer(t, d.resolver(), f)

	req := newTestRequest("trunk.example.com", 5029)
	_, err := l.ClientRequestConnection(req)

	require.ErrorIs(t, err, ErrDestinationRefused)
	require.EqualValues(t, 0, tr.dials.Load())
	require.Equal(t, []filterCall{{"udp", netip.MustParseAddrPort("127.0.0.1:5029")}}, f.Calls())
}

// The same goes for an address that comes from an SRV lookup.
func TestAddressFilterSeesSRVAddress(t *testing.T) {
	const host = "srvonly.example.com"
	d := newFakeDNS(t,
		map[string][]string{"edge.example.com": {"10.0.0.7"}},
		map[string][]fakeSRV{
			"_sip._udp." + host: {{target: "edge.example.com", port: 5062}},
		})
	f := &recordingFilter{deny: map[netip.Addr]bool{netip.MustParseAddr("10.0.0.7"): true}}
	l, tr := newFilterTestLayer(t, d.resolver(), f)

	req := newTestRequest(host, 0)
	_, err := l.ClientRequestConnection(req)

	require.ErrorIs(t, err, ErrDestinationRefused)
	require.EqualValues(t, 0, tr.dials.Load())
	require.Equal(t, []filterCall{{"udp", netip.MustParseAddrPort("10.0.0.7:5062")}}, f.Calls())
}

// An allowed address is dialed as before.
func TestAddressFilterAllows(t *testing.T) {
	f := &recordingFilter{deny: map[netip.Addr]bool{netip.MustParseAddr("127.0.0.1"): true}}
	l, tr := newFilterTestLayer(t, net.DefaultResolver, f)

	req := newTestRequest("192.0.2.10", 5060)
	req.SetTransport("TCP")
	c, err := l.ClientRequestConnection(req)

	require.NoError(t, err)
	require.NotNil(t, c)
	require.EqualValues(t, 1, tr.dials.Load())
	require.Equal(t, []filterCall{{"tcp", netip.MustParseAddrPort("192.0.2.10:5060")}}, f.Calls())
}

// A pooled connection to a refused address, such as one the remote opened to
// us, is not used for a request.
func TestAddressFilterRefusesPooledConnection(t *testing.T) {
	f := &recordingFilter{deny: map[netip.Addr]bool{netip.MustParseAddr("10.0.0.7"): true}}
	l, tr := newFilterTestLayer(t, net.DefaultResolver, f)

	pooled := &TCPConnection{
		Conn: &fakes.TCPConn{
			LAddr: net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 40000},
			RAddr: net.TCPAddr{IP: net.ParseIP("10.0.0.7"), Port: 5060},
		},
		log:      slog.Default(),
		refcount: 1,
	}
	tr.pool.Add("10.0.0.7:5060", pooled)

	req := newTestRequest("10.0.0.7", 5060)
	req.SetTransport("TCP")
	_, err := l.ClientRequestConnection(req)

	require.ErrorIs(t, err, ErrDestinationRefused)
	require.EqualValues(t, 0, tr.dials.Load())
	require.Equal(t, 1, pooled.Ref(0))
}

// Without a filter nothing is checked.
func TestAddressFilterUnset(t *testing.T) {
	f := &recordingFilter{}
	l, tr := newFilterTestLayer(t, net.DefaultResolver, f)
	l.AddressFilter = nil

	req := newTestRequest("127.0.0.1", 5029)
	req.SetTransport("TCP")
	_, err := l.ClientRequestConnection(req)

	require.NoError(t, err)
	require.EqualValues(t, 1, tr.dials.Load())
	require.Empty(t, f.Calls())
}

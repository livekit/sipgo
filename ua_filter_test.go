package sipgo

import (
	"errors"
	"net"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"
)

// WithUserAgentAddressFilter installs the filter on the UA's transport layer.
func TestWithUserAgentAddressFilter(t *testing.T) {
	errRefused := errors.New("refused")
	ua, err := NewUA(
		WithUserAgentIP(net.ParseIP("10.0.0.0")),
		WithUserAgentAddressFilter(func(network string, addr netip.AddrPort) error {
			return errRefused
		}),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ua.Close() })

	f := ua.TransportLayer().AddressFilter
	require.NotNil(t, f)
	require.ErrorIs(t, f("udp", netip.MustParseAddrPort("127.0.0.1:5060")), errRefused)
}

// Without the option, no filter is set.
func TestUserAgentNoAddressFilter(t *testing.T) {
	ua, err := NewUA(WithUserAgentIP(net.ParseIP("10.0.0.0")))
	require.NoError(t, err)
	t.Cleanup(func() { _ = ua.Close() })

	require.Nil(t, ua.TransportLayer().AddressFilter)
}

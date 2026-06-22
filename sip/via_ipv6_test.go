package sip

import (
	"testing"

	sipgo "github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestParseViaIPv6Issue640 is a regression test for livekit/sip#640: inbound
// IPv6 INVITEs (UDP and TCP) were silently dropped because the Via parser
// mistook the inner colons of a bracketed IPv6 host literal for the host:port
// separator. The fix lives in emiago/sipgo's parse_via.go and is pulled in via
// the dependency bump; this test guards against a future downgrade.
func TestParseViaIPv6Issue640(t *testing.T) {
	cases := []struct {
		name     string
		via      string
		wantHost string
		wantPort int
	}{
		{"bracketed IPv6 with port", "SIP/2.0/UDP [2001:db8::1]:5060;branch=z9hG4bKabc", "2001:db8::1", 5060},
		{"bracketed IPv6 without port", "SIP/2.0/UDP [2001:db8::1];branch=z9hG4bKabc", "2001:db8::1", 0},
		{"loopback ::1", "SIP/2.0/UDP [::1];branch=z9hG4bKabc", "::1", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := "INVITE sip:bob@example.com SIP/2.0\r\n" +
				"Via: " + tc.via + "\r\n" +
				"From: <sip:alice@example.com>;tag=1\r\n" +
				"To: <sip:bob@example.com>\r\n" +
				"Call-ID: test-640\r\n" +
				"CSeq: 1 INVITE\r\n" +
				"Content-Length: 0\r\n\r\n"

			msg, err := sipgo.NewParser().ParseSIP([]byte(raw))
			require.NoError(t, err)

			req, ok := msg.(*Request)
			require.True(t, ok)

			via := req.Via()
			require.NotNil(t, via)
			assert.Equal(t, tc.wantHost, via.Host)
			assert.Equal(t, tc.wantPort, via.Port)
		})
	}
}

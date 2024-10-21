package sipgo

import (
	"net"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/livekit/sipgo/sip"
)

func TestClientRequestBuild(t *testing.T) {
	ua, err := NewUA(WithUserAgentIP(net.ParseIP("10.0.0.0")))
	require.Nil(t, err)

	c, err := NewClient(ua)
	require.Nil(t, err)

	recipment := sip.Uri{
		User:      "bob",
		Host:      "10.2.2.2",
		Port:      5060,
		Headers:   sip.HeaderParams{"transport": "udp"},
		UriParams: sip.HeaderParams{"foo": "bar"},
	}

	req := sip.NewRequest(sip.OPTIONS, recipment)
	clientRequestBuildReq(c, req)

	via := req.Via()
	assert.NotNil(t, via)
	assert.Equal(t, "SIP/2.0/UDP 10.0.0.0;branch="+via.Params["branch"], via.Value())

	from := req.From()
	assert.NotNil(t, from)
	// No ports should exists, headers, uriparams should exists, except tag
	assert.Equal(t, "\"sipgo\" <sip:sipgo@10.0.0.0>;tag="+from.Params["tag"], from.Value())

	to := req.To()
	assert.NotNil(t, to)
	// No ports should exists, headers, uriparams should exists
	assert.Equal(t, "<sip:bob@10.2.2.2>", to.Value())

	callid := req.CallID()
	assert.NotNil(t, callid)
	assert.NotEmpty(t, callid.Value())

	cseq := req.CSeq()
	assert.NotNil(t, cseq)
	assert.Equal(t, "1 OPTIONS", cseq.Value())

	maxfwd := req.MaxForwards()
	assert.NotNil(t, maxfwd)
	assert.Equal(t, "70", maxfwd.Value())

	clen := req.ContentLength()
	assert.NotNil(t, clen)
	assert.Equal(t, "0", clen.Value())
}

func TestClientRequestBuildWithHostAndPort(t *testing.T) {
	ua, err := NewUA(WithUserAgentIP(net.ParseIP("10.0.0.0")))
	require.Nil(t, err)

	c, err := NewClient(ua,
		WithClientHostname("sip.myserver.com"),
		WithClientPort(5066),
	)
	require.Nil(t, err)

	recipment := sip.Uri{
		User: "bob",
		Host: "10.2.2.2",
		Port: 5060,
	}

	req := sip.NewRequest(sip.OPTIONS, recipment)
	clientRequestBuildReq(c, req)

	via := req.Via()
	assert.NotNil(t, via)
	assert.Equal(t, "SIP/2.0/UDP sip.myserver.com:5066;branch="+via.Params["branch"], via.Value())

	from := req.From()
	assert.NotNil(t, from)
	// No ports should exists
	assert.Equal(t, "\"sipgo\" <sip:sipgo@sip.myserver.com>;tag="+from.Params["tag"], from.Value())

	to := req.To()
	assert.NotNil(t, to)
	// No port should exists or special values
	assert.Equal(t, "<sip:bob@10.2.2.2>", to.Value())
}

func TestClientRequestOptions(t *testing.T) {
	ua, err := NewUA(WithUserAgentIP(net.ParseIP("10.0.0.0")))
	require.Nil(t, err)

	c, err := NewClient(ua)
	require.Nil(t, err)

	sender := sip.Uri{
		User: "alice",
		Host: "10.1.1.1",
		Port: 5060,
	}

	recipment := sip.Uri{
		User: "bob",
		Host: "10.2.2.2",
		Port: 5060,
	}

	// Proxy receives this request
	req := createSimpleRequest(sip.INVITE, sender, recipment, "UDP")
	oldvia := req.Via()
	assert.Equal(t, "Via: SIP/2.0/UDP 10.1.1.1:5060;branch="+oldvia.Params["branch"], oldvia.String())

	// Proxy will add via header with client host
	err = ClientRequestAddVia(c, req)
	require.Nil(t, err)
	via := req.Via()
	tmpvia := *via // Save this for later usage
	assert.Equal(t, "Via: SIP/2.0/UDP 10.0.0.0;branch="+via.Params["branch"], via.String())
	assert.NotEqual(t, via.Params["branch"], oldvia.Params["branch"])

	// Add Record Route
	err = ClientRequestAddRecordRoute(c, req)
	require.Nil(t, err)
	rr := req.RecordRoute()

	if strings.Contains(";lr;transport=udp", rr.String()) {
		assert.Equal(t, "Record-Route: <sip:10.0.0.0;lr;transport=udp>", rr.String())
	}
	if strings.Contains(";transport=udp;lr", rr.String()) {
		assert.Equal(t, "Record-Route: <sip:10.0.0.0;transport=udp;lr>", rr.String())
	}

	// When proxy gets response, he will remove via
	res := sip.NewResponseFromRequest(req, 400, "", nil)
	res.RemoveHeader("Via")
	viaprev := res.Via()
	assert.Equal(t, oldvia, viaprev)

	// Lets make via multivalue
	req = createSimpleRequest(sip.INVITE, sender, recipment, "UDP")
	via = req.Via()
	req.AppendHeader(&tmpvia)
	res = sip.NewResponseFromRequest(req, 400, "", nil)
	res.RemoveHeader("Via")
	viaprev = res.Via()
	assert.Equal(t, tmpvia.Host, viaprev.Host)

	assert.Len(t, res.GetHeaders("Via"), 1)
}

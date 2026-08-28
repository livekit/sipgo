package transport

import (
	"context"
	"encoding/binary"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	sipgo "github.com/emiago/sipgo/sip"
)

// fakeDNS is a minimal UDP DNS server answering A and SRV queries from a table.
// It exists so the resolver can be driven through a real *net.Resolver, which is
// the only type Layer accepts.
type fakeDNS struct {
	t    *testing.T
	conn *net.UDPConn

	a   map[string][]string  // hostname -> IPv4 addresses
	srv map[string][]fakeSRV // owner name -> records

	mu   sync.Mutex
	seen map[string]int // qname/qtype -> count

	done sync.WaitGroup
}

type fakeSRV struct {
	target string
	port   uint16
}

const (
	typeA   = 1
	typeSRV = 33
)

func newFakeDNS(t *testing.T, a map[string][]string, srv map[string][]fakeSRV) *fakeDNS {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	d := &fakeDNS{t: t, conn: conn, a: a, srv: srv, seen: map[string]int{}}
	d.done.Add(1)
	go d.serve()
	// Closing the socket makes the read in serve fail, which ends the goroutine.
	// Wait for it so it cannot outlive the test.
	t.Cleanup(func() {
		_ = conn.Close()
		d.done.Wait()
	})
	return d
}

// resolver returns a *net.Resolver that queries only this server.
func (d *fakeDNS) resolver() *net.Resolver {
	addr := d.conn.LocalAddr().String()
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "udp", addr)
		},
	}
}

func (d *fakeDNS) queries(name string, qtype uint16) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !strings.HasSuffix(name, ".") {
		name += "."
	}
	return d.seen[strings.ToLower(name)+"/"+string(rune(qtype))]
}

func (d *fakeDNS) serve() {
	defer d.done.Done()
	buf := make([]byte, 512)
	for {
		n, from, err := d.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		resp := d.respond(buf[:n])
		if resp != nil {
			_, _ = d.conn.WriteToUDP(resp, from)
		}
	}
}

// respond parses the question section and builds an answer. Only the subset of
// the wire format these tests need is implemented.
func (d *fakeDNS) respond(q []byte) []byte {
	if len(q) < 12 {
		return nil
	}
	name, off, ok := readName(q, 12)
	if !ok || off+4 > len(q) {
		return nil
	}
	qtype := binary.BigEndian.Uint16(q[off : off+2])
	d.mu.Lock()
	d.seen[strings.ToLower(name)+"/"+string(rune(qtype))]++
	d.mu.Unlock()

	var answers []byte
	var count int
	switch qtype {
	case typeA:
		for _, ip := range d.a[strings.TrimSuffix(name, ".")] {
			answers = append(answers, encodeName(name)...)
			answers = append(answers, rrHeader(typeA, 4)...)
			answers = append(answers, net.ParseIP(ip).To4()...)
			count++
		}
	case typeSRV:
		for _, rec := range d.srv[strings.TrimSuffix(name, ".")] {
			target := encodeName(rec.target)
			rdata := make([]byte, 6, 6+len(target))
			binary.BigEndian.PutUint16(rdata[0:], 5)  // priority
			binary.BigEndian.PutUint16(rdata[2:], 50) // weight
			binary.BigEndian.PutUint16(rdata[4:], rec.port)
			rdata = append(rdata, target...)

			answers = append(answers, encodeName(name)...)
			answers = append(answers, rrHeader(typeSRV, uint16(len(rdata)))...)
			answers = append(answers, rdata...)
			count++
		}
	}

	resp := make([]byte, 12)
	copy(resp, q[:2])                                   // transaction ID
	binary.BigEndian.PutUint16(resp[2:], 0x8180)        // response, recursion available
	binary.BigEndian.PutUint16(resp[4:], 1)             // question count
	binary.BigEndian.PutUint16(resp[6:], uint16(count)) // answer count
	if count == 0 {
		binary.BigEndian.PutUint16(resp[2:], 0x8183) // NXDOMAIN
	}
	resp = append(resp, q[12:off+4]...) // echo the question
	return append(resp, answers...)
}

func rrHeader(qtype, rdlen uint16) []byte {
	b := make([]byte, 10)
	binary.BigEndian.PutUint16(b[0:], qtype)
	binary.BigEndian.PutUint16(b[2:], 1)  // class IN
	binary.BigEndian.PutUint32(b[4:], 60) // TTL
	binary.BigEndian.PutUint16(b[8:], rdlen)
	return b
}

func encodeName(name string) []byte {
	name = strings.TrimSuffix(name, ".")
	var b []byte
	for _, label := range strings.Split(name, ".") {
		b = append(b, byte(len(label)))
		b = append(b, label...)
	}
	return append(b, 0)
}

func readName(msg []byte, off int) (string, int, bool) {
	var sb strings.Builder
	for off < len(msg) {
		n := int(msg[off])
		off++
		if n == 0 {
			return sb.String(), off, true
		}
		if n > 63 || off+n > len(msg) {
			return "", 0, false
		}
		sb.Write(msg[off : off+n])
		sb.WriteByte('.')
		off += n
	}
	return "", 0, false
}

// A carrier layout where two servers sit on different ports behind one hostname
// whose A record lists both of their addresses.
const (
	testSRVHost = "trunk.example.com"
	testSBC1IP  = "198.51.100.11"
	testSBC2IP  = "198.51.100.12"
	testSBC1    = "sbc1.example.net"
	testSBC2    = "sbc2.example.net"
)

func newTwoSBCDNS(t *testing.T, hostA []string) *fakeDNS {
	return newFakeDNS(t,
		map[string][]string{
			testSBC1:    {testSBC1IP},
			testSBC2:    {testSBC2IP},
			testSRVHost: hostA,
		},
		map[string][]fakeSRV{
			"_sip._udp." + testSRVHost: {
				{target: testSBC1, port: 5006},
				{target: testSBC2, port: 5008},
			},
		},
	)
}

func newTestLayer(t *testing.T, d *fakeDNS) *Layer {
	t.Helper()
	l := NewLayer(slog.Default(), d.resolver(), sipgo.NewParser(), nil, nil)
	t.Cleanup(func() { _ = l.Close() })
	return l
}

func resolveWithin(t *testing.T, fn func(ctx context.Context) error) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return fn(ctx)
}

// Both SRV records share a priority and weight, so Go randomises which one wins;
// that is load balancing and is fine. What must never happen is a host from one
// record paired with the port from the other, which sends the request to a port
// that host does not serve.
func TestResolveAddrNeverCrossPairsHostAndPort(t *testing.T) {
	d := newTwoSBCDNS(t, []string{testSBC1IP, testSBC2IP})
	l := newTestLayer(t, d)

	valid := map[string]bool{
		testSBC1IP + ":5006": true,
		testSBC2IP + ":5008": true,
	}
	seen := map[string]bool{}

	// Records of equal priority and weight come back in random order, so repeat
	// until both have won. Either is a fine choice; only the pairing is fixed.
	const rounds = 30
	for i := 0; i < rounds; i++ {
		var addr Addr
		addr.Port = 5060
		err := resolveWithin(t, func(ctx context.Context) error {
			return l.resolveAddr(ctx, "sip", "udp", testSRVHost, &addr)
		})
		if err != nil {
			t.Fatalf("resolveAddr: %v", err)
		}
		got := addr.String()
		if !valid[got] {
			t.Fatalf("got %s, which pairs a host with another record's port", got)
		}
		seen[got] = true
	}

	if len(seen) != 2 {
		t.Fatalf("only saw %v in %d resolutions, so one record went unchecked", seen, rounds)
	}

	// The hostname's own A record lists both servers. Reading it is what paired an
	// address with another record's port, so it must never be consulted here.
	if n := d.queries(testSRVHost, typeA); n != 0 {
		t.Fatalf("the hostname's A record was queried %d times", n)
	}
}

// An explicit port on the request URI means the port was chosen for us, so SRV
// must not be consulted at all. RFC 3263 4.2.
func TestResolveRequestAddrExplicitPortSkipsSRV(t *testing.T) {
	d := newTwoSBCDNS(t, []string{testSBC1IP, testSBC2IP})
	l := newTestLayer(t, d)

	req := sipgo.NewRequest(sipgo.INVITE, sipgo.Uri{Scheme: "sip", User: "1234", Host: testSRVHost, Port: 5006})
	var addr Addr
	addr.Port = 5006
	err := resolveWithin(t, func(ctx context.Context) error {
		return l.resolveRequestAddr(ctx, "udp", testSRVHost, req, &addr)
	})
	if err != nil {
		t.Fatalf("resolveRequestAddr: %v", err)
	}
	if n := d.queries("_sip._udp."+testSRVHost, typeSRV); n != 0 {
		t.Fatalf("SRV was queried %d times despite an explicit port", n)
	}
	if got, want := addr.Port, 5006; got != want {
		t.Fatalf("port %d, want %d", got, want)
	}
}

// Without a port, the same call path does consult SRV.
func TestResolveRequestAddrNoPortUsesSRV(t *testing.T) {
	d := newTwoSBCDNS(t, []string{testSBC1IP, testSBC2IP})
	l := newTestLayer(t, d)

	req := sipgo.NewRequest(sipgo.INVITE, sipgo.Uri{Scheme: "sip", User: "1234", Host: testSRVHost})
	var addr Addr
	addr.Port = 5060 // Destination() default
	err := resolveWithin(t, func(ctx context.Context) error {
		return l.resolveRequestAddr(ctx, "udp", testSRVHost, req, &addr)
	})
	if err != nil {
		t.Fatalf("resolveRequestAddr: %v", err)
	}
	if n := d.queries("_sip._udp."+testSRVHost, typeSRV); n == 0 {
		t.Fatal("SRV was not queried for a URI without a port")
	}
	if addr.Port == 5060 {
		t.Fatalf("port stayed at the default; SRV port was not applied")
	}
}

// A host with no SRV records falls back to a plain lookup, keeping the port the
// caller already had.
func TestResolveAddrFallsBackToHostLookup(t *testing.T) {
	d := newFakeDNS(t, map[string][]string{"plain.example.com": {"192.0.2.10"}}, nil)
	l := newTestLayer(t, d)

	var addr Addr
	addr.Port = 5060
	err := resolveWithin(t, func(ctx context.Context) error {
		return l.resolveAddr(ctx, "sip", "udp", "plain.example.com", &addr)
	})
	if err != nil {
		t.Fatalf("resolveAddr: %v", err)
	}
	if got, want := addr.String(), "192.0.2.10:5060"; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

// An SRV target that does not resolve is an error, not a nil IP passed onward.
func TestResolveAddrSRVTargetMustResolve(t *testing.T) {
	d := newFakeDNS(t, nil, map[string][]fakeSRV{
		"_sip._udp.broken.example.com": {{target: "missing.example.com", port: 5006}},
	})
	l := newTestLayer(t, d)

	var addr Addr
	addr.Port = 5060
	err := resolveWithin(t, func(ctx context.Context) error {
		return l.resolveAddrSRV(ctx, "sip", "udp", "broken.example.com", &addr)
	})
	if err == nil {
		t.Fatalf("expected an error, got addr %s", addr.String())
	}
}

// An explicitly set destination carries a port chosen by whoever set it, so SRV
// must not override it. Mirrors the first branch of Request.Destination().
func TestResolveRequestAddrExplicitDestinationSkipsSRV(t *testing.T) {
	d := newTwoSBCDNS(t, []string{testSBC1IP, testSBC2IP})
	l := newTestLayer(t, d)

	// Recipient carries no port, so only the set destination says 5006.
	req := sipgo.NewRequest(sipgo.INVITE, sipgo.Uri{Scheme: "sip", User: "1234", Host: testSRVHost})
	req.SetDestination(testSRVHost + ":5006")

	var addr Addr
	addr.Port = 5006
	err := resolveWithin(t, func(ctx context.Context) error {
		return l.resolveRequestAddr(ctx, "udp", testSRVHost, req, &addr)
	})
	if err != nil {
		t.Fatalf("resolveRequestAddr: %v", err)
	}
	if n := d.queries("_sip._udp."+testSRVHost, typeSRV); n != 0 {
		t.Fatalf("SRV was queried %d times despite an explicit destination", n)
	}
	if got, want := addr.Port, 5006; got != want {
		t.Fatalf("port %d, want %d", got, want)
	}
}

// ResolveTargets keeps every SRV record, each with its own port, so a caller can
// fail over. This is the list the single-address resolveAddr collapses to one.
func TestResolveTargetsPairsEachRecordWithItsPort(t *testing.T) {
	d := newTwoSBCDNS(t, []string{testSBC1IP, testSBC2IP})
	l := newTestLayer(t, d)

	var targets []Addr
	err := resolveWithin(t, func(ctx context.Context) error {
		var err error
		targets, err = l.ResolveTargets(ctx, "udp", testSRVHost, 0, "sip")
		return err
	})
	if err != nil {
		t.Fatalf("ResolveTargets: %v", err)
	}
	if len(targets) != 2 {
		t.Fatalf("got %d targets, want 2: %v", len(targets), targets)
	}
	// Order varies with SRV weighting; the pairing must not.
	want := map[string]bool{testSBC1IP + ":5006": true, testSBC2IP + ":5008": true}
	for _, tg := range targets {
		if !want[tg.String()] {
			t.Fatalf("%s pairs a host with another record's port", tg.String())
		}
		delete(want, tg.String())
	}
	if len(want) != 0 {
		t.Fatalf("missing targets: %v", want)
	}
}

func TestResolveTargetsExplicitPortSkipsSRV(t *testing.T) {
	d := newTwoSBCDNS(t, []string{testSBC1IP, testSBC2IP})
	l := newTestLayer(t, d)

	var targets []Addr
	err := resolveWithin(t, func(ctx context.Context) error {
		var err error
		targets, err = l.ResolveTargets(ctx, "udp", testSRVHost, 5006, "sip")
		return err
	})
	if err != nil {
		t.Fatalf("ResolveTargets: %v", err)
	}
	if n := d.queries("_sip._udp."+testSRVHost, typeSRV); n != 0 {
		t.Fatalf("SRV queried %d times despite an explicit port", n)
	}

	if len(targets) != 2 {
		t.Fatalf("got %d targets, want 2: %v", len(targets), targets)
	}
	for _, tg := range targets {
		if tg.Port != 5006 {
			t.Fatalf("%s does not use the configured port", tg.String())
		}
	}
}

func TestResolveTargetsIPLiteral(t *testing.T) {
	d := newTwoSBCDNS(t, []string{testSBC1IP})
	l := newTestLayer(t, d)

	var targets []Addr
	err := resolveWithin(t, func(ctx context.Context) error {
		var err error
		targets, err = l.ResolveTargets(ctx, "udp", testSBC1IP, 5006, "sip")
		return err
	})
	if err != nil {
		t.Fatalf("ResolveTargets: %v", err)
	}
	if len(targets) != 1 || targets[0].String() != testSBC1IP+":5006" {
		t.Fatalf("got %v, want [%s:5006]", targets, testSBC1IP)
	}
}

func TestResolveTargetsFallsBackWhenNoSRV(t *testing.T) {
	d := newFakeDNS(t, map[string][]string{"plain.example.com": {"192.0.2.10", "192.0.2.11"}}, nil)
	l := newTestLayer(t, d)

	var targets []Addr
	err := resolveWithin(t, func(ctx context.Context) error {
		var err error
		targets, err = l.ResolveTargets(ctx, "udp", "plain.example.com", 0, "sip")
		return err
	})
	if err != nil {
		t.Fatalf("ResolveTargets: %v", err)
	}
	if len(targets) != 2 {
		t.Fatalf("got %d targets, want 2: %v", len(targets), targets)
	}
	for _, tg := range targets {
		if tg.Port != 5060 {
			t.Fatalf("%s should use the default port", tg.String())
		}
	}
}

// A host with no SRV records must produce an error, not a target.
func TestResolveAddrSRVEmptyAnswerIsAnError(t *testing.T) {
	d := newFakeDNS(t, map[string][]string{"empty.example.com": {"192.0.2.50"}},
		map[string][]fakeSRV{"_sip._udp.empty.example.com": {}})
	l := newTestLayer(t, d)

	var addr Addr
	addr.Port = 5060
	err := resolveWithin(t, func(ctx context.Context) error {
		return l.resolveAddrSRV(ctx, "sip", "udp", "empty.example.com", &addr)
	})
	if err == nil {
		t.Fatalf("expected an error, got %s", addr.String())
	}
}

// A failing SRV lookup must not leave its port behind for the fallback lookup to
// pair with an unrelated address.
func TestResolveAddrSRVFailureLeavesPortAlone(t *testing.T) {
	// SRV points at a target that does not resolve; the host itself does.
	d := newFakeDNS(t,
		map[string][]string{"mixed.example.com": {"192.0.2.60"}},
		map[string][]fakeSRV{
			"_sip._udp.mixed.example.com": {{target: "missing.example.com", port: 5006}},
		})
	l := newTestLayer(t, d)

	var addr Addr
	addr.Port = 5060
	err := resolveWithin(t, func(ctx context.Context) error {
		return l.resolveAddr(ctx, "sip", "udp", "mixed.example.com", &addr)
	})
	if err != nil {
		t.Fatalf("resolveAddr: %v", err)
	}
	// Fell back to the host lookup, so the port must still be the caller's.
	if got, want := addr.String(), "192.0.2.60:5060"; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

// A sip: URI carried over TLS looks up _sips._tcp, not _sip._tcp and not
// _sips._tls.
func TestResolveRequestAddrTLSUsesSipsTCP(t *testing.T) {
	const host = "secure.example.com"
	d := newFakeDNS(t,
		map[string][]string{"edge.example.com": {"192.0.2.70"}},
		map[string][]fakeSRV{
			"_sips._tcp." + host: {{target: "edge.example.com", port: 5061}},
		})
	l := newTestLayer(t, d)

	// Scheme stays sip; the TLS transport is what selects the sips service.
	req := sipgo.NewRequest(sipgo.INVITE, sipgo.Uri{Scheme: "sip", User: "1234", Host: host})
	var addr Addr
	addr.Port = 5061
	err := resolveWithin(t, func(ctx context.Context) error {
		return l.resolveRequestAddr(ctx, "tls", host, req, &addr)
	})
	if err != nil {
		t.Fatalf("resolveRequestAddr: %v", err)
	}
	if got, want := addr.String(), "192.0.2.70:5061"; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
	if n := d.queries("_sip._tcp."+host, typeSRV); n != 0 {
		t.Fatalf("_sip._tcp was queried %d times for a TLS transport", n)
	}
}

// srvLabels is the single place transports become SRV names, for both resolvers.
func TestSRVLabels(t *testing.T) {
	for _, tc := range []struct {
		network, uriScheme string
		want               string // the owner name prefix these labels build
	}{
		{"udp", "sip", "_sip._udp"},
		{"udp4", "sip", "_sip._udp"},
		{"tcp", "sip", "_sip._tcp"},
		{"tls", "sip", "_sips._tcp"}, // RFC 3263 4.1, not _sips._tls
		{"tls", "sips", "_sips._tcp"},
		{"ws", "sip", "_sip._ws"},    // RFC 7118 6
		{"wss", "sip", "_sips._wss"}, // secure transport wins over the scheme
		{"wss", "sips", "_sips._wss"},
	} {
		service, protocol := srvLabels(tc.network, tc.uriScheme)
		if got := "_" + service + "._" + protocol; got != tc.want {
			t.Errorf("srvLabels(%q, %q) = %s, want %s", tc.network, tc.uriScheme, got, tc.want)
		}
	}
}

// A WebSocket transport resolves through _sip._ws rather than _sip._tcp.
func TestResolveRequestAddrWSUsesSipWS(t *testing.T) {
	const host = "ws.example.com"
	d := newFakeDNS(t,
		map[string][]string{"edge.example.com": {"192.0.2.80"}},
		map[string][]fakeSRV{
			"_sip._ws." + host: {{target: "edge.example.com", port: 8080}},
		})
	l := newTestLayer(t, d)

	req := sipgo.NewRequest(sipgo.INVITE, sipgo.Uri{Scheme: "sip", User: "1234", Host: host})
	var addr Addr
	addr.Port = 5060
	err := resolveWithin(t, func(ctx context.Context) error {
		return l.resolveRequestAddr(ctx, "ws", host, req, &addr)
	})
	if err != nil {
		t.Fatalf("resolveRequestAddr: %v", err)
	}
	if got, want := addr.String(), "192.0.2.80:8080"; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
	if n := d.queries("_sip._tcp."+host, typeSRV); n != 0 {
		t.Fatalf("_sip._tcp was queried %d times for a ws transport", n)
	}
}

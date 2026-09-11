package transport

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	sipgo "github.com/emiago/sipgo/sip"
)

// These tests pin behaviour of the upstream parser, not of this package. The
// transport decides when to close a connection based on what the parser does
// with a stream it cannot frame, so a parser upgrade that changes any of this
// should fail here rather than go unnoticed.

// invite builds a well formed INVITE with a body of the given size.
func invite(callID string, bodySize int) string {
	body := strings.Repeat("x", bodySize)
	return "INVITE sip:bob@example.com SIP/2.0\r\n" +
		"Via: SIP/2.0/TCP 10.0.0.1:5060;branch=z9hG4bK." + callID + "\r\n" +
		"From: <sip:alice@example.com>;tag=" + callID + "\r\n" +
		"To: <sip:bob@example.com>\r\n" +
		"Call-ID: " + callID + "\r\n" +
		"CSeq: 1 INVITE\r\n" +
		fmt.Sprintf("Content-Length: %d\r\n", len(body)) +
		"\r\n" + body
}

// collect returns a callback that records parsed messages.
func collect(msgs *[]sipgo.Message) func(sipgo.Message) {
	return func(m sipgo.Message) { *msgs = append(*msgs, m) }
}

func newTestStream(t *testing.T) (*sipgo.Parser, *sipgo.ParserStream) {
	t.Helper()
	par := sipgo.NewParser()
	st := par.NewSIPStream()
	t.Cleanup(st.Close)
	return par, st
}

// A message that stops mid write does not stop the parser from framing later
// messages. The fragment merges with whatever arrives next, one corrupted
// message comes out carrying headers from both, and the stream realigns on its
// own.
//
// Nothing accumulates here, so no size bound can catch this case. It is the
// reason the transport cannot detect every desync and why the close reason
// counter is what sizes how often each mechanism fires.
func TestUpstreamParserTruncationMergesThenRealigns(t *testing.T) {
	_, st := newTestStream(t)
	var msgs []sipgo.Message

	// A message that stops inside a header value, before its Content-Length.
	truncated := "INVITE sip:bob@example.com SIP/2.0\r\n" +
		"Via: SIP/2.0/TCP 10.0.0.1:5060;branch=z9hG4bK.trunc\r\n" +
		"Call-ID: TRUNCATED\r\n" +
		"CSeq: 1 INVITE\r\n" +
		"Min-SE:"
	if err := st.ParseSIPStream([]byte(truncated), collect(&msgs)); !errors.Is(err, sipgo.ErrParseSipPartial) {
		t.Fatalf("a truncated message alone should look partial, got: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("expected no messages yet, got %d", len(msgs))
	}

	// The next message completes the fragment instead of being parsed on its
	// own, so it is consumed and its content is lost.
	if err := st.ParseSIPStream([]byte(invite("VICTIM", 40)), collect(&msgs)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 merged message, got %d", len(msgs))
	}
	// The merged message has a mixed identity: the routing of the message that
	// was cut off, and the Call-ID of the one that completed it. A response
	// shaped like this cannot be matched to the transaction that is waiting for
	// it, which is how a call ends up timing out with no final response.
	merged := msgs[0]
	via := merged.Via()
	if via == nil {
		t.Fatal("merged message has no Via")
	}
	if !strings.Contains(via.Value(), "z9hG4bK.trunc") {
		t.Fatalf("expected the truncated message's branch in Via, got %q", via.Value())
	}
	if got := merged.CallID().Value(); got != "VICTIM" {
		t.Fatalf("expected the completing message's Call-ID, got %q", got)
	}

	// From here the stream is aligned again and later messages are intact.
	msgs = msgs[:0]
	for i := 0; i < 3; i++ {
		if err := st.ParseSIPStream([]byte(invite(fmt.Sprintf("AFTER%d", i), 40)), collect(&msgs)); err != nil {
			t.Fatalf("round %d: unexpected error: %v", i, err)
		}
	}
	if len(msgs) != 3 {
		t.Fatalf("expected 3 clean messages after realignment, got %d", len(msgs))
	}
	for i, m := range msgs {
		want := fmt.Sprintf("AFTER%d", i)
		if got := m.CallID().Value(); got != want {
			t.Fatalf("message %d: expected Call-ID %q, got %q", i, want, got)
		}
	}
}

// The parser bounds an incomplete message itself, on the sum of what it has
// already drained and what is still buffered. This is what replaced a bound
// tracked in this package, so it has to keep working across upgrades.
func TestUpstreamParserBoundsIncompleteMessage(t *testing.T) {
	par, st := newTestStream(t)
	var msgs []sipgo.Message

	// A valid start line first, so the filler below is read as headers rather
	// than as a start line.
	if err := st.ParseSIPStream([]byte("INVITE sip:bob@example.com SIP/2.0\r\n"), collect(&msgs)); !errors.Is(err, sipgo.ErrParseSipPartial) {
		t.Fatalf("unexpected error on start line: %v", err)
	}

	// Header lines that never terminate the message. Each one is complete, so
	// the parser drains it and its own buffer stays near empty. Only the sum
	// bounds this.
	filler := "X-Pad: " + strings.Repeat("y", 900) + "\r\n"
	var err error
	for sent := 0; sent <= par.MaxMessageLength+len(filler); sent += len(filler) {
		if err = st.ParseSIPStream([]byte(filler), collect(&msgs)); !errors.Is(err, sipgo.ErrParseSipPartial) {
			break
		}
	}
	if !errors.Is(err, sipgo.ErrMessageTooLarge) {
		t.Fatalf("expected ErrMessageTooLarge, got: %v", err)
	}
	if got := closeReason(err); got != "message_too_large" {
		t.Fatalf("expected reason message_too_large, got %q", got)
	}
	if len(msgs) != 0 {
		t.Fatalf("expected no messages, got %d", len(msgs))
	}
}

// A CR with no LF is the case the parser cannot recover from and cannot bound.
// nextLine reports the offending byte's position but neither parseStartLine nor
// parseNextHeader returns that count, so the parser advances by zero and every
// later read re-hits the same byte. The size bound above never applies, because
// it only runs on the path that knows the message is incomplete.
//
// So the buffer grows without limit while nothing is ever framed, which is why
// the transport has to close on this itself.
func TestUpstreamParserBareCRDoesNotAdvance(t *testing.T) {
	_, st := newTestStream(t)
	var msgs []sipgo.Message

	bare := "INVITE sip:bob@example.com SIP/2.0\rVia: SIP/2.0/TCP 10.0.0.1:5060\r\n\r\n"
	err := st.ParseSIPStream([]byte(bare), collect(&msgs))
	if !errors.Is(err, sipgo.ErrParseLineNoCRLF) {
		t.Fatalf("expected ErrParseLineNoCRLF, got: %v", err)
	}
	if got := closeReason(err); got != "no_crlf" {
		t.Fatalf("expected reason no_crlf, got %q", got)
	}

	// Well formed messages arriving afterwards are neither parsed nor dropped,
	// they just pile up behind the byte the parser will not move past.
	buffered := st.Buffer().Len()
	for i := 0; i < 4; i++ {
		err = st.ParseSIPStream([]byte(invite(fmt.Sprintf("AFTER%d", i), 40)), collect(&msgs))
		if !errors.Is(err, sipgo.ErrParseLineNoCRLF) {
			t.Fatalf("round %d: expected the same error, got: %v", i, err)
		}
		grown := st.Buffer().Len()
		if grown <= buffered {
			t.Fatalf("round %d: expected the buffer to keep growing, %d then %d", i, buffered, grown)
		}
		buffered = grown
	}
	if len(msgs) != 0 {
		t.Fatalf("expected no messages to be framed, got %d", len(msgs))
	}
}

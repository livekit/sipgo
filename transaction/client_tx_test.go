package transaction

import (
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/livekit/sipgo/sip"
	"github.com/stretchr/testify/require"
)

// writeHookConn runs onWrite inside WriteMsg, as when the transport's read goroutine handles
// the response before the goroutine in Init has returned from WriteMsg.
type writeHookConn struct{ onWrite func() }

func (c *writeHookConn) LocalAddr() net.Addr { return &net.UDPAddr{} }
func (c *writeHookConn) WriteMsg(sip.Message) error {
	if c.onWrite != nil {
		c.onWrite()
	}
	return nil
}
func (c *writeHookConn) Ref(int) int            { return 1 }
func (c *writeHookConn) TryClose() (int, error) { return 1, nil }
func (c *writeHookConn) Close() error           { return nil }

func TestClientTxFinalResponseDuringInitUDP(t *testing.T) {
	for range 100 {
		req := sip.NewRequest(sip.REFER, sip.Uri{Host: "peer", Port: 5060})
		params := sip.NewParams()
		params.Add("branch", sip.GenerateBranch())
		req.AppendHeader(&sip.ViaHeader{ProtocolName: "SIP", ProtocolVersion: "2.0", Transport: "UDP", Host: "me", Port: 5060, Params: params})
		req.AppendHeader(&sip.CSeqHeader{SeqNo: 2, MethodName: sip.REFER})
		req.SetTransport("UDP")
		res := sip.NewResponseFromRequest(req, 202, "Accepted", nil)

		conn := &writeHookConn{}
		tx := NewClientTx("key", req, conn, slog.Default())
		tx.OnTerminate(func(string) {})
		received := make(chan struct{})
		conn.onWrite = func() {
			go func() {
				_ = tx.Receive(res)
				close(received)
			}()
			select {
			case <-received:
			case <-time.After(5 * time.Millisecond):
			}
		}
		require.NoError(t, tx.Init())
		<-received

		select {
		case <-tx.Done():
			t.Fatal("transaction deleted on a 202 that arrived while Init was writing the request")
		default:
		}
		r := <-tx.Responses()
		require.NotNil(t, r)
		require.Equal(t, 202, r.StatusCode)
		tx.Terminate()
	}
}

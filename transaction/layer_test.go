package transaction

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/require"

	"github.com/livekit/sipgo/fakes"
	lksip "github.com/livekit/sipgo/sip"
	"github.com/livekit/sipgo/transport"
)

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testRequest(method lksip.RequestMethod, branch string) *lksip.Request {
	req := lksip.NewRequest(method, lksip.Uri{Scheme: "sip", Host: "10.0.0.8", Port: 5060})
	params := lksip.NewParams()
	params.Add("branch", branch)
	req.AppendHeader(&lksip.ViaHeader{
		ProtocolName:    "SIP",
		ProtocolVersion: "2.0",
		Transport:       "TCP",
		Host:            "10.9.9.9",
		Port:            5060,
		Params:          params,
	})
	fromParams := lksip.NewParams()
	fromParams.Add("tag", "from")
	req.AppendHeader(&lksip.FromHeader{
		Address: lksip.Uri{User: "a", Host: "example.com"},
		Params:  fromParams,
	})
	req.AppendHeader(&lksip.ToHeader{
		Address: lksip.Uri{User: "b", Host: "example.com"},
		Params:  lksip.NewParams(),
	})
	callID := lksip.CallIDHeader("call-1")
	req.AppendHeader(&callID)
	req.AppendHeader(&lksip.CSeqHeader{SeqNo: 1, MethodName: method})
	req.SetTransport("TCP")
	return req
}

func TestServerTxKeyKeepsAckAndCancel(t *testing.T) {
	branch := lksip.GenerateBranch()
	invite := testRequest(lksip.INVITE, branch)
	cancel := testRequest(lksip.CANCEL, branch)
	ack := testRequest(lksip.ACK, branch)

	inviteKey, err := MakeServerTxKey(invite)
	require.NoError(t, err)
	cancelKey, err := MakeServerTxKey(cancel)
	require.NoError(t, err)
	ackKey, err := MakeServerTxKey(ack)
	require.NoError(t, err)

	require.NotEqual(t, inviteKey, cancelKey)
	require.NotEqual(t, inviteKey, ackKey)
	require.True(t, strings.HasSuffix(cancelKey, string(lksip.CANCEL)))
	require.True(t, strings.HasSuffix(ackKey, string(lksip.ACK)))

	asInvite, err := makeServerTxKey(cancel, lksip.INVITE, false)
	require.NoError(t, err)
	require.Equal(t, inviteKey, asInvite)
	asInvite, err = makeServerTxKey(ack, lksip.INVITE, false)
	require.NoError(t, err)
	require.Equal(t, inviteKey, asInvite)
}

// A CANCEL or ACK with the INVITE's branch is that transaction: the CANCEL is answered
// 200 and the INVITE is terminated 487, and neither is delivered as its own request.
// One whose branch matches nothing is a CANCEL or ACK transaction of its own.
func TestAckAndCancelMatchInviteOrStandAlone(t *testing.T) {
	log := discardLog()
	tpl := transport.NewLayer(log, nil, sip.NewParser(), nil, nil)
	txl := NewLayer(log, tpl)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer lis.Close()
	go tpl.ServeTCP(lis)

	client, err := net.Dial("tcp", lis.Addr().String())
	require.NoError(t, err)
	defer client.Close()

	var got []byte
	var mu sync.Mutex
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
			n, err := client.Read(buf)
			if n > 0 {
				mu.Lock()
				got = append(got, buf[:n]...)
				mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()

	var conn transport.Connection
	require.Eventually(t, func() bool {
		c, err := tpl.GetConnection("tcp", client.LocalAddr().String())
		if err != nil || c == nil {
			return false
		}
		conn = c
		return true
	}, time.Second, 5*time.Millisecond)

	branch := lksip.GenerateBranch()
	invite := testRequest(lksip.INVITE, branch)
	invite.SetSource(client.LocalAddr().String())
	key, err := makeServerTxKey(invite, lksip.INVITE, false)
	require.NoError(t, err)
	invTx := NewServerTx(key, invite, conn, log)
	invTx.OnTerminate(txl.serverTxTerminate)
	require.NoError(t, invTx.Init())
	txl.serverTransactions.put(invTx.Key(), invTx)
	t.Cleanup(invTx.Terminate)

	var methods []lksip.RequestMethod
	txl.OnRequest(func(_ *slog.Logger, req *lksip.Request, tx lksip.ServerTransaction) {
		methods = append(methods, req.Method)
		t.Cleanup(tx.Terminate)
	})

	cancel := testRequest(lksip.CANCEL, branch)
	cancel.SetSource(client.LocalAddr().String())
	txl.handleRequest(cancel)

	ack := testRequest(lksip.ACK, branch)
	ack.SetSource(client.LocalAddr().String())
	txl.handleRequest(ack)

	other := lksip.GenerateBranch()
	loneCancel := testRequest(lksip.CANCEL, other)
	loneCancel.SetSource(client.LocalAddr().String())
	txl.handleRequest(loneCancel)
	loneAck := testRequest(lksip.ACK, other)
	loneAck.SetSource(client.LocalAddr().String())
	txl.handleRequest(loneAck)

	require.Equal(t, []lksip.RequestMethod{lksip.CANCEL, lksip.ACK}, methods)

	loneCancelKey, err := MakeServerTxKey(loneCancel)
	require.NoError(t, err)
	_, ok := txl.getServerTx(loneCancelKey)
	require.True(t, ok)
	asInvite, err := makeServerTxKey(loneCancel, lksip.INVITE, false)
	require.NoError(t, err)
	_, ok = txl.getServerTx(asInvite)
	require.False(t, ok)

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		s := string(got)
		ok200, ok487 := strings.Index(s, "SIP/2.0 200"), strings.Index(s, "SIP/2.0 487")
		return ok200 >= 0 && ok487 == -1 // Must not generate 487 from sipgo
	}, 15*time.Millisecond, 1*time.Millisecond)
}

func TestServerTxKeyIgnoreViaHost(t *testing.T) {
	branch := lksip.GenerateBranch()
	a := testRequest(lksip.INVITE, branch)
	b := testRequest(lksip.INVITE, branch)
	b.Via().Host = "10.9.9.10"

	keyA, err := makeServerTxKey(a, "", false)
	require.NoError(t, err)
	keyB, err := makeServerTxKey(b, "", false)
	require.NoError(t, err)
	require.NotEqual(t, keyA, keyB)

	keyA, err = makeServerTxKey(a, "", true)
	require.NoError(t, err)
	keyB, err = makeServerTxKey(b, "", true)
	require.NoError(t, err)
	require.Equal(t, keyA, keyB)

	b.Via().Host = a.Via().Host
	b.Via().Port = a.Via().Port + 1
	keyB, err = makeServerTxKey(b, "", true)
	require.NoError(t, err)
	require.Equal(t, keyA, keyB)
}

// An INVITE forwarded by one stateless proxy instance, and its CANCEL and non-2xx ACK
// forwarded by another: same branch, different top Via host and port. With
// IgnoreViaAddr they belong to the INVITE transaction; without, each is its own.
func TestIgnoreViaAddrMatchesInvite(t *testing.T) {
	proxyA := net.UDPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 5060}
	proxyB := net.UDPAddr{IP: net.IPv4(10, 0, 0, 2), Port: 5070}
	for _, ignore := range []bool{false, true} {
		t.Run(fmt.Sprintf("ignore=%v", ignore), func(t *testing.T) {
			log := discardLog()
			tpl := transport.NewLayer(log, nil, sip.NewParser(), nil, nil)
			txl := NewLayer(log, tpl)
			if ignore {
				txl.IgnoreViaAddr()
			}
			// Serve UDP over a fake connection, so any source address has a connection.
			idle, idleW := io.Pipe()
			t.Cleanup(func() { idleW.Close() })
			go tpl.ServeUDP(&fakes.UDPConn{
				LAddr:   net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 5060},
				Reader:  idle,
				Writers: map[string]io.Writer{proxyA.String(): io.Discard, proxyB.String(): io.Discard},
			})
			require.Eventually(t, func() bool {
				c, err := tpl.GetConnection("udp", proxyA.String())
				return err == nil && c != nil
			}, time.Second, time.Millisecond)

			txs := make(chan lksip.ServerTransaction, 4)
			txl.OnRequest(func(_ *slog.Logger, req *lksip.Request, tx lksip.ServerTransaction) {
				txs <- tx
				t.Cleanup(tx.Terminate)
			})
			forward := func(method lksip.RequestMethod, branch string, proxy net.UDPAddr) {
				req := testRequest(method, branch)
				req.SetTransport("UDP")
				req.Via().Transport = "UDP"
				req.Via().Host = proxy.IP.String()
				req.Via().Port = proxy.Port
				req.SetSource(proxy.String())
				txl.handleRequest(req)
			}

			branch := lksip.GenerateBranch()
			forward(lksip.INVITE, branch, proxyA)
			invTx := (<-txs).(*ServerTx)

			forward(lksip.CANCEL, branch, proxyB)
			if !ignore {
				require.Equal(t, lksip.CANCEL, (<-txs).(*ServerTx).Origin().Method)
				return
			}
			select {
			case <-invTx.Cancels():
			case <-time.After(time.Second):
				require.FailNow(t, "CANCEL not delivered to the INVITE transaction")
			}

			require.NoError(t, invTx.Respond(lksip.NewResponseFromRequest(invTx.Origin(), lksip.StatusRequestTerminated, "Request Terminated", nil)))
			forward(lksip.ACK, branch, proxyA)
			select {
			case <-invTx.Acks():
			case <-time.After(time.Second):
				require.FailNow(t, "ACK not delivered to the INVITE transaction")
			}
			require.Empty(t, txs, "neither request is a transaction of its own")
		})
	}
}

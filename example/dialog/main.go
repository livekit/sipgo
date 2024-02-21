package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

func main() {
	extIP := flag.String("ip", "127.0.0.1:5060", "My exernal ip")
	dst := flag.String("dst", "127.0.0.2:5060", "Destination pbx, sip server")
	flag.Parse()

	lvl := slog.LevelInfo
	slog.SetDefault(slog.New(
		slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}),
	))

	ua, _ := sipgo.NewUA()

	srv, err := sipgo.NewServerDialog(ua)
	if err != nil {
		panic(fmt.Errorf("Fail to setup dialog server: %w", err))
	}
	client, err := sipgo.NewClient(ua)
	if err != nil {
		panic(fmt.Errorf("Fail to setup dialog server: %w", err))
	}

	h := &Handler{
		srv,
		client,
		*dst,
	}

	setupRoutes(srv, h)

	slog.Info("Starting server", "ip", *extIP, "dst", *dst)
	if err := srv.ListenAndServe(context.TODO(), "udp", *extIP); err != nil {
		slog.Error("Fail to serve", "err", err)
	}
}

func setupRoutes(srv *sipgo.ServerDialog, h *Handler) {
	srv.OnInvite(h.route)
	srv.OnAck(h.route)
	srv.OnCancel(h.route)
	srv.OnBye(h.route)

	srv.OnDialog(h.onDialog)
}

type Handler struct {
	s   *sipgo.ServerDialog
	c   *sipgo.Client
	dst string
}

func (s *Handler) proxyDestination() string {
	return s.dst
}

// onDialog is our main function for handling dialogs
func (h *Handler) onDialog(d sip.Dialog) {
	slog.Info("New dialog <--", "state", d.StateString(), "ID", d.ID)
	switch d.State {
	case sip.DialogStateEstablished:
		// 200 response
	case sip.DialogStateConfirmed:
		// ACK send
	case sip.DialogStateEnded:
		// BYE send
	}
}

// route is our main route function to proxy to our dst
func (h *Handler) route(req *sip.Request, tx sip.ServerTransaction) {
	dst := h.proxyDestination()
	req.SetDestination(dst)
	// Handle 200 Ack
	if req.IsAck() {
		if err := h.c.WriteRequest(req); err != nil {
			slog.Error("Send failed", "err", err)
			reply(tx, req, 500, "")
			return
		}
		return
	}

	// Start client transaction and relay our request
	clTx, err := h.c.TransactionRequest(req, sipgo.ClientRequestAddVia, sipgo.ClientRequestAddRecordRoute)
	if err != nil {
		slog.Error("RequestWithContext  failed", "err", err)
		reply(tx, req, 500, "")
		return
	}
	defer clTx.Terminate()

	for {
		select {
		case res, more := <-clTx.Responses():
			if !more {
				return
			}
			res.SetDestination(req.Source())
			res.RemoveHeader("Via")
			if err := tx.Respond(res); err != nil {
				slog.Error("ResponseHandler transaction respond failed", "err", err)
			}

		case m := <-tx.Cancels():
			// Send response imediatelly
			reply(tx, m, 200, "OK")
			// Cancel client transacaction without waiting. This will send CANCEL request
			clTx.Cancel()

		case <-tx.Done():
			if err := tx.Err(); err != nil {
				slog.Error("Transaction done with error", "err", err, "req", req.Method.String())
				return
			}
			slog.Debug("Transaction done", "req", req.Method.String())
			return
		case <-clTx.Done():
			if err := clTx.Err(); err != nil {
				slog.Error("Transaction done with error", "err", err, "req", req.Method.String())
				return
			}
			slog.Debug("Client Transaction done", "err", err, "req", req.Method.String())
			return
		}
	}
}

func reply(tx sip.ServerTransaction, req *sip.Request, code sip.StatusCode, reason string) {
	resp := sip.NewResponseFromRequest(req, code, reason, nil)
	resp.SetDestination(req.Source()) //This is optional, but can make sure not wrong via is read
	if err := tx.Respond(resp); err != nil {
		slog.Error("Fail to respond on transaction", "err", err)
	}
}

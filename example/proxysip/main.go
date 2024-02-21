package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"runtime"
	"strconv"

	"github.com/emiago/sipgo/sip"
	"github.com/emiago/sipgo/transport"

	"github.com/emiago/sipgo"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	// _ "go.uber.org/automaxprocs"
)

var ()

func main() {
	debflag := flag.Bool("debug", false, "")
	pprof := flag.Bool("pprof", false, "Full profile")
	extIP := flag.String("ip", "127.0.0.1:5060", "My exernal ip")
	dst := flag.String("dst", "", "Destination pbx, sip server")
	transportType := flag.String("t", "udp", "Transport, default will be determined by request")
	flag.Parse()

	transport.UDPMTUSize = 10000
	if *pprof {
		runtime.SetBlockProfileRate(1)
		runtime.SetMutexProfileFraction(1)
		runtime.MemProfileRate = 64
	}

	lvl := slog.LevelInfo
	if *debflag || os.Getenv("LOGDEBUG") != "" {
		lvl = slog.LevelDebug
		transport.SIPDebug = true
	}
	slog.SetDefault(slog.New(
		slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}),
	))

	slog.Info("Runtime", "cpus", runtime.NumCPU())
	slog.Info("Server routes setuped")
	go httpServer(":8080")

	srv := setupSipProxy(*dst, *extIP)
	if err := srv.ListenAndServe(context.TODO(), *transportType, *extIP); err != nil {
		slog.Error("Fail to start sip server", "err", err)
		return
	}
}

func httpServer(address string) {
	http.Handle("/metrics", promhttp.Handler())
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("Alive"))
	})

	http.HandleFunc("/mem", func(w http.ResponseWriter, r *http.Request) {
		runtime.GC()
		stats := &runtime.MemStats{}
		runtime.ReadMemStats(stats)
		data, _ := json.MarshalIndent(stats, "", "  ")
		w.WriteHeader(200)
		w.Write(data)
	})

	slog.Info("Http server started", "address", address)
	http.ListenAndServe(address, nil)
}

func setupSipProxy(proxydst string, ip string) *sipgo.Server {
	// Prepare all variables we need for our service
	host, port, _ := sip.ParseAddr(ip)
	ua, err := sipgo.NewUA(
		sipgo.WithUserAgentIP(net.ParseIP(host)),
	)
	if err != nil {
		panic(fmt.Errorf("Fail to setup user agent: %w", err))
	}

	srv, err := sipgo.NewServer(ua)
	if err != nil {
		panic(fmt.Errorf("Fail to setup server handle: %w", err))
	}

	client, err := sipgo.NewClient(ua)
	if err != nil {
		panic(fmt.Errorf("Fail to setup client handle: %w", err))
	}

	registry := NewRegistry()
	var getDestination = func(req *sip.Request) string {
		tohead, _ := req.To()
		dst := registry.Get(tohead.Address.User)

		if dst == "" {
			return proxydst
		}

		return dst
	}

	var reply = func(tx sip.ServerTransaction, req *sip.Request, code sip.StatusCode, reason string) {
		resp := sip.NewResponseFromRequest(req, code, reason, nil)
		resp.SetDestination(req.Source()) //This is optional, but can make sure not wrong via is read
		if err := tx.Respond(resp); err != nil {
			slog.Error("Fail to respond on transaction", "err", err)
		}
	}

	var route = func(req *sip.Request, tx sip.ServerTransaction) {
		// If we are proxying to asterisk or other proxy -dst must be set
		// Otherwise we will look on our registration entries
		dst := getDestination(req)

		if dst == "" {
			reply(tx, req, 404, "Not found")
			return
		}

		req.SetDestination(dst)
		// Start client transaction and relay our request
		clTx, err := client.TransactionRequest(req, sipgo.ClientRequestAddVia, sipgo.ClientRequestAddRecordRoute)
		if err != nil {
			slog.Error("RequestWithContext  failed", "err", err)
			reply(tx, req, 500, "")
			return
		}
		defer clTx.Terminate()

		// Keep monitoring transactions, and proxy client responses to server transaction
		slog.Debug("Starting transaction", "req", req.Method.String())
		for {
			select {

			case res, more := <-clTx.Responses():
				if !more {
					return
				}

				res.SetDestination(req.Source())

				// https://datatracker.ietf.org/doc/html/rfc3261#section-16.7
				// Based on section removing via. Topmost via should be removed and check that exist

				// Removes top most header
				res.RemoveHeader("Via")
				if err := tx.Respond(res); err != nil {
					slog.Error("ResponseHandler transaction respond failed", "err", err)
				}

				// Early terminate
				// if req.Method == sip.BYE {
				// 	// We will call client Terminate
				// 	return
				// }

			case m := <-tx.Acks():
				// Acks can not be send directly trough destination
				slog.Info("Proxing ACK", "dst", dst, "m", m.StartLine())
				m.SetDestination(dst)
				client.WriteRequest(m)

			case m := <-tx.Cancels():
				// Send response imediatelly
				reply(tx, m, 200, "OK")
				// Cancel client transacaction without waiting. This will send CANCEL request
				clTx.Cancel()

			case <-tx.Done():
				if err := tx.Err(); err != nil {
					slog.Error("Transaction done with error", "req", req.Method.String(), "err", err)
					return
				}
				slog.Debug("Transaction done", "req", req.Method.String())
				return
			}
		}
	}

	var registerHandler = func(req *sip.Request, tx sip.ServerTransaction) {
		// https://www.rfc-editor.org/rfc/rfc3261#section-10.3
		cont, exists := req.Contact()
		if !exists {
			reply(tx, req, 404, "Missing address of record")
			return
		}

		// We have a list of uris
		uri := cont.Address
		if uri.Host == host && uri.Port == port {
			reply(tx, req, 401, "Contact address not provided")
			return
		}

		addr := uri.Host + ":" + strconv.Itoa(uri.Port)

		registry.Add(uri.User, addr)
		slog.Debug("Contact added", "user", uri.User, "addr", addr)

		res := sip.NewResponseFromRequest(req, 200, "OK", nil)
		// slog.Debug().Msgf("Sending response: \n%s", res.String())

		// URI params must be reset or this should be regenetad
		cont.Address.UriParams = sip.NewParams()
		cont.Address.UriParams.Add("transport", req.Transport())

		if err := tx.Respond(res); err != nil {
			slog.Error("Sending REGISTER OK failed", "err", err)
			return
		}
	}

	var inviteHandler = func(req *sip.Request, tx sip.ServerTransaction) {
		route(req, tx)
	}

	var ackHandler = func(req *sip.Request, tx sip.ServerTransaction) {
		dst := getDestination(req)
		if dst == "" {
			return
		}
		req.SetDestination(dst)
		if err := client.WriteRequest(req, sipgo.ClientRequestAddVia); err != nil {
			slog.Error("Send failed", "err", err)
			reply(tx, req, 500, "")
		}
	}

	var cancelHandler = func(req *sip.Request, tx sip.ServerTransaction) {
		route(req, tx)
	}

	var byeHandler = func(req *sip.Request, tx sip.ServerTransaction) {
		route(req, tx)
	}

	srv.OnRegister(registerHandler)
	srv.OnInvite(inviteHandler)
	srv.OnAck(ackHandler)
	srv.OnCancel(cancelHandler)
	srv.OnBye(byeHandler)
	return srv
}

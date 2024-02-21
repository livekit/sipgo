package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/parser"
	"github.com/emiago/sipgo/sip"
	"github.com/emiago/sipgo/transport"

	"github.com/icholy/digest"
)

func main() {
	inter := flag.String("h", "localhost", "My interface ip or hostname")
	dst := flag.String("srv", "127.0.0.1:5060", "Destination")
	tran := flag.String("t", "udp", "Transport")
	username := flag.String("u", "alice", "SIP Username")
	password := flag.String("p", "alice", "Password")
	flag.Parse()

	// Make SIP Debugging available
	transport.SIPDebug = os.Getenv("SIP_DEBUG") != ""

	lvl := slog.LevelInfo
	if s := os.Getenv("LOG_LEVEL"); s != "" {
		lvl.UnmarshalText([]byte(s))
	}
	slog.SetDefault(slog.New(
		slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}),
	))

	// Setup UAC
	ua, err := sipgo.NewUA(
		sipgo.WithUserAgent(*username),
	)
	if err != nil {
		panic(fmt.Errorf("Fail to setup user agent: %w", err))
	}

	client, err := sipgo.NewClient(ua, sipgo.WithClientHostname(*inter))
	if err != nil {
		panic(fmt.Errorf("Fail to setup client handle: %w", err))
	}
	defer client.Close()

	// Create basic REGISTER request structure
	recipient := &sip.Uri{}
	parser.ParseUri(fmt.Sprintf("sip:%s@%s", *username, *dst), recipient)
	req := sip.NewRequest(sip.REGISTER, recipient)
	req.AppendHeader(
		sip.NewHeader("Contact", fmt.Sprintf("<sip:%s@%s>", *username, *inter)),
	)
	req.SetTransport(strings.ToUpper(*tran))

	// Send request and parse response
	// req.SetDestination(*dst)
	slog.Info(req.StartLine())
	tx, err := client.TransactionRequest(req)
	if err != nil {
		panic(fmt.Errorf("Fail to create transaction: %w", err))
	}
	defer tx.Terminate()

	res, err := getResponse(tx)
	if err != nil {
		panic(fmt.Errorf("Fail to get response: %w", err))
	}

	slog.Info("Received status", "status", int(res.StatusCode))
	if res.StatusCode == 401 {
		// Get WwW-Authenticate
		wwwAuth := res.GetHeader("WWW-Authenticate")
		chal, err := digest.ParseChallenge(wwwAuth.Value())
		if err != nil {
			panic(fmt.Errorf("Fail to parse challenge %s=%s: %w", "wwwauth", wwwAuth.Value(), err))
		}

		// Reply with digest
		cred, _ := digest.Digest(chal, digest.Options{
			Method:   req.Method.String(),
			URI:      recipient.Host,
			Username: *username,
			Password: *password,
		})

		newReq := req.Clone()
		newReq.AppendHeader(sip.NewHeader("Authorization", cred.String()))

		tx, err := client.TransactionRequest(newReq)
		if err != nil {
			panic(fmt.Errorf("Fail to create transaction: %w", err))
		}
		defer tx.Terminate()

		res, err = getResponse(tx)
		if err != nil {
			panic(fmt.Errorf("Fail to get response: %w", err))
		}
	}

	if res.StatusCode != 200 {
		panic("Fail to register")
	}

	slog.Info("Client registered")
}

func getResponse(tx sip.ClientTransaction) (*sip.Response, error) {
	select {
	case <-tx.Done():
		return nil, fmt.Errorf("transaction died")
	case res := <-tx.Responses():
		return res, nil
	}
}

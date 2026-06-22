package sip

import (
	"bytes"
	"errors"
	"net"
	"strings"

	sipgo "github.com/emiago/sipgo/sip"
)

// RandString returns a random alphanumeric string of length n. It builds on
// RandStringBytesMask because emiago/sipgo does not export an equivalent.
func RandString(n int) string {
	var sb strings.Builder
	return RandStringBytesMask(&sb, n)
}

// https://stackoverflow.com/questions/22892120/how-to-generate-a-random-string-of-a-fixed-length-in-go
func RandStringBytesMask(sb *strings.Builder, n int) string {
	return sipgo.RandStringBytesMask(sb, n)
}

// ASCIIToLower is faster than go version. It avoids one more loop
func ASCIIToLower(s string) string {
	return sipgo.ASCIIToLower(s)
}

func ASCIIToLowerInPlace(s []byte) {
	sipgo.ASCIIToLowerInPlace(s)
}

// HeaderToLower is fast ASCII lower string
func HeaderToLower(s string) string {
	return sipgo.HeaderToLower(s)
}

// Check uri is SIP fast
func UriIsSIP(s string) bool {
	switch s {
	case "sip", "SIP":
		return true
	}
	return false
}

func UriIsSIPS(s string) bool {
	switch s {
	case "sips", "SIPS":
		return true
	}
	return false
}

// SplitByWhitespace splits text into sections separated by one or more ABNF
// whitespace characters (space and tab).
func SplitByWhitespace(text string) []string {
	const abnf = " \t"
	var buffer bytes.Buffer
	var inString = true
	result := make([]string, 0)

	for _, char := range text {
		s := string(char)
		if strings.Contains(abnf, s) {
			if inString {
				// First whitespace char following text; flush buffer to the results array.
				result = append(result, buffer.String())
				buffer.Reset()
			}
			inString = false
		} else {
			buffer.WriteString(s)
			inString = true
		}
	}

	if buffer.Len() > 0 {
		result = append(result, buffer.String())
	}

	return result
}

// Forked from github.com/StefanKopieczek/gossip by @StefanKopieczek
func ResolveSelfIP() (net.IP, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 {
			continue // interface down
		}
		if iface.Flags&net.FlagLoopback != 0 {
			continue // loopback interface
		}
		addrs, err := iface.Addrs()
		if err != nil {
			return nil, err
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() {
				continue
			}
			ip = ip.To4()
			if ip == nil {
				continue // not an ipv4 address
			}
			return ip, nil
		}
	}
	return nil, errors.New("server not connected to any network")
}

func NonceWrite(buf []byte) {
	sipgo.NonceWrite(buf)
}

// MessageShortString dumps short version of msg. Used only for logging
func MessageShortString(msg Message) string {
	switch m := msg.(type) {
	case *Request:
		return m.Short()
	case *Response:
		return m.Short()
	}
	return "Unknown message type"
}

package transport

import (
	"context"
	"fmt"
	"net"

	"github.com/livekit/sipgo/sip"
)

// ResolveTargets resolves host into every address worth trying, in the order to
// try them. Each address carries the port that belongs to it.
//
// This has no counterpart on emiago/sipgo upstream. Upstream resolves to a single
// address, which is enough to place a request but leaves the caller no second
// address to try when a server fails.
//
// The SRV-versus-host decision matches resolveRequestAddr: an explicit port is
// honored as given, and only its absence lets SRV supply the port.
func (l *Layer) ResolveTargets(ctx context.Context, network, host string, port int, sipScheme string) ([]Addr, error) {
	if host == "" {
		return nil, fmt.Errorf("resolve targets: empty host")
	}
	if sipScheme == "" {
		sipScheme = "sip"
	}

	// An IP literal is already the answer.
	if ip := net.ParseIP(host); ip != nil {
		if port == 0 {
			port = sip.DefaultPort(network)
		}
		return []Addr{{IP: ip, Port: port}}, nil
	}

	if port != 0 {
		return l.resolveTargetsIP(ctx, host, port)
	}

	service, protocol := srvLabels(network, sipScheme)
	targets, err := l.resolveTargetsSRV(ctx, service, protocol, host)
	if err == nil && len(targets) > 0 {
		return targets, nil
	}
	if err != nil {
		l.log.Info("SRV lookup failed, falling back to host lookup", "host", host, "error", err)
	}
	return l.resolveTargetsIP(ctx, host, sip.DefaultPort(network))
}

// resolveTargetsIP pairs every address of host with the given port.
func (l *Layer) resolveTargetsIP(ctx context.Context, host string, port int) ([]Addr, error) {
	ips, err := l.dnsResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	targets := make([]Addr, 0, len(ips))
	// IPv4 first, then IPv6.
	for _, ip := range ips {
		if ip.IP.To4() != nil {
			targets = append(targets, Addr{IP: ip.IP, Port: port})
		}
	}
	for _, ip := range ips {
		if ip.IP.To4() == nil {
			targets = append(targets, Addr{IP: ip.IP, Port: port})
		}
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("resolve targets: no addresses for %q", host)
	}
	return targets, nil
}

// resolveTargetsSRV resolves every SRV record, keeping each record's port with
// the addresses of the target that named it. Records arrive sorted by priority
// and randomised by weight, so record order is the order to try. Labels come
// from srvLabels, the same mapping resolveAddrSRV uses.
func (l *Layer) resolveTargetsSRV(ctx context.Context, service, protocol, host string) ([]Addr, error) {
	_, records, err := l.dnsResolver.LookupSRV(ctx, service, protocol, host)
	if err != nil {
		return nil, fmt.Errorf("fail to lookup SRV for %q: %w", host, err)
	}

	var targets []Addr
	for _, rec := range records {
		if rec == nil || rec.Target == "" || rec.Target == "." {
			// A single "." target means the service is not offered here.
			continue
		}
		ips, err := l.dnsResolver.LookupIP(ctx, "ip", rec.Target)
		if err != nil {
			// One unresolvable target must not sink the others.
			l.log.Info("SRV target did not resolve", "target", rec.Target, "error", err)
			continue
		}
		for _, ip := range ips {
			targets = append(targets, Addr{IP: ip, Port: int(rec.Port)})
		}
	}
	return targets, nil
}

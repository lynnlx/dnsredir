/*
 * Created Feb 16, 2020
 */

package dnsredir

import (
	"context"
	"errors"
	"fmt"
	"github.com/coredns/coredns/plugin"
	"github.com/coredns/coredns/plugin/debug"
	"github.com/coredns/coredns/plugin/pkg/dnsutil"
	clog "github.com/coredns/coredns/plugin/pkg/log"
	"github.com/coredns/coredns/request"
	"github.com/miekg/dns"
	"sync/atomic"
	"time"
)

var log = clog.NewWithPlugin(pluginName)

type Dnsredir struct {
	Next plugin.Handler

	Upstreams *[]Upstream
}

// Upstream manages a pool of proxy upstream hosts
// see: github.com/coredns/proxy#proxy.go
type Upstream interface {
	// Check if given domain name should be routed to this upstream zone
	Match(name string) bool
	// Select an upstream host to be routed to, nil if no available host
	Select() *UpstreamHost

	// Exchanger returns the exchanger to be used for this upstream
	//Exchanger() interface{}
	// Send question to upstream host and await for response
	//Exchange(ctx context.Context, state request.Request) (*dns.Msg, error)

	Start() error
	Stop() error
}

func (r *Dnsredir) OnStartup() error {
	for _, up := range *r.Upstreams {
		if err := up.Start(); err != nil {
			return err
		}
	}
	return nil
}

func (r *Dnsredir) OnShutdown() error {
	for _, up := range *r.Upstreams {
		if err := up.Stop(); err != nil {
			return err
		}
	}
	return nil
}

func (r *Dnsredir) ServeDNS(ctx context.Context, w dns.ResponseWriter, req *dns.Msg) (int, error) {
	state := request.Request{W: w, Req: req}
	name := state.Name()

	upstream0, t := r.match(name)
	if upstream0 == nil {
		log.Debugf("%q not found in name list, t: %v", name, t)
		return plugin.NextOrFailure(r.Name(), r.Next, ctx, w, req)
	}
	upstream, ok := upstream0.(*reloadableUpstream)
	if !ok {
		panic(fmt.Sprintf("Why %T isn't a %T", upstream0, reloadableUpstream{}))
	}

	log.Debugf("%q in name list, t: %v", name, t)

	var reply *dns.Msg
	var upstreamErr error
	deadline := time.Now().Add(defaultTimeout)
	for time.Now().Before(deadline) {
		host := upstream.Select()
		if host == nil {
			log.Debug(errNoHealthy)
			return dns.RcodeServerFailure, errNoHealthy
		}
		log.Debugf("Upstream host %v is selected", host.addr)

		for {
			t := time.Now()
			reply, upstreamErr = host.Exchange(ctx, state)
			log.Debugf("rtt: %v", time.Since(t))
			if upstreamErr == errCachedConnClosed {
				// [sic] Remote side closed conn, can only happen with TCP.
				// Retry for another connection
				log.Debugf("%v: %v", upstreamErr, host.addr)
				continue
			}
			if reply != nil && reply.Truncated && !host.transport.forceTcp && host.transport.preferUdp {
				log.Warningf("TODO: Retry with TCP since response truncated and prefer_udp configured")
			}
			break
		}

		if upstreamErr != nil {
			if upstream.maxFails != 0 {
				log.Warningf("Exchange() failed  error: %v", upstreamErr)
				healthCheck(upstream, host)
			}
			continue
		}

		if !state.Match(reply) {
			debug.Hexdumpf(reply, "Wrong reply  id: %v, qname: %v qtype: %v", reply.Id, state.QName(), state.QType())

			formerr := new(dns.Msg)
			formerr.SetRcode(state.Req, dns.RcodeFormatError)
			_ = w.WriteMsg(formerr)
			return 0, nil
		}

		if r.urlInitialInProgress() {
			rewriteToMinimalTTLs(reply, uint32(dnsutil.MinimalDefaultTTL / time.Second))
		}
		_ = w.WriteMsg(reply)
		return 0, nil
	}

	if upstreamErr == nil {
		panic("Why upstreamErr is nil?! Are you in a debugger or your machine running slow?")
	}
	return dns.RcodeServerFailure, upstreamErr
}

// [optimization]
// Positive cache once all upstream hosts finished initial name list population from URL
//	thus we don't need to iterate over all upstream hosts
var initialFinished int32 = 0

func (r *Dnsredir)urlInitialInProgress() bool {
	if atomic.LoadInt32(&initialFinished) != 0 {
		return false
	}

	for _, u := range *r.Upstreams {
		up := u.(*reloadableUpstream)
		if atomic.LoadInt32(&up.initialCount) != 0 {
			return true
		}
	}

	atomic.StoreInt32(&initialFinished, 1)
	return false
}

// minimalTTL: TTL in seconds
// see: dnsutil.MinimalTTL()
func rewriteToMinimalTTLs(reply *dns.Msg, minimalTTL uint32) {
	for _, r := range reply.Answer {
		r.Header().Ttl = MinUint32(r.Header().Ttl, minimalTTL)
	}

	for _, r := range reply.Ns {
		r.Header().Ttl = MinUint32(r.Header().Ttl, minimalTTL)
	}

	for _, r := range reply.Extra {
		// [sic] OPT records use TTL field for extended rcode and flags
		if r.Header().Rrtype != dns.TypeOPT {
			r.Header().Ttl = MinUint32(r.Header().Ttl, minimalTTL)
		}
	}
}

func healthCheck(r *reloadableUpstream, uh *UpstreamHost) {
	// Skip unnecessary health checking
	if r.checkInterval == 0 || r.maxFails == 0 {
		return
	}

	failTimeout := defaultFailTimeout
	fails := atomic.AddInt32(&uh.fails, 1)
	go func(uh *UpstreamHost) {
		time.Sleep(failTimeout)
		// Failure count may go negative here, should be rectified by HC eventually
		atomic.AddInt32(&uh.fails, -1)
		// Kick off health check on every failureCheck failure
		if fails % failureCheck == 0 {
			_ = uh.Check()
		}
	}(uh)
}

func (r *Dnsredir) Name() string { return pluginName }

func (r *Dnsredir) match(name string) (Upstream, time.Duration) {
	if r.Upstreams == nil {
		panic("Why Dnsredir.Upstreams is nil?!")
	}

	// TODO: Add a metric value in Prometheus to determine average lookup time

	// Don't check validity of domain name, delegate to upstream host
	if len(name) > 1 {
		name = removeTrailingDot(name)
	}

	t := time.Now()
	for _, up := range *r.Upstreams {
		// For maximum performance, we search the first matched item and return directly
		// Unlike proxy plugin, which try to find longest match
		if up.Match(name) {
			return up, time.Since(t)
		}
	}

	return nil, time.Since(t)
}

var (
	errNoHealthy = errors.New("no healthy upstream host")
	errCachedConnClosed = errors.New("cached connection was closed by peer")
)

const (
	defaultTimeout = 15000 * time.Millisecond
	defaultFailTimeout = 2000 * time.Millisecond
	failureCheck = 3
)


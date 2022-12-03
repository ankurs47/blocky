package resolver

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/0xERR0R/blocky/config"
	"github.com/0xERR0R/blocky/log"
	"github.com/0xERR0R/blocky/model"
	"github.com/0xERR0R/blocky/util"
	"github.com/hashicorp/go-multierror"
	"github.com/miekg/dns"
	"github.com/sirupsen/logrus"
)

//nolint:gochecknoglobals
var (
	v4v6QTypes = []dns.Type{dns.Type(dns.TypeA), dns.Type(dns.TypeAAAA)}
)

// Bootstrap allows resolving hostnames using the configured bootstrap DNS.
type Bootstrap struct {
	log *logrus.Entry

	resolver    Resolver
	bootstraped bootstrapedResolvers

	systemResolver *net.Resolver
}

// NewBootstrap creates and returns a new Bootstrap.
// Internally, it uses a CachingResolver and an UpstreamResolver.
func NewBootstrap(cfg *config.Config) (b *Bootstrap, err error) {
	log := log.PrefixedLog("bootstrap")

	// Create b in multiple steps: Bootstrap and UpstreamResolver have a cyclic dependency
	// This also prevents the GC to clean up these two structs, but is not currently an
	// issue since they stay allocated until the process terminates
	b = &Bootstrap{
		log:            log,
		systemResolver: net.DefaultResolver, // allow replacing it during tests
	}

	bootstraped, err := newBootstrapedResolvers(b, cfg.BootstrapDNS)
	if err != nil {
		return nil, err
	}

	if len(bootstraped) == 0 {
		log.Infof("bootstrapDns is not configured, will use system resolver")

		return b, nil
	}

	parallelResolver, err := newParallelBestResolver(bootstraped.ResolverGroups())
	if err != nil {
		return nil, fmt.Errorf("could not create bootstrap ParallelBestResolver: %w", err)
	}

	b.bootstraped = bootstraped

	b.resolver = Chain(
		NewFilteringResolver(cfg.Filtering),
		NewCachingResolver(cfg.Caching, nil),
		parallelResolver,
	)

	return b, nil
}

func (b *Bootstrap) UpstreamIPs(r *UpstreamResolver) (*IPSet, error) {
	hostname := r.upstream.Host

	if ip := net.ParseIP(hostname); ip != nil { // nil-safe when hostname is an IP: makes writing test easier
		return newIPSet([]net.IP{ip}), nil
	}

	ips, err := b.resolveUpstream(r, hostname)
	if err != nil {
		return nil, err
	}

	return newIPSet(ips), nil
}

func (b *Bootstrap) resolveUpstream(r Resolver, host string) ([]net.IP, error) {
	// Use system resolver if no bootstrap is configured
	if b.resolver == nil {
		cfg := config.GetConfig()
		ctx := context.Background()

		timeout := cfg.UpstreamTimeout
		if timeout != 0 {
			var cancel context.CancelFunc

			ctx, cancel = context.WithTimeout(ctx, time.Duration(timeout))
			defer cancel()
		}

		return b.systemResolver.LookupIP(ctx, cfg.ConnectIPVersion.Net(), host)
	}

	if ips, ok := b.bootstraped[r]; ok {
		// Special path for bootstraped upstreams to avoid infinite recursion
		return ips, nil
	}

	return b.resolve(host, v4v6QTypes)
}

// NewHTTPTransport returns a new http.Transport that uses b to resolve hostnames
func (b *Bootstrap) NewHTTPTransport() *http.Transport {
	if b.resolver == nil {
		return &http.Transport{}
	}

	dialer := net.Dialer{}

	return &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			log := b.log.WithField("network", network).WithField("addr", addr)

			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				log.Errorf("dial error: %s", err)

				return nil, err
			}

			connectIPVersion := config.GetConfig().ConnectIPVersion

			var qTypes []dns.Type

			switch {
			case connectIPVersion != config.IPVersionDual: // ignore `network` if a specific version is configured
				qTypes = connectIPVersion.QTypes()
			case strings.HasSuffix(network, "4"):
				qTypes = []dns.Type{dns.Type(dns.TypeA)}
			case strings.HasSuffix(network, "6"):
				qTypes = []dns.Type{dns.Type(dns.TypeAAAA)}
			default:
				qTypes = v4v6QTypes
			}

			// Resolve the host with the bootstrap DNS
			ips, err := b.resolve(host, qTypes)
			if err != nil {
				log.Errorf("resolve error: %s", err)

				return nil, err
			}

			ip := ips[rand.Intn(len(ips))] //nolint:gosec

			log.WithField("ip", ip).Tracef("dialing %s", host)

			// Use the standard dialer to actually connect
			addrWithIP := net.JoinHostPort(ip.String(), port)

			return dialer.DialContext(ctx, network, addrWithIP)
		},
	}
}

func (b *Bootstrap) resolve(hostname string, qTypes []dns.Type) (ips []net.IP, err error) {
	ips = make([]net.IP, 0, len(qTypes))

	for _, qType := range qTypes {
		qIPs, qErr := b.resolveType(hostname, qType)
		if qErr != nil {
			err = multierror.Append(err, qErr)

			continue
		}

		ips = append(ips, qIPs...)
	}

	if err == nil && len(ips) == 0 {
		return nil, fmt.Errorf("no such host %s", hostname)
	}

	return
}

func (b *Bootstrap) resolveType(hostname string, qType dns.Type) (ips []net.IP, err error) {
	if ip := net.ParseIP(hostname); ip != nil {
		return []net.IP{ip}, nil
	}

	req := model.Request{
		Req: util.NewMsgWithQuestion(hostname, qType),
		Log: b.log,
	}

	rsp, err := b.resolver.Resolve(&req)
	if err != nil {
		return nil, err
	}

	if rsp.Res.Rcode != dns.RcodeSuccess {
		return nil, nil
	}

	ips = make([]net.IP, 0, len(rsp.Res.Answer))

	for _, a := range rsp.Res.Answer {
		switch rr := a.(type) {
		case *dns.A:
			ips = append(ips, rr.A)
		case *dns.AAAA:
			ips = append(ips, rr.AAAA)
		}
	}

	return ips, nil
}

// map of bootstraped resolvers their hardcoded IPs
type bootstrapedResolvers map[Resolver][]net.IP

func newBootstrapedResolvers(b *Bootstrap, cfg config.BootstrapDNSConfig) (bootstrapedResolvers, error) {
	upstreamIPs := make(bootstrapedResolvers, len(cfg))

	var multiErr *multierror.Error

	for i, upstreamCfg := range cfg {
		i := i + 1 // user visible index should start at 1

		upstream := upstreamCfg.Upstream

		var ips []net.IP

		switch {
		case upstream.IsDefault():
			multiErr = multierror.Append(
				multiErr,
				fmt.Errorf("item %d: upstream not configured (ips=%v)", i, upstreamCfg.IPs),
			)
			continue
		case upstream.Net == config.NetProtocolTcpUdp:
			ip := net.ParseIP(upstream.Host)
			if ip == nil {
				multiErr = multierror.Append(
					multiErr,
					fmt.Errorf("item %d: '%s': protocol %s must use IP instead of hostname", i, upstream, upstream.Net),
				)
				continue
			}

			ips = append(ips, ip)
		default:
			ips = upstreamCfg.IPs
			if len(ips) == 0 {
				multiErr = multierror.Append(
					multiErr,
					fmt.Errorf("item %d: '%s': protocol %s requires IPs to be set", i, upstream, upstream.Net),
				)
				continue
			}
		}

		resolver := newUpstreamResolverUnchecked(upstream, b)

		upstreamIPs[resolver] = ips
	}

	if multiErr != nil {
		return nil, fmt.Errorf("invalid bootstrapDns configuration: %w", multiErr)
	}

	return upstreamIPs, nil
}

func (br bootstrapedResolvers) ResolverGroups() map[string][]Resolver {
	resolvers := make([]Resolver, 0, len(br))

	for resolver := range br {
		resolvers = append(resolvers, resolver)
	}

	return map[string][]Resolver{
		upstreamDefaultCfgName: resolvers,
	}
}

type IPSet struct {
	values []net.IP
	index  uint32
}

func newIPSet(ips []net.IP) *IPSet {
	return &IPSet{values: ips}
}

func (ips *IPSet) Current() net.IP {
	idx := atomic.LoadUint32(&ips.index)

	return ips.values[idx]
}

func (ips *IPSet) Next() {
	oldIP := ips.index
	newIP := uint32(int(ips.index+1) % len(ips.values))

	// We don't care about the result: if the call fails,
	// it means the value was incremented by another goroutine
	_ = atomic.CompareAndSwapUint32(&ips.index, oldIP, newIP)
}

// Package tailnet serves a cc-interact daemon on its own tailnet addresses,
// trusting synckit mesh peers, and renders the URLs a tailnet browser opens.
package tailnet

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/yasyf/cc-interact/daemon"
	"github.com/yasyf/daemonkit/paths"
	"github.com/yasyf/synckit/meshtrust"
)

const (
	reconcileInterval = 30 * time.Second
	urlCertWait       = 2 * time.Second
)

// Tailnet is a daemon's tailnet serving state, returned by NewServer for
// rendering URLs. Without a Provider it serves nothing and renders no URLs.
type Tailnet struct {
	tp   *meshtrust.Provider
	p    paths.Paths
	bind string
	mgr  *certManager
	srv  *daemon.Server
}

// NewServer builds the daemon from cfg. A non-nil tp trusts mesh peers and the
// daemon's own tailnet origins, serves TLS and plaintext on every tailnet
// address, and binds addresses gained later from an OnHTTPStart hook that runs
// beside cfg.OnHTTPStart. A nil tp builds the daemon from cfg unchanged.
func NewServer(ctx context.Context, cfg daemon.Config, tp *meshtrust.Provider) (*daemon.Server, *Tailnet, error) {
	t := &Tailnet{tp: tp, p: cfg.Paths, bind: cfg.BindAddr}
	if tp != nil {
		t.mgr = newCertManager(filepath.Join(cfg.Paths.StateDir(), "tls"))
		armBeforeLegs(ctx, t.mgr, tp.SelfCertDomain(ctx))
		cfg.TrustedPeer = tp.TrustedPeer
		cfg.TrustedOrigin = tp.TrustedOrigin
		cfg.ExtraHTTPListeners = listeners(cfg.Paths, cfg.BindAddr, tp.SelfAddrs(ctx), t.mgr)
		cfg.OnHTTPStart = combineHooks(cfg.OnHTTPStart, func(ctx context.Context, _ int) { t.reconcile(ctx) })
	}
	srv, err := daemon.New(cfg)
	if err != nil {
		return nil, nil, err
	}
	t.srv = srv
	return srv, t, nil
}

// URLs returns the tailnet URLs reaching path, which starts with a slash, on a
// daemon whose primary HTTP plane listens on port: https on the cert domain
// once minted, else http on the machine label, else http on raw addresses.
// Nil without a Provider or when no tailnet leg serves.
func (t *Tailnet) URLs(ctx context.Context, port int, path string) []string {
	if t.tp == nil {
		return nil
	}
	certDomain := t.tp.SelfCertDomain(ctx)
	if certDomain != "" {
		t.mgr.awaitReady(ctx, urlCertWait)
	}
	domain := t.mgr.mintedDomain()
	minted := domain != "" && domain == certDomain
	return displayURLs(domain, minted, t.tp.SelfHostLabel(ctx), t.srv.HTTPExtraAddrs(), t.tp.SelfAddrs(ctx), t.bind, port, path)
}

func listeners(p paths.Paths, bind string, addrs []netip.Addr, mgr *certManager) []func(context.Context) (net.Listener, error) {
	return sniffFactories(meshtrust.Listeners(bind, addrs, lastHTTPPort(p)), mgr)
}

func lastHTTPPort(p paths.Paths) uint16 {
	b, err := os.ReadFile(p.HTTPInfoPath())
	if err != nil {
		return 0
	}
	var info daemon.HTTPInfo
	if err := json.Unmarshal(b, &info); err != nil {
		return 0
	}
	if info.Port < 1 || info.Port > math.MaxUint16 {
		return 0
	}
	return uint16(info.Port)
}

func (t *Tailnet) reconcile(ctx context.Context) {
	tick := time.NewTicker(reconcileInterval)
	defer tick.Stop()
	for {
		reconcilePass(ctx, t.srv, t.tp, t.p, t.bind, t.mgr)
		t.mgr.ensure(ctx, t.tp.SelfCertDomain(ctx))
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// reconcilePass filters served addresses by exact IP because meshtrust.Listeners
// binds eagerly: re-passing one would bind ip:0 on a fresh port.
func reconcilePass(ctx context.Context, srv *daemon.Server, tp *meshtrust.Provider, p paths.Paths, bind string, mgr *certManager) {
	extra := srv.HTTPExtraAddrs()
	bound := make(map[netip.Addr]bool, len(extra))
	hint := lastHTTPPort(p)
	haveHint := false
	for _, a := range extra {
		ap, err := netip.ParseAddrPort(a)
		if err != nil {
			continue
		}
		bound[ap.Addr().Unmap()] = true
		if !haveHint {
			hint = ap.Port()
			haveHint = true
		}
	}
	var missing []netip.Addr
	for _, a := range tp.SelfAddrs(ctx) {
		if !bound[a.Unmap()] {
			missing = append(missing, a)
			bound[a.Unmap()] = true
		}
	}
	if len(missing) == 0 {
		return
	}
	for _, factory := range sniffFactories(meshtrust.Listeners(bind, missing, hint), mgr) {
		ln, err := factory(ctx)
		if err != nil {
			slog.Warn("tailnet: bind listener", "err", err)
			continue
		}
		if err := srv.AddHTTPListener(ln); err != nil {
			slog.Warn("tailnet: add listener", "addr", ln.Addr().String(), "err", err)
		}
	}
}

// displayURLs never renders https for the primary-port fallback: the primary
// listener has no TLS sniffer.
func displayURLs(certDomain string, minted bool, hostLabel string, extra []string, selfAddrs []netip.Addr, bind string, port int, path string) []string {
	if len(extra) > 0 {
		return tailnetURLs(certDomain, minted, hostLabel, extra, selfAddrs, port, path)
	}
	if !isUnspecifiedBind(bind) || port < 1 || port > math.MaxUint16 {
		return nil
	}
	addrs := make([]string, 0, len(selfAddrs))
	for _, a := range selfAddrs {
		addrs = append(addrs, netip.AddrPortFrom(a.Unmap(), uint16(port)).String())
	}
	return tailnetURLs(certDomain, false, hostLabel, addrs, selfAddrs, port, path)
}

func isUnspecifiedBind(bind string) bool {
	ip, err := netip.ParseAddr(bind)
	return err == nil && ip.IsUnspecified()
}

// tailnetURLs prefers the bare machine label for http: the ts.net FQDN is
// HSTS-preloaded, so a browser would force it to https.
func tailnetURLs(certDomain string, minted bool, hostLabel string, extraAddrs []string, selfAddrs []netip.Addr, primaryPort int, path string) []string {
	var urls []string
	if minted && certDomain != "" {
		if port, ok := canonicalPort(extraAddrs, selfAddrs, primaryPort); ok {
			urls = append(urls, fmt.Sprintf("https://%s:%d%s", certDomain, port, path))
		}
		return urls
	}
	if hostLabel != "" {
		if port, ok := canonicalPort(extraAddrs, selfAddrs, primaryPort); ok {
			urls = append(urls, fmt.Sprintf("http://%s:%d%s", hostLabel, port, path))
		}
		return urls
	}
	for _, a := range extraAddrs {
		ap, err := netip.ParseAddrPort(a)
		if err != nil {
			continue
		}
		urls = append(urls, fmt.Sprintf("http://%s%s", ap.String(), path))
	}
	return urls
}

// canonicalPort advertises nothing when every leg is stale against a live
// self-address set; the next reconcile pass revives a live leg.
func canonicalPort(extraAddrs []string, selfAddrs []netip.Addr, primaryPort int) (uint16, bool) {
	self := make(map[netip.Addr]bool, len(selfAddrs))
	for _, addr := range selfAddrs {
		self[addr.Unmap()] = true
	}

	parsed := make([]netip.AddrPort, 0, len(extraAddrs))
	hasSelf := false
	for _, extra := range extraAddrs {
		ap, err := netip.ParseAddrPort(extra)
		if err != nil {
			continue
		}
		parsed = append(parsed, ap)
		if self[ap.Addr().Unmap()] {
			hasSelf = true
		}
	}

	if !hasSelf && len(selfAddrs) > 0 {
		return 0, false
	}
	var lowest uint16
	found := false
	for _, ap := range parsed {
		if hasSelf && !self[ap.Addr().Unmap()] {
			continue
		}
		if int(ap.Port()) == primaryPort {
			return ap.Port(), true
		}
		if !found || ap.Port() < lowest {
			lowest = ap.Port()
			found = true
		}
	}
	return lowest, found
}

// combineHooks returns once every hook has, so each keeps the substrate's
// wait-for-OnHTTPStart shutdown contract.
func combineHooks(hooks ...func(context.Context, int)) func(context.Context, int) {
	var live []func(context.Context, int)
	for _, h := range hooks {
		if h != nil {
			live = append(live, h)
		}
	}
	return func(ctx context.Context, port int) {
		var wg sync.WaitGroup
		for _, h := range live {
			wg.Go(func() { h(ctx, port) })
		}
		wg.Wait()
	}
}

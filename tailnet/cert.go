package tailnet

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yasyf/synckit/meshtrust"
)

const (
	certRefreshWindow = 30 * 24 * time.Hour
	mintTimeout       = 3 * time.Minute
	bootCertWait      = 5 * time.Second
)

var errNoCert = errors.New("tailnet: no valid TLS certificate")

type mintedCert struct {
	domain string
	cert   tls.Certificate
}

// certManager mints the cert off the serving path (a boot goroutine and the
// reconcile ticker), never inside a TLS handshake.
type certManager struct {
	dir  string
	mint func(ctx context.Context, dnsName, dir string) (string, string, error)
	now  func() time.Time

	mintMu    sync.Mutex
	cert      atomic.Pointer[mintedCert]
	readyOnce sync.Once
	ready     chan struct{}
}

func newCertManager(dir string) *certManager {
	return &certManager{dir: dir, mint: meshtrust.MintCert, now: time.Now, ready: make(chan struct{})}
}

// ensure skips rather than queues behind a mint already in flight; the next
// tick retries. Failures leave the previously served cert in place.
func (m *certManager) ensure(ctx context.Context, domain string) {
	if domain == "" {
		return
	}
	if !m.mintMu.TryLock() {
		return
	}
	defer m.mintMu.Unlock()
	if c := m.cert.Load(); c != nil && c.domain == domain && m.now().Add(certRefreshWindow).Before(c.cert.Leaf.NotAfter) {
		return
	}
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		slog.Warn("tailnet: create cert dir", "dir", m.dir, "err", err)
		return
	}
	mctx, cancel := context.WithTimeout(ctx, mintTimeout)
	defer cancel()
	certFile, keyFile, err := m.mint(mctx, domain, m.dir)
	if err != nil {
		slog.Warn("tailnet: mint cert", "domain", domain, "err", err)
		return
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		slog.Warn("tailnet: load cert", "domain", domain, "err", err)
		return
	}
	// LoadX509KeyPair fills Leaf only under the x509keypairleaf GODEBUG default.
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		slog.Warn("tailnet: parse cert leaf", "domain", domain, "err", err)
		return
	}
	cert.Leaf = leaf
	m.cert.Store(&mintedCert{domain: domain, cert: cert})
	m.readyOnce.Do(func() { close(m.ready) })
	slog.Info("tailnet: cert ready", "domain", domain, "notAfter", cert.Leaf.NotAfter)
}

// armBeforeLegs waits briefly for the boot mint because meshtrust.Listeners
// binds eagerly: a peer reconnecting the instant a leg appears would otherwise
// fail its handshake against a manager holding no cert.
func armBeforeLegs(ctx context.Context, mgr *certManager, domain string) {
	if domain == "" {
		return
	}
	go mgr.ensure(ctx, domain)
	mgr.awaitReady(ctx, bootCertWait)
}

func (m *certManager) awaitReady(ctx context.Context, wait time.Duration) {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-m.ready:
	case <-ctx.Done():
	case <-timer.C:
	}
}

func (m *certManager) valid() *mintedCert {
	c := m.cert.Load()
	if c == nil || !m.now().Before(c.cert.Leaf.NotAfter) {
		return nil
	}
	return c
}

func (m *certManager) get() *tls.Certificate {
	if c := m.valid(); c != nil {
		return &c.cert
	}
	return nil
}

func (m *certManager) mintedDomain() string {
	if c := m.valid(); c != nil {
		return c.domain
	}
	return ""
}

func (m *certManager) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	if c := m.get(); c != nil {
		return c, nil
	}
	return nil, errNoCert
}

// tlsConfig offers h2 because http.Server.Serve configures HTTP/2 for
// hand-wrapped *tls.Conn values when Server.TLSConfig is nil.
func (m *certManager) tlsConfig() *tls.Config {
	return &tls.Config{
		GetCertificate: m.getCertificate,
		MinVersion:     tls.VersionTLS12,
		NextProtos:     []string{"h2", "http/1.1"},
	}
}

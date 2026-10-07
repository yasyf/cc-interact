package tailnet

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

func makeTestCert(t *testing.T, domain string, notAfter time.Time) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		DNSNames:     []string{domain},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}

func writeTestCert(t *testing.T, dir, domain string, notAfter time.Time) (certFile, keyFile string) {
	t.Helper()
	certPEM, keyPEM := makeTestCert(t, domain, notAfter)
	certFile = filepath.Join(dir, domain+".crt")
	keyFile = filepath.Join(dir, domain+".key")
	if err := os.WriteFile(certFile, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

func TestCertManagerEnsure(t *testing.T) {
	const domain = "host.ts.net"
	ctx := context.Background()
	now := time.Now()
	notAfter := now.Add(90 * 24 * time.Hour)
	mints := 0
	m := newCertManager(filepath.Join(t.TempDir(), "tls"))
	m.now = func() time.Time { return now }
	m.mint = func(_ context.Context, dnsName, dir string) (string, string, error) {
		mints++
		certFile, keyFile := writeTestCert(t, dir, dnsName, notAfter)
		return certFile, keyFile, nil
	}

	m.ensure(ctx, "")
	if mints != 0 || m.get() != nil {
		t.Fatalf("empty domain: mints = %d, cert = %v, want no mint and nil", mints, m.get())
	}

	m.ensure(ctx, domain)
	if mints != 1 {
		t.Fatalf("first ensure: mints = %d, want 1", mints)
	}
	cert := m.get()
	if cert == nil {
		t.Fatal("first ensure left no cert")
	}
	if got := cert.Leaf.NotAfter.Unix(); got != notAfter.Unix() {
		t.Fatalf("leaf NotAfter = %d, want %d", got, notAfter.Unix())
	}

	m.ensure(ctx, domain)
	if mints != 1 {
		t.Fatalf("fresh cert re-minted: mints = %d, want 1", mints)
	}

	now = notAfter.Add(-15 * 24 * time.Hour)
	renewed := notAfter.Add(90 * 24 * time.Hour)
	notAfter = renewed
	m.ensure(ctx, domain)
	if mints != 2 {
		t.Fatalf("within refresh window: mints = %d, want 2", mints)
	}
	if got := m.get().Leaf.NotAfter.Unix(); got != renewed.Unix() {
		t.Fatalf("renewed leaf NotAfter = %d, want %d", got, renewed.Unix())
	}

	now = renewed.Add(-24 * time.Hour)
	m.mint = func(context.Context, string, string) (string, string, error) {
		mints++
		return "", "", errors.New("boom")
	}
	m.ensure(ctx, domain)
	if mints != 3 {
		t.Fatalf("failing mint not invoked: mints = %d, want 3", mints)
	}
	if got := m.get(); got == nil || got.Leaf.NotAfter.Unix() != renewed.Unix() {
		t.Fatalf("mint failure evicted the prior cert: %v", got)
	}
}

func TestCertManagergetCertificate(t *testing.T) {
	m := newCertManager(t.TempDir())
	if _, err := m.getCertificate(nil); !errors.Is(err, errNoCert) {
		t.Fatalf("empty manager GetCertificate err = %v, want errNoCert", err)
	}
}

func TestCertManagerDomainChange(t *testing.T) {
	ctx := context.Background()
	notAfter := time.Now().Add(90 * 24 * time.Hour)
	mints := 0
	m := newCertManager(filepath.Join(t.TempDir(), "tls"))
	m.mint = func(_ context.Context, dnsName, dir string) (string, string, error) {
		mints++
		certFile, keyFile := writeTestCert(t, dir, dnsName, notAfter)
		return certFile, keyFile, nil
	}

	m.ensure(ctx, "old.ts.net")
	if mints != 1 || m.get() == nil {
		t.Fatalf("first ensure: mints = %d, cert = %v, want 1 and non-nil", mints, m.get())
	}

	m.ensure(ctx, "new.ts.net")
	if mints != 2 {
		t.Fatalf("domain change did not re-mint: mints = %d, want 2", mints)
	}
	cert := m.get()
	if cert == nil || !slices.Contains(cert.Leaf.DNSNames, "new.ts.net") {
		t.Fatalf("served cert not re-minted for new domain: %v", cert)
	}
}

func TestCertManagerExpiredCert(t *testing.T) {
	const domain = "host.ts.net"
	ctx := context.Background()
	now := time.Now()
	notAfter := now.Add(90 * 24 * time.Hour)
	m := newCertManager(filepath.Join(t.TempDir(), "tls"))
	m.now = func() time.Time { return now }
	m.mint = func(_ context.Context, dnsName, dir string) (string, string, error) {
		certFile, keyFile := writeTestCert(t, dir, dnsName, notAfter)
		return certFile, keyFile, nil
	}

	m.ensure(ctx, domain)
	if m.get() == nil {
		t.Fatal("first ensure left no cert")
	}

	m.mint = func(context.Context, string, string) (string, string, error) {
		return "", "", errors.New("tailscale down")
	}
	now = notAfter.Add(time.Minute)
	m.ensure(ctx, domain)
	if got := m.get(); got != nil {
		t.Fatalf("expired cert still served: %v", got)
	}
	if _, err := m.getCertificate(nil); !errors.Is(err, errNoCert) {
		t.Fatalf("expired cert GetCertificate err = %v, want errNoCert", err)
	}
}

func TestCertManagerSingleFlight(t *testing.T) {
	const domain = "host.ts.net"
	certFile, keyFile := writeTestCert(t, t.TempDir(), domain, time.Now().Add(90*24*time.Hour))
	m := newCertManager(filepath.Join(t.TempDir(), "tls"))
	block := make(chan struct{})
	started := make(chan struct{})
	var mints atomic.Int32
	m.mint = func(context.Context, string, string) (string, string, error) {
		mints.Add(1)
		close(started)
		<-block
		return certFile, keyFile, nil
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		m.ensure(context.Background(), domain)
	}()
	<-started

	finished := make(chan struct{})
	go func() {
		defer close(finished)
		m.ensure(context.Background(), domain)
	}()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("concurrent ensure blocked behind the in-flight mint")
	}
	if got := mints.Load(); got != 1 {
		t.Fatalf("mints = %d, want 1", got)
	}

	close(block)
	<-done
	if m.get() == nil {
		t.Fatal("blocked mint never landed a cert")
	}
}

func TestArmBeforeLegs(t *testing.T) {
	const domain = "host.ts.net"
	certFile, keyFile := writeTestCert(t, t.TempDir(), domain, time.Now().Add(90*24*time.Hour))

	t.Run("returns with the cert armed", func(t *testing.T) {
		m := newCertManager(filepath.Join(t.TempDir(), "tls"))
		m.mint = func(context.Context, string, string) (string, string, error) {
			time.Sleep(10 * time.Millisecond)
			return certFile, keyFile, nil
		}
		armBeforeLegs(context.Background(), m, domain)
		if m.get() == nil {
			t.Fatal("legs would bind unarmed; want the boot mint awaited")
		}
	})

	t.Run("skips the wait when the tailnet publishes no cert domain", func(t *testing.T) {
		m := newCertManager(filepath.Join(t.TempDir(), "tls"))
		var mints atomic.Int32
		m.mint = func(context.Context, string, string) (string, string, error) {
			mints.Add(1)
			return certFile, keyFile, nil
		}
		start := time.Now()
		armBeforeLegs(context.Background(), m, "")
		if waited := time.Since(start); waited > time.Second {
			t.Fatalf("armBeforeLegs waited %v on an empty domain, want no wait", waited)
		}
		if got := mints.Load(); got != 0 {
			t.Fatalf("mints = %d, want 0 on an empty domain", got)
		}
	})

	t.Run("gives the legs up rather than holding the daemon on a stuck mint", func(t *testing.T) {
		m := newCertManager(filepath.Join(t.TempDir(), "tls"))
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		m.mint = func(context.Context, string, string) (string, string, error) {
			<-release
			return certFile, keyFile, nil
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			m.awaitReady(context.Background(), 20*time.Millisecond)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("a stuck mint held leg binding past the bound")
		}
		if m.get() != nil {
			t.Fatal("cert armed from a mint that never returned")
		}
	})
}

func TestCertManagerAwaitReady(t *testing.T) {
	const domain = "host.ts.net"
	certFile, keyFile := writeTestCert(t, t.TempDir(), domain, time.Now().Add(90*24*time.Hour))

	t.Run("returns once the first mint lands", func(t *testing.T) {
		m := newCertManager(filepath.Join(t.TempDir(), "tls"))
		release := make(chan struct{})
		m.mint = func(context.Context, string, string) (string, string, error) {
			<-release
			return certFile, keyFile, nil
		}
		go func() {
			time.Sleep(10 * time.Millisecond)
			close(release)
			m.ensure(context.Background(), domain)
		}()

		start := time.Now()
		m.awaitReady(context.Background(), 5*time.Second)
		if waited := time.Since(start); waited >= 5*time.Second {
			t.Fatalf("awaitReady waited %v, want a return on the mint", waited)
		}
		if m.mintedDomain() != domain {
			t.Fatalf("mintedDomain = %q, want %q", m.mintedDomain(), domain)
		}
	})

	t.Run("gives up after wait when no cert arrives", func(t *testing.T) {
		m := newCertManager(filepath.Join(t.TempDir(), "tls"))
		start := time.Now()
		m.awaitReady(context.Background(), 20*time.Millisecond)
		if waited := time.Since(start); waited < 20*time.Millisecond {
			t.Fatalf("awaitReady returned after %v, want the full wait", waited)
		}
		if m.mintedDomain() != "" {
			t.Fatalf("mintedDomain = %q, want empty", m.mintedDomain())
		}
	})

	t.Run("returns immediately on a cancelled context", func(t *testing.T) {
		m := newCertManager(filepath.Join(t.TempDir(), "tls"))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		start := time.Now()
		m.awaitReady(ctx, 5*time.Second)
		if waited := time.Since(start); waited >= 5*time.Second {
			t.Fatalf("awaitReady waited %v, want a return on the dead context", waited)
		}
	})

	t.Run("stays latched across a re-mint", func(t *testing.T) {
		m := newCertManager(filepath.Join(t.TempDir(), "tls"))
		m.mint = func(context.Context, string, string) (string, string, error) {
			return certFile, keyFile, nil
		}
		m.ensure(context.Background(), domain)
		m.ensure(context.Background(), "other.ts.net")

		start := time.Now()
		m.awaitReady(context.Background(), 5*time.Second)
		if waited := time.Since(start); waited >= 5*time.Second {
			t.Fatalf("awaitReady waited %v, want the latched ready", waited)
		}
	})
}

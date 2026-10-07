package tailnet

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"testing"
	"time"
)

func serveSniffed(t *testing.T, mgr *certManager) string {
	t.Helper()
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := newSniffListener(inner, mgr.tlsConfig())
	if got := ln.Addr().String(); got != inner.Addr().String() {
		t.Fatalf("Addr() = %q, want inner %q", got, inner.Addr().String())
	}
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, err := w.Write([]byte("ok")); err != nil {
			t.Errorf("write response: %v", err)
		}
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return inner.Addr().String()
}

func getBody(t *testing.T, client *http.Client, url string) string {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(body)
}

func TestSniffListenerDualProtocol(t *testing.T) {
	const domain = "host.ts.net"
	certPEM, keyPEM := makeTestCert(t, domain, time.Now().Add(24*time.Hour))
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("keypair: %v", err)
	}
	mgr := newCertManager(t.TempDir())
	mgr.cert.Store(&mintedCert{domain: domain, cert: pair})
	addr := serveSniffed(t, mgr)

	pool := x509.NewCertPool()
	pool.AddCert(pair.Leaf)
	clientTLS := &tls.Config{RootCAs: pool, ServerName: domain, MinVersion: tls.VersionTLS12}

	t.Run("plain http roundtrips", func(t *testing.T) {
		if got := getBody(t, http.DefaultClient, "http://"+addr+"/"); got != "ok" {
			t.Fatalf("body = %q, want ok", got)
		}
	})

	t.Run("tls roundtrips on the same listener", func(t *testing.T) {
		client := &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
			TLSClientConfig: clientTLS,
		}}
		if got := getBody(t, client, "https://"+domain+"/"); got != "ok" {
			t.Fatalf("body = %q, want ok", got)
		}
	})

	t.Run("empty manager fails tls but keeps plain", func(t *testing.T) {
		bare := serveSniffed(t, newCertManager(t.TempDir()))
		conn, err := tls.Dial("tcp", bare, clientTLS)
		if err == nil {
			_ = conn.Close()
			t.Fatal("tls dial succeeded with no cert minted")
		}
		if got := getBody(t, http.DefaultClient, "http://"+bare+"/"); got != "ok" {
			t.Fatalf("plain body = %q, want ok", got)
		}
	})
}

func TestSniffListenerSilentConn(t *testing.T) {
	addr := serveSniffed(t, newCertManager(t.TempDir()))

	silent, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = silent.Close() }()
	time.Sleep(50 * time.Millisecond)

	start := time.Now()
	if got := getBody(t, http.DefaultClient, "http://"+addr+"/"); got != "ok" {
		t.Fatalf("body = %q, want ok", got)
	}
	if elapsed := time.Since(start); elapsed >= sniffPeekTimeout {
		t.Fatalf("fast conn delayed %v behind the silent one", elapsed)
	}

	if err := silent.SetReadDeadline(time.Now().Add(sniffPeekTimeout + 2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := silent.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("silent conn not closed within the peek deadline: err = %v", err)
	}
}

func TestSniffListenerCloseUnblocksAccept(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := newSniffListener(inner, newCertManager(t.TempDir()).tlsConfig())
	accepted := make(chan error, 1)
	go func() {
		_, err := ln.Accept()
		accepted <- err
	}()
	if err := ln.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-accepted:
		if err == nil {
			t.Fatal("Accept returned a conn after Close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Accept still blocked after Close")
	}
}

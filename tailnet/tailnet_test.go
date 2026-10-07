package tailnet

import (
	"context"
	"encoding/json"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yasyf/cc-interact/daemon"
	"github.com/yasyf/daemonkit/paths"
)

func testPaths(t *testing.T) paths.Paths {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DAEMONKIT_HOME", home)
	p := paths.Paths{App: ".cc-interact-tailnet-test"}
	if err := p.EnsureStateDir(); err != nil {
		t.Fatalf("EnsureStateDir: %v", err)
	}
	return p
}

func writeHandshake(t *testing.T, p paths.Paths, port int) {
	t.Helper()
	b, err := json.Marshal(daemon.HTTPInfo{Port: port})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.HTTPInfoPath(), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestTailnetListenersUsesHandshakeHint(t *testing.T) {
	p := testPaths(t)

	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hint := probe.Addr().(*net.TCPAddr).Port
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}
	writeHandshake(t, p, hint)

	factories := listeners(p, "", []netip.Addr{netip.MustParseAddr("127.0.0.1")}, newCertManager(t.TempDir()))
	if len(factories) != 1 {
		t.Fatalf("len(factories) = %d, want 1", len(factories))
	}
	ln, err := factories[0](context.Background())
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	defer func() { _ = ln.Close() }()
	if got := ln.Addr().(*net.TCPAddr).Port; got != hint {
		t.Errorf("bound port = %d, want handshake hint %d", got, hint)
	}
}

func TestTailnetURLs(t *testing.T) {
	self := []netip.Addr{netip.MustParseAddr("100.1.2.3")}
	tests := []struct {
		name        string
		certDomain  string
		minted      bool
		hostLabel   string
		addrs       []string
		selfAddrs   []netip.Addr
		primaryPort int
		want        []string
	}{
		{
			"minted dedupes v4 and v6 legs sharing a port",
			"host.ts.net", true, "host",
			[]string{"100.1.2.3:8080", "[fd7a:115c:a1e0::1]:8080"},
			self, 8080,
			[]string{"https://host.ts.net:8080/p/board"},
		},
		{
			"minted collapses distinct ports to the primary port",
			"host.ts.net", true, "host",
			[]string{"100.1.2.3:8080", "100.1.2.3:9090"},
			self, 8080,
			[]string{"https://host.ts.net:8080/p/board"},
		},
		{
			"all legs stale against live self addresses advertises nothing",
			"host.ts.net", true, "host",
			[]string{"100.9.9.9:8080", "100.9.9.8:9090"},
			self, 8080,
			nil,
		},
		{
			"self address beats a stale address on the primary port",
			"host.ts.net", true, "host",
			[]string{"100.9.9.9:8080", "100.1.2.3:9090"},
			self, 8080,
			[]string{"https://host.ts.net:9090/p/board"},
		},
		{
			"no self-address info prefers the primary port",
			"host.ts.net", true, "host",
			[]string{"100.9.9.9:9090", "100.8.8.8:8080"},
			nil, 8080,
			[]string{"https://host.ts.net:8080/p/board"},
		},
		{
			"no self-address info or primary-port match uses the lowest port",
			"host.ts.net", true, "host",
			[]string{"100.9.9.9:9090", "100.8.8.8:8080"},
			nil, 7070,
			[]string{"https://host.ts.net:8080/p/board"},
		},
		{
			"host label also prefers a self address over a stale primary-port leg",
			"host.ts.net", false, "host",
			[]string{"100.9.9.9:8080", "100.1.2.3:9090"},
			self, 8080,
			[]string{"http://host:9090/p/board"},
		},
		{
			"unminted dedupes v4 and v6 legs sharing a port",
			"host.ts.net", false, "host",
			[]string{"100.1.2.3:8080", "[fd7a:115c:a1e0::1]:8080"},
			self, 8080,
			[]string{"http://host:8080/p/board"},
		},
		{
			"unminted without a host label stays on raw ips",
			"host.ts.net", false, "",
			[]string{"100.1.2.3:8080", "[fd7a:115c:a1e0::1]:9090"},
			self, 8080,
			[]string{"http://100.1.2.3:8080/p/board", "http://[fd7a:115c:a1e0::1]:9090/p/board"},
		},
		{
			"minted without a domain stays on raw ips",
			"", true, "",
			[]string{"100.1.2.3:8080"},
			self, 8080,
			[]string{"http://100.1.2.3:8080/p/board"},
		},
		{
			"no domain no cert falls back to raw ips, bracketing v6",
			"", false, "",
			[]string{"100.1.2.3:8080", "[fd7a:115c:a1e0::1]:9090"},
			self, 8080,
			[]string{"http://100.1.2.3:8080/p/board", "http://[fd7a:115c:a1e0::1]:9090/p/board"},
		},
		{"empty addrs minted yields nil", "host.ts.net", true, "host", nil, self, 8080, nil},
		{"empty addrs unminted yields nil", "", false, "host", nil, self, 8080, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tailnetURLs(tt.certDomain, tt.minted, tt.hostLabel, tt.addrs, tt.selfAddrs, tt.primaryPort, "/p/board")
			if !slices.Equal(got, tt.want) {
				t.Fatalf("tailnetURLs(%q, %v, %q, %v, %v, %d) = %v, want %v", tt.certDomain, tt.minted, tt.hostLabel, tt.addrs, tt.selfAddrs, tt.primaryPort, got, tt.want)
			}
		})
	}
}

func TestDisplayURLs(t *testing.T) {
	self := []netip.Addr{netip.MustParseAddr("100.64.0.7"), netip.MustParseAddr("fd7a::7")}
	tests := []struct {
		name   string
		minted bool
		label  string
		extra  []string
		bind   string
		want   []string
	}{
		{"extra legs minted", true, "", []string{"100.64.0.7:62520"}, "", []string{"https://ts.example.ts.net:62520/p/s"}},
		{"extra legs minted collapse to the primary port", true, "", []string{"100.64.0.7:8080", "100.64.0.7:62520"}, "", []string{"https://ts.example.ts.net:8080/p/s"}},
		{"extra legs unminted", false, "", []string{"100.64.0.7:62520"}, "", []string{"http://100.64.0.7:62520/p/s"}},
		{"extra legs unminted with label", false, "pi", []string{"100.64.0.7:62520"}, "", []string{"http://pi:62520/p/s"}},
		{"default loopback bind without legs", true, "", nil, "", nil},
		{"explicit loopback bind without legs", true, "", nil, "127.0.0.1", nil},
		{"specific non-loopback bind without legs", true, "", nil, "192.168.1.5", nil},
		{
			"unspecified bind unminted serves ip urls on the primary port",
			false, "", nil, "0.0.0.0",
			[]string{"http://100.64.0.7:8080/p/s", "http://[fd7a::7]:8080/p/s"},
		},
		{
			"unspecified bind with label collapses to one url on the primary port",
			true, "pi", nil, "0.0.0.0",
			[]string{"http://pi:8080/p/s"},
		},
		{
			"unspecified bind minted stays http: the primary leg has no sniffer",
			true, "", nil, "0.0.0.0",
			[]string{"http://100.64.0.7:8080/p/s", "http://[fd7a::7]:8080/p/s"},
		},
		{
			"unspecified v6 bind minted stays http on raw ips",
			true, "", nil, "::",
			[]string{"http://100.64.0.7:8080/p/s", "http://[fd7a::7]:8080/p/s"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := displayURLs("ts.example.ts.net", tt.minted, tt.label, tt.extra, self, tt.bind, 8080, "/p/s")
			if !slices.Equal(got, tt.want) {
				t.Errorf("displayURLs() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLastHTTPPort(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, p paths.Paths)
		want  uint16
	}{
		{"absent handshake", func(*testing.T, paths.Paths) {}, 0},
		{"corrupt handshake", func(t *testing.T, p paths.Paths) {
			if err := os.WriteFile(filepath.Join(p.StateDir(), "http.json"), []byte("{"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, 0},
		{"out-of-range port", func(t *testing.T, p paths.Paths) { writeHandshake(t, p, 99999) }, 0},
		{"negative port", func(t *testing.T, p paths.Paths) { writeHandshake(t, p, -1) }, 0},
		{"valid port", func(t *testing.T, p paths.Paths) { writeHandshake(t, p, 4321) }, 4321},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := testPaths(t)
			tt.setup(t, p)
			if got := lastHTTPPort(p); got != tt.want {
				t.Errorf("lastHTTPPort() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestCombineHooksRunsEveryHookAndWaits(t *testing.T) {
	var a, b atomic.Int32
	release := make(chan struct{})
	hook := combineHooks(
		func(_ context.Context, port int) { a.Store(int32(port)) },
		nil,
		func(_ context.Context, port int) {
			<-release
			b.Store(int32(port))
		},
	)
	done := make(chan struct{})
	go func() {
		hook(context.Background(), 7)
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("combined hook returned before every hook finished")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	<-done
	if a.Load() != 7 || b.Load() != 7 {
		t.Fatalf("hooks saw ports %d and %d, want 7 and 7", a.Load(), b.Load())
	}
}

func TestURLsWithoutProvider(t *testing.T) {
	if got := (&Tailnet{}).URLs(context.Background(), 8080, "/"); got != nil {
		t.Fatalf("URLs() = %v, want nil", got)
	}
}

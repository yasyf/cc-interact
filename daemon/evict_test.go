package daemon

import (
	"encoding/json"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// shortSockPath returns a socket path short enough for the unix sun_path limit
// (t.TempDir() under macOS test runners can exceed it).
func shortSockPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "cci")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "d.sock")
}

// fakeHolder is an in-process stand-in for an old daemon holding the socket: it
// answers health with a fixed version and runs onShutdown when asked to step down.
type fakeHolder struct {
	ln         net.Listener
	version    string
	shutdowns  atomic.Int32
	onShutdown func()
}

func startFakeHolder(t *testing.T, socket, ver string) *fakeHolder {
	t.Helper()
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("fake holder listen: %v", err)
	}
	h := &fakeHolder{ln: ln, version: ver}
	h.onShutdown = func() { h.Close() }
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				var env Envelope
				if err := json.NewDecoder(conn).Decode(&env); err != nil {
					return
				}
				_ = json.NewEncoder(conn).Encode(Reply{Proto: ProtocolVersion, OK: true, DaemonVersion: h.version})
				if env.Op == OpShutdown {
					h.shutdowns.Add(1)
					h.onShutdown()
				}
			}(conn)
		}
	}()
	t.Cleanup(h.Close)
	return h
}

func (h *fakeHolder) Close() { _ = h.ln.Close() }

func evictServer(socket, ver string) *Server {
	return &Server{
		appName:      "cc-test",
		version:      ver,
		socket:       socket,
		log:          log.New(io.Discard, "", 0),
		evictTimeout: 500 * time.Millisecond,
	}
}

func TestListenNoHolderBinds(t *testing.T) {
	sock := shortSockPath(t)
	// A stale socket file with no listener behind it must not block binding.
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ln, err := evictServer(sock, "dev").listen()
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ln.Close()
}

func TestEvictRefusesSameVersionHolder(t *testing.T) {
	sock := shortSockPath(t)
	startFakeHolder(t, sock, "v1.0.0")

	_, err := evictServer(sock, "v1.0.0").listen()
	if err == nil || !strings.Contains(err.Error(), "already holds the socket") {
		t.Fatalf("err = %v, want same-version refusal", err)
	}
}

func TestEvictRefusesNewerHolder(t *testing.T) {
	sock := shortSockPath(t)
	h := startFakeHolder(t, sock, "v0.6.0")

	_, err := evictServer(sock, "v0.5.0").listen()
	if err == nil || !strings.Contains(err.Error(), "already holds the socket") {
		t.Fatalf("err = %v, want newer-holder refusal", err)
	}
	if n := h.shutdowns.Load(); n != 0 {
		t.Fatalf("newer holder received %d shutdowns, want 0", n)
	}
}

func TestEvictShutsDownOlderHolder(t *testing.T) {
	sock := shortSockPath(t)
	h := startFakeHolder(t, sock, "v0.0.1-old")

	ln, err := evictServer(sock, "v1.0.0").listen()
	if err != nil {
		t.Fatalf("listen after eviction: %v", err)
	}
	ln.Close()
	if n := h.shutdowns.Load(); n != 1 {
		t.Fatalf("holder received %d shutdowns, want 1", n)
	}
}

func TestEvictKillsWedgedHolder(t *testing.T) {
	sock := shortSockPath(t)
	h := startFakeHolder(t, sock, "v0.0.1-old")
	h.onShutdown = func() {} // ack the shutdown but keep holding the socket

	const wedgedPID = 424242
	origHolderPID, origKillProc := holderPID, killProc
	t.Cleanup(func() { holderPID, killProc = origHolderPID, origKillProc })
	var killed atomic.Int32
	holderPID = func(*Client) (int, error) { return wedgedPID, nil }
	killProc = func(pid int, sig syscall.Signal) error {
		if sig == syscall.SIGKILL {
			if pid != wedgedPID {
				t.Errorf("SIGKILL sent to pid %d, want %d", pid, wedgedPID)
			}
			killed.Add(1)
			h.Close() // the "kill" releases the socket
			return nil
		}
		return syscall.ESRCH // the exit-wait probe: process already gone
	}

	ln, err := evictServer(sock, "v1.0.0").listen()
	if err != nil {
		t.Fatalf("listen after kill: %v", err)
	}
	ln.Close()
	if killed.Load() != 1 {
		t.Fatal("wedged holder was not SIGKILLed")
	}
	if h.shutdowns.Load() != 1 {
		t.Fatal("graceful shutdown was not attempted before the kill")
	}
}

func TestWaitGone(t *testing.T) {
	sock := shortSockPath(t)
	h := startFakeHolder(t, sock, "v")
	c := NewClient(sock)

	if c.WaitGone(300 * time.Millisecond) {
		t.Fatal("WaitGone reported a live socket as gone")
	}
	h.Close()
	if !c.WaitGone(2 * time.Second) {
		t.Fatal("WaitGone never saw the socket release")
	}
}

func TestPeerPIDReturnsRealPeer(t *testing.T) {
	sock := shortSockPath(t)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	pid, err := NewClient(sock).peerPID()
	if err != nil {
		t.Fatalf("peerPID: %v", err)
	}
	if pid != os.Getpid() {
		t.Fatalf("peer pid = %d, want self %d", pid, os.Getpid())
	}
}

func TestCurrentVersionPolicy(t *testing.T) {
	cases := []struct {
		name          string
		myVersion     string
		holderVersion string
		wantCurrent   bool
	}{
		{"newer daemon is current", "v0.5.0", "v0.6.0", true},
		{"older daemon is not current", "v0.6.0", "v0.5.0", false},
		{"same version is current", "v0.5.0", "v0.5.0", true},
		{"dev daemon is current for releases", "v0.5.0", "dev", true},
		{"dev binary outranks every release daemon", "dev", "v9.9.9", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sock := shortSockPath(t)
			startFakeHolder(t, sock, tc.holderVersion)

			l := Launcher{Version: tc.myVersion}
			if got := l.currentVersion(NewClient(sock)); got != tc.wantCurrent {
				t.Fatalf("currentVersion(me=%s, daemon=%s) = %v, want %v",
					tc.myVersion, tc.holderVersion, got, tc.wantCurrent)
			}
		})
	}
}

func TestKillHolderSparesSelfAndInit(t *testing.T) {
	origHolderPID, origKillProc := holderPID, killProc
	t.Cleanup(func() { holderPID, killProc = origHolderPID, origKillProc })
	killProc = func(pid int, sig syscall.Signal) error {
		t.Errorf("killProc called for pid %d", pid)
		return nil
	}

	for _, pid := range []int{0, 1, os.Getpid()} {
		holderPID = func(*Client) (int, error) { return pid, nil }
		got, err := NewClient("unused").KillHolder()
		if err != nil || got != 0 {
			t.Fatalf("KillHolder(pid=%d) = (%d, %v), want (0, nil)", pid, got, err)
		}
	}
}

func TestKillHolderToleratesESRCH(t *testing.T) {
	origHolderPID, origKillProc := holderPID, killProc
	t.Cleanup(func() { holderPID, killProc = origHolderPID, origKillProc })
	holderPID = func(*Client) (int, error) { return 99999999, nil }
	killProc = func(int, syscall.Signal) error { return syscall.ESRCH }

	pid, err := NewClient("unused").KillHolder()
	if err != nil {
		t.Fatalf("ESRCH must read as already-dead, got %v", err)
	}
	if pid != 99999999 {
		t.Fatalf("pid = %d", pid)
	}
}

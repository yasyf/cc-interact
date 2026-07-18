package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	dkdaemon "github.com/yasyf/daemonkit/daemon"
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
	ln, lock, err := evictServer(sock, "dev").listen(context.Background())
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ln.Close()
	_ = lock.Close()
}

func TestEvictRefusesSameVersionHolder(t *testing.T) {
	sock := shortSockPath(t)
	startFakeHolder(t, sock, "v1.0.0")

	_, _, err := evictServer(sock, "v1.0.0").listen(context.Background())
	if err == nil || !strings.Contains(err.Error(), "already holds the socket") {
		t.Fatalf("err = %v, want same-version refusal", err)
	}
}

func TestEvictRefusesNewerHolder(t *testing.T) {
	sock := shortSockPath(t)
	h := startFakeHolder(t, sock, "v0.6.0")

	_, _, err := evictServer(sock, "v0.5.0").listen(context.Background())
	if err == nil || !strings.Contains(err.Error(), "already holds the socket") {
		t.Fatalf("err = %v, want newer-holder refusal", err)
	}
	if n := h.shutdowns.Load(); n != 0 {
		t.Fatalf("newer holder received %d shutdowns, want 0", n)
	}
}

// TestEvictOlderHolderDelegatesToTakeover proves cc-interact classifies a
// strictly-older holder for eviction and hands it to daemonkit's takeover ladder.
// The in-process holder shares the test's pid, so daemonkit's self-victim guard
// fires; the distinct child below pins the full shutdown->grace->SIGKILL ladder.
func TestEvictOlderHolderDelegatesToTakeover(t *testing.T) {
	sock := shortSockPath(t)
	startFakeHolder(t, sock, "v0.0.1-old")

	_, _, err := evictServer(sock, "v1.0.0").listen(context.Background())
	if !errors.Is(err, dkdaemon.ErrRefuseVictim) {
		t.Fatalf("err = %v, want ErrRefuseVictim (older holder entered the takeover ladder)", err)
	}
}

// TestEvictKillsAcceptingUnresponsiveHolder proves the post-shutdown gone probe
// does not mistake a framed-RPC timeout for a released socket.
func TestEvictKillsAcceptingUnresponsiveHolder(t *testing.T) {
	if socket := os.Getenv("CC_INTERACT_WEDGED_HOLDER_SOCKET"); socket != "" {
		if err := serveAcceptingUnresponsiveHolder(socket); err != nil {
			t.Fatal(err)
		}
		return
	}

	sock := shortSockPath(t)
	child := exec.Command(os.Args[0], "-test.run=^TestEvictKillsAcceptingUnresponsiveHolder$")
	child.Env = append(os.Environ(), "CC_INTERACT_WEDGED_HOLDER_SOCKET="+sock)
	var stderr strings.Builder
	child.Stderr = &stderr
	if err := child.Start(); err != nil {
		t.Fatalf("start fake holder: %v", err)
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- child.Wait() }()
	waited := false
	t.Cleanup(func() {
		if waited {
			return
		}
		_ = child.Process.Kill()
		<-waitDone
	})

	client := NewClient(sock)
	deadline := time.Now().Add(2 * time.Second)
	for {
		select {
		case err := <-waitDone:
			waited = true
			t.Fatalf("fake holder exited before ready: %v: %s", err, stderr.String())
		default:
		}
		reply, err := client.Health()
		if err == nil && reply.OK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fake holder did not become ready")
		}
		time.Sleep(20 * time.Millisecond)
	}

	ln, lock, err := evictServer(sock, "v1.0.0").listen(context.Background())
	if err != nil {
		t.Fatalf("listen after eviction: %v", err)
	}
	defer func() { _ = ln.Close() }()
	defer func() { _ = lock.Close() }()

	select {
	case err := <-waitDone:
		waited = true
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("fake holder exit = %v, want SIGKILL", err)
		}
		status, ok := exit.Sys().(syscall.WaitStatus)
		if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
			t.Fatalf("fake holder status = %v, want SIGKILL", status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("accepting unresponsive holder was not SIGKILLed")
	}
}

func serveAcceptingUnresponsiveHolder(socket string) error {
	ln, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	var wedged atomic.Bool
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go func() {
			defer func() { _ = conn.Close() }()
			if wedged.Load() {
				_, _ = io.Copy(io.Discard, conn)
				return
			}
			var env Envelope
			if err := json.NewDecoder(conn).Decode(&env); err != nil {
				return
			}
			if err := json.NewEncoder(conn).Encode(Reply{Proto: ProtocolVersion, OK: true, DaemonVersion: "v0.0.1-old"}); err != nil {
				return
			}
			if env.Op == OpShutdown {
				wedged.Store(true)
			}
		}()
	}
}

// TestWaitHolderExit covers the post-eviction soft wait: it returns as soon as the
// holder's pid is gone (ESRCH), and on a lingering pid it waits out the timeout and
// proceeds rather than blocking the successor's bind.
func TestWaitHolderExit(t *testing.T) {
	orig := killProc
	t.Cleanup(func() { killProc = orig })

	t.Run("returns on holder exit", func(t *testing.T) {
		var probes atomic.Int32
		killProc = func(int, syscall.Signal) error {
			if probes.Add(1) >= 3 {
				return syscall.ESRCH
			}
			return nil
		}
		done := make(chan struct{})
		go func() {
			evictServer("unused", "v1.0.0").waitHolderExit(context.Background(), 424242)
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("waitHolderExit did not return after the holder exited")
		}
		if probes.Load() < 3 {
			t.Fatalf("probed %d times, want at least 3 before ESRCH", probes.Load())
		}
	})

	t.Run("proceeds when the pid lingers", func(t *testing.T) {
		killProc = func(int, syscall.Signal) error { return nil } // never exits
		s := evictServer("unused", "v1.0.0")
		s.evictTimeout = 200 * time.Millisecond
		start := time.Now()
		s.waitHolderExit(context.Background(), 424242)
		if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
			t.Fatalf("waitHolderExit returned after %s, want it to wait out the timeout", elapsed)
		}
	})
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

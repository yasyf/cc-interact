package daemon

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"testing"
	"time"

	"github.com/yasyf/cc-interact/paths"
	"github.com/yasyf/cc-interact/sse"
)

func testPaths() paths.Paths { return paths.Paths{App: ".cc-interact-test"} }

// isolateStateDir points the test state dir at a fresh temp HOME so each case
// starts without an http.json handshake.
func isolateStateDir(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	if err := testPaths().EnsureStateDir(); err != nil {
		t.Fatal(err)
	}
}

func boundPort(t *testing.T, ln net.Listener) int {
	t.Helper()
	return ln.Addr().(*net.TCPAddr).Port
}

func TestListenHTTPPortReuse(t *testing.T) {
	t.Run("no handshake binds ephemeral", func(t *testing.T) {
		isolateStateDir(t)

		ln, err := (&Server{paths: testPaths()}).listenHTTP()
		if err != nil {
			t.Fatalf("listenHTTP: %v", err)
		}
		defer ln.Close()
		if boundPort(t, ln) == 0 {
			t.Fatal("ephemeral bind returned port 0")
		}
	})

	t.Run("free published port is reused", func(t *testing.T) {
		isolateStateDir(t)
		prev, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := boundPort(t, prev)
		prev.Close()
		s := &Server{paths: testPaths()}
		if err := s.writeHTTPInfo(HTTPInfo{Port: port}); err != nil {
			t.Fatal(err)
		}

		ln, err := s.listenHTTP()
		if err != nil {
			t.Fatalf("listenHTTP: %v", err)
		}
		defer ln.Close()
		if got := boundPort(t, ln); got != port {
			t.Fatalf("bound port %d, want published port %d", got, port)
		}
	})

	t.Run("held published port falls back to ephemeral", func(t *testing.T) {
		isolateStateDir(t)
		holder, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer holder.Close()
		port := boundPort(t, holder)
		s := &Server{paths: testPaths()}
		if err := s.writeHTTPInfo(HTTPInfo{Port: port}); err != nil {
			t.Fatal(err)
		}

		ln, err := s.listenHTTP()
		if err != nil {
			t.Fatalf("listenHTTP: %v", err)
		}
		defer ln.Close()
		if got := boundPort(t, ln); got == port {
			t.Fatalf("bound the held port %d, want a different one", port)
		}
	})

	t.Run("occupied fixed port fails", func(t *testing.T) {
		isolateStateDir(t)
		holder, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer holder.Close()

		if _, err := (&Server{paths: testPaths(), fixedPort: boundPort(t, holder)}).listenHTTP(); err == nil {
			t.Fatal("listenHTTP on an occupied fixed port must fail")
		}
	})

	t.Run("configured bind addr is honored", func(t *testing.T) {
		isolateStateDir(t)

		ln, err := (&Server{paths: testPaths(), bindAddr: "127.0.0.1"}).listenHTTP()
		if err != nil {
			t.Fatalf("listenHTTP: %v", err)
		}
		defer ln.Close()
		if got := ln.Addr().(*net.TCPAddr).IP.String(); got != "127.0.0.1" {
			t.Fatalf("bound IP %s, want 127.0.0.1", got)
		}
	})

	t.Run("corrupt handshake binds ephemeral", func(t *testing.T) {
		isolateStateDir(t)
		if err := os.WriteFile(testPaths().HTTPInfoPath(), []byte("not json"), 0o600); err != nil {
			t.Fatal(err)
		}

		ln, err := (&Server{paths: testPaths()}).listenHTTP()
		if err != nil {
			t.Fatalf("listenHTTP: %v", err)
		}
		defer ln.Close()
		if boundPort(t, ln) == 0 {
			t.Fatal("ephemeral bind returned port 0")
		}
	})
}

func TestNewRefusesUnauthenticatedBind(t *testing.T) {
	if _, err := New(Config{BindAddr: "0.0.0.0"}); !errors.Is(err, ErrUnauthenticatedBind) {
		t.Fatalf("New err = %v, want ErrUnauthenticatedBind", err)
	}
}

func TestStartHTTPFiresOnHTTPStart(t *testing.T) {
	isolateStateDir(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	got := make(chan int, 1)
	s := &Server{
		paths:       testPaths(),
		log:         log.New(io.Discard, "", 0),
		onHTTPStart: func(_ context.Context, port int) { got <- port },
	}
	s.sse = sse.NewServer(s, sse.Config{})

	if err := s.startHTTP(ctx); err != nil {
		t.Fatalf("startHTTP: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		s.wg.Wait()
	})

	if got := s.readHTTPInfo().Bind; got != "127.0.0.1" {
		t.Fatalf("published bind %q, want the 127.0.0.1 default", got)
	}

	select {
	case port := <-got:
		if port == 0 {
			t.Fatal("OnHTTPStart received port 0")
		}
		if port != s.httpPort {
			t.Fatalf("OnHTTPStart port = %d, want bound port %d", port, s.httpPort)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnHTTPStart did not fire")
	}
}

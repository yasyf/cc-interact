package daemon

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
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

func TestNewRefusesUnauthenticatedExtraListeners(t *testing.T) {
	cfg := Config{ExtraHTTPListeners: []func(context.Context) (net.Listener, error){
		func(context.Context) (net.Listener, error) { return net.Listen("tcp", "127.0.0.1:0") },
	}}
	if _, err := New(cfg); !errors.Is(err, ErrUnauthenticatedBind) {
		t.Fatalf("New err = %v, want ErrUnauthenticatedBind", err)
	}
}

// spoofAddrListener wraps accepted connections so they report a fixed peer
// address, exercising the per-connection loopback bypass with a non-loopback
// peer over a real socket.
type spoofAddrListener struct {
	net.Listener
	addr net.Addr
}

func (l spoofAddrListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return spoofAddrConn{Conn: conn, addr: l.addr}, nil
}

type spoofAddrConn struct {
	net.Conn
	addr net.Addr
}

func (c spoofAddrConn) RemoteAddr() net.Addr { return c.addr }

func TestStartHTTPExtraListeners(t *testing.T) {
	const token = "s3cret-token"
	tests := []struct {
		name       string
		viaExtra   bool
		authHeader string
		wantStatus int
		wantBody   string
	}{
		{"extra listener serves the same routes with token", true, "Bearer " + token, http.StatusOK, "pong"},
		{"non-loopback peer on extra listener without token rejected", true, "", http.StatusUnauthorized, "unauthorized\n"},
		{"non-loopback peer on extra listener with wrong token rejected", true, "Bearer nope", http.StatusUnauthorized, "unauthorized\n"},
		{"loopback peer on primary still bypasses token", false, "", http.StatusOK, "pong"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateStateDir(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			var extraAddr string
			factory := func(context.Context) (net.Listener, error) {
				inner, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					return nil, err
				}
				extraAddr = inner.Addr().String()
				return spoofAddrListener{
					Listener: inner,
					addr:     &net.TCPAddr{IP: net.ParseIP("203.0.113.9"), Port: 41000},
				}, nil
			}
			s := &Server{
				paths:          testPaths(),
				log:            log.New(io.Discard, "", 0),
				httpToken:      token,
				extraListeners: []func(context.Context) (net.Listener, error){factory},
			}
			s.sse = sse.NewServer(s, sse.Config{})
			s.Mux().HandleFunc("/ping", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("pong"))
			})

			if err := s.startHTTP(ctx); err != nil {
				t.Fatalf("startHTTP: %v", err)
			}
			t.Cleanup(func() {
				cancel()
				s.wg.Wait()
			})

			addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(s.httpPort))
			if tt.viaExtra {
				addr = extraAddr
			}
			req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/ping", nil)
			if err != nil {
				t.Fatal(err)
			}
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("GET %s: %v", addr, err)
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			if got := string(body); got != tt.wantBody {
				t.Fatalf("body = %q, want %q", got, tt.wantBody)
			}
		})
	}
}

func TestStartHTTPExtraListenerFactoryErrorFailsStartup(t *testing.T) {
	isolateStateDir(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errBoom := errors.New("boom")
	var opened net.Listener
	factories := []func(context.Context) (net.Listener, error){
		func(context.Context) (net.Listener, error) {
			var err error
			opened, err = net.Listen("tcp", "127.0.0.1:0")
			return opened, err
		},
		func(context.Context) (net.Listener, error) { return nil, errBoom },
	}
	s := &Server{
		paths:          testPaths(),
		log:            log.New(io.Discard, "", 0),
		httpToken:      "s3cret-token",
		extraListeners: factories,
	}
	s.sse = sse.NewServer(s, sse.Config{})

	if err := s.startHTTP(ctx); !errors.Is(err, errBoom) {
		t.Fatalf("startHTTP err = %v, want errBoom", err)
	}
	// The deadline only bounds the failure path: on a closed listener (SetDeadline
	// errors too, ignored) Accept returns net.ErrClosed immediately.
	_ = opened.(*net.TCPListener).SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := opened.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("first extra listener Accept err = %v, want net.ErrClosed", err)
	}
	if port := s.readHTTPInfo().Port; port != 0 {
		t.Fatalf("handshake published port %d after failed startup, want none", port)
	}
}

func TestStartHTTPShutdownClosesExtraListeners(t *testing.T) {
	isolateStateDir(t)
	ctx, cancel := context.WithCancel(context.Background())

	var extra net.Listener
	factory := func(context.Context) (net.Listener, error) {
		var err error
		extra, err = net.Listen("tcp", "127.0.0.1:0")
		return extra, err
	}
	s := &Server{
		paths:          testPaths(),
		log:            log.New(io.Discard, "", 0),
		httpToken:      "s3cret-token",
		extraListeners: []func(context.Context) (net.Listener, error){factory},
	}
	s.sse = sse.NewServer(s, sse.Config{})

	if err := s.startHTTP(ctx); err != nil {
		t.Fatalf("startHTTP: %v", err)
	}
	cancel()
	s.wg.Wait()

	_ = extra.(*net.TCPListener).SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := extra.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("extra listener Accept err = %v, want net.ErrClosed", err)
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

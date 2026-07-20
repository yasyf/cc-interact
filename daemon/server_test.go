package daemon

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yasyf/cc-interact/sse"
	"github.com/yasyf/daemonkit/daemonrole"
	"github.com/yasyf/daemonkit/paths"
	"github.com/yasyf/daemonkit/wire"
)

func testPaths() paths.Paths { return paths.Paths{App: ".cc-interact-test"} }

func testDaemonRole(t *testing.T) daemonrole.Classifier {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	target, err := filepath.EvalSymlinks(executable)
	if err != nil {
		t.Fatal(err)
	}
	rolePath := filepath.Join(t.TempDir(), "cc-interact-test")
	if err := os.Symlink(target, rolePath); err != nil {
		t.Fatal(err)
	}
	return daemonrole.Classifier{RoleID: "com.yasyf.cc-interact.test", RolePath: rolePath}
}

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

func settleTestWorkers(t *testing.T, s *Server) {
	t.Helper()
	s.workers.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := s.workers.Wait(ctx); err != nil {
		t.Errorf("settle workers: %v", err)
	}
}

// shortHome points HOME at a short-prefix temp dir so the daemon's unix socket
// path stays under the sun_path length limit.
func shortHome(t *testing.T) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "cci-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("HOME", dir)
}

func TestNewRequiresDaemonRole(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("New accepted a missing daemon role")
	}
}

func TestLauncherRequiresDaemonRole(t *testing.T) {
	if _, err := (Launcher{}).NewClient(context.Background()); err == nil {
		t.Fatal("Launcher.NewClient accepted a missing daemon role")
	}
}

func TestLauncherAndServerShareStableDaemonRole(t *testing.T) {
	shortHome(t)
	role := testDaemonRole(t)
	launcher := Launcher{Paths: testPaths(), Version: "0.0.1", DaemonRole: role}
	if got := launcher.spawn(launcher.peer(), time.Second).ExecPath; got != role.RolePath {
		t.Fatalf("spawn executable = %q, want role path %q", got, role.RolePath)
	}
	s, err := New(Config{
		AppName: "cc-interact-test", Paths: testPaths(), Version: "0.0.1",
		DaemonRole: role, ActiveStatuses: []string{"open"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	wireServer, _, err := s.runtime()
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	got, ok := wireServer.ProtectedSessionClassifier.(daemonrole.Classifier)
	if !ok || got != role {
		t.Fatalf("protected classifier = %#v, want %#v", wireServer.ProtectedSessionClassifier, role)
	}
}

func TestStoreOpensOnlyAfterRuntimeOwnsListener(t *testing.T) {
	shortHome(t)
	var migrations atomic.Int32
	s, err := New(Config{
		AppName:        "cc-interact-test",
		Paths:          testPaths(),
		Version:        "0.0.1",
		DaemonRole:     testDaemonRole(t),
		ActiveStatuses: []string{"open"},
		Migrate: func(context.Context, *sql.DB) error {
			migrations.Add(1)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if s.DB() != nil || migrations.Load() != 0 {
		t.Fatalf("state activated before Serve: db=%v migrations=%d", s.DB(), migrations.Load())
	}

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- s.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-served:
			if err != nil {
				t.Errorf("Serve: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Serve did not return")
		}
	})

	deadline := time.Now().Add(5 * time.Second)
	for {
		probeCtx, probeCancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		client, connectErr := NewClient(probeCtx, ClientConfig{Socket: testPaths().SocketPath(), Build: "0.0.1"})
		if connectErr == nil {
			closeErr := client.Close()
			probeCancel()
			if closeErr != nil {
				t.Fatalf("close readiness client: %v", closeErr)
			}
			break
		}
		probeCancel()
		if time.Now().After(deadline) {
			t.Fatalf("daemon did not become ready: %v", connectErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if s.DB() == nil || migrations.Load() != 1 {
		t.Fatalf("state after readiness: db=%v migrations=%d", s.DB(), migrations.Load())
	}
}

func TestBackgroundWaitedBeforeServeReturns(t *testing.T) {
	shortHome(t)
	s, err := New(Config{
		AppName:        "cc-interact-test",
		Paths:          testPaths(),
		Version:        "0.0.1",
		DaemonRole:     testDaemonRole(t),
		ActiveStatuses: []string{"open"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- s.Serve(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		probeCtx, probeCancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		client, connectErr := NewClient(probeCtx, ClientConfig{
			Socket: testPaths().SocketPath(), Build: "0.0.1",
		})
		if connectErr == nil {
			health, healthErr := client.Health(probeCtx)
			_ = client.Close()
			probeCancel()
			if healthErr == nil && health.Protocol == int(wire.ProtocolVersion) {
				break
			}
		} else {
			probeCancel()
		}
		if time.Now().After(deadline) {
			t.Fatal("daemon did not come up")
		}
		time.Sleep(10 * time.Millisecond)
	}

	var finished atomic.Bool
	s.Background(func(ctx context.Context) {
		<-ctx.Done()
		time.Sleep(50 * time.Millisecond)
		finished.Store(true)
	})

	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return")
	}
	if !finished.Load() {
		t.Fatal("Serve returned before Background work finished")
	}
}

func TestServeDrainsBackgroundBeforeStoreCloseOnHTTPStartupFailure(t *testing.T) {
	shortHome(t)
	holder, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()

	writesStarted := make(chan struct{})
	releaseCleanup := make(chan struct{})
	workerDone := make(chan error, 1)
	const shutdownWrite = `
		INSERT INTO shutdown_probe(id, hits) VALUES('shutdown-probe', 0)
		ON CONFLICT(id) DO UPDATE SET hits=hits+1
	`
	s, err := New(Config{
		AppName:        "cc-interact-test",
		Paths:          testPaths(),
		Version:        "0.0.1",
		DaemonRole:     testDaemonRole(t),
		ActiveStatuses: []string{"open"},
		FixedPort:      boundPort(t, holder),
		Migrate: func(ctx context.Context, db *sql.DB) error {
			_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS shutdown_probe(id TEXT PRIMARY KEY, hits INTEGER NOT NULL)`)
			return err
		},
		BootReconcile: func(_ context.Context, s *Server) error {
			s.Background(func(ctx context.Context) {
				if _, err := s.DB().ExecContext(context.Background(), shutdownWrite); err != nil {
					workerDone <- err
					return
				}
				close(writesStarted)
				for {
					select {
					case <-ctx.Done():
						<-releaseCleanup
						_, err := s.DB().ExecContext(context.Background(), shutdownWrite)
						workerDone <- err
						return
					default:
						if _, err := s.DB().ExecContext(context.Background(), shutdownWrite); err != nil {
							workerDone <- err
							return
						}
					}
				}
			})
			<-writesStarted
			return nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	served := make(chan error, 1)
	go func() { served <- s.Serve(context.Background()) }()
	<-writesStarted

	var serveErr error
	returnedBeforeRelease := false
	select {
	case serveErr = <-served:
		returnedBeforeRelease = true
	case <-time.After(200 * time.Millisecond):
	}
	close(releaseCleanup)
	if !returnedBeforeRelease {
		select {
		case serveErr = <-served:
		case <-time.After(5 * time.Second):
			t.Fatal("Serve did not return")
		}
	}

	select {
	case err := <-workerDone:
		if err != nil {
			t.Errorf("background cleanup write: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Background worker did not return")
	}
	if returnedBeforeRelease {
		t.Error("Serve returned before Background worker finished")
	}
	var opErr *net.OpError
	if !errors.As(serveErr, &opErr) {
		t.Fatalf("Serve err = %v, want HTTP listen error", serveErr)
	}
	if opErr.Op != "listen" {
		t.Errorf("Serve listen op = %q, want listen", opErr.Op)
	}
	if got, want := opErr.Addr.String(), holder.Addr().String(); got != want {
		t.Errorf("Serve listen addr = %q, want %q", got, want)
	}
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
	if _, err := New(Config{BindAddr: "0.0.0.0", DaemonRole: testDaemonRole(t)}); !errors.Is(err, ErrUnauthenticatedBind) {
		t.Fatalf("New err = %v, want ErrUnauthenticatedBind", err)
	}
}

func TestNewRefusesUnauthenticatedExtraListeners(t *testing.T) {
	cfg := Config{DaemonRole: testDaemonRole(t), ExtraHTTPListeners: []func(context.Context) (net.Listener, error){
		func(context.Context) (net.Listener, error) { return net.Listen("tcp", "127.0.0.1:0") },
	}}
	if _, err := New(cfg); !errors.Is(err, ErrUnauthenticatedBind) {
		t.Fatalf("New err = %v, want ErrUnauthenticatedBind", err)
	}
}

func assertListenerClosed(t *testing.T, ln net.Listener) {
	t.Helper()
	_ = ln.(*net.TCPListener).SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := ln.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("listener Accept err = %v, want net.ErrClosed", err)
	}
}

func TestAddHTTPListener(t *testing.T) {
	const token = "s3cret-token"

	t.Run("added listener serves and republishes handshake", func(t *testing.T) {
		isolateStateDir(t)
		ctx, cancel := context.WithCancel(context.Background())
		s := &Server{
			paths:     testPaths(),
			log:       log.New(io.Discard, "", 0),
			httpToken: token,
			workers:   newWorkerGroup(),
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
			settleTestWorkers(t, s)
		})

		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		if err := s.AddHTTPListener(ln); err != nil {
			t.Fatalf("AddHTTPListener: %v", err)
		}

		if got := s.readHTTPInfo().ExtraAddrs; len(got) != 1 || got[0] != addr {
			t.Fatalf("handshake ExtraAddrs = %v, want [%s]", got, addr)
		}
		if got := s.HTTPExtraAddrs(); len(got) != 1 || got[0] != addr {
			t.Fatalf("HTTPExtraAddrs = %v, want [%s]", got, addr)
		}

		resp, err := http.Get("http://" + addr + "/ping")
		if err != nil {
			t.Fatalf("GET %s: %v", addr, err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK || string(body) != "pong" {
			t.Fatalf("GET %s = %d %q, want 200 pong", addr, resp.StatusCode, body)
		}
	})

	t.Run("before start errors and closes the listener", func(t *testing.T) {
		isolateStateDir(t)
		s := &Server{
			paths:     testPaths(),
			log:       log.New(io.Discard, "", 0),
			httpToken: token,
		}
		s.sse = sse.NewServer(s, sse.Config{})
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		if err := s.AddHTTPListener(ln); err == nil {
			t.Fatal("AddHTTPListener before start must error")
		}
		assertListenerClosed(t, ln)
	})

	t.Run("tokenless untrusted refuses and closes the listener", func(t *testing.T) {
		isolateStateDir(t)
		ctx, cancel := context.WithCancel(context.Background())
		s := &Server{
			paths:   testPaths(),
			log:     log.New(io.Discard, "", 0),
			workers: newWorkerGroup(),
		}
		s.sse = sse.NewServer(s, sse.Config{})
		if err := s.startHTTP(ctx); err != nil {
			t.Fatalf("startHTTP: %v", err)
		}
		t.Cleanup(func() {
			cancel()
			settleTestWorkers(t, s)
		})
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		if err := s.AddHTTPListener(ln); !errors.Is(err, ErrUnauthenticatedBind) {
			t.Fatalf("AddHTTPListener err = %v, want ErrUnauthenticatedBind", err)
		}
		assertListenerClosed(t, ln)
	})
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
				workers:        newWorkerGroup(),
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
				settleTestWorkers(t, s)
			})

			if got := s.readHTTPInfo().ExtraAddrs; len(got) != 1 || got[0] != extraAddr {
				t.Fatalf("handshake ExtraAddrs = %v, want [%s]", got, extraAddr)
			}

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
		workers:        newWorkerGroup(),
	}
	s.sse = sse.NewServer(s, sse.Config{})

	if err := s.startHTTP(ctx); err != nil {
		t.Fatalf("startHTTP: %v", err)
	}
	cancel()
	settleTestWorkers(t, s)

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
		workers:     newWorkerGroup(),
	}
	s.sse = sse.NewServer(s, sse.Config{})

	if err := s.startHTTP(ctx); err != nil {
		t.Fatalf("startHTTP: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		settleTestWorkers(t, s)
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

// TestStartHTTPWaitsForOnHTTPStartCleanup proves worker settlement blocks until a hook's
// ctx.Done cleanup finishes — the contract mDNS goodbye packets rely on to flush
// before the process exits.
func TestStartHTTPWaitsForOnHTTPStartCleanup(t *testing.T) {
	isolateStateDir(t)
	ctx, cancel := context.WithCancel(context.Background())

	release := make(chan struct{})
	var cleaned atomic.Bool
	s := &Server{
		paths:   testPaths(),
		log:     log.New(io.Discard, "", 0),
		workers: newWorkerGroup(),
		onHTTPStart: func(hookCtx context.Context, _ int) {
			<-hookCtx.Done()
			<-release
			cleaned.Store(true)
		},
	}
	s.sse = sse.NewServer(s, sse.Config{})

	if err := s.startHTTP(ctx); err != nil {
		t.Fatalf("startHTTP: %v", err)
	}

	cancel()
	waited := make(chan struct{})
	go func() {
		s.workers.Close()
		_ = s.workers.Wait(context.Background())
		close(waited)
	}()

	// The hook has unblocked from ctx.Done but is parked on release; settlement must
	// not return until it completes.
	select {
	case <-waited:
		t.Fatal("worker settlement returned before the onHTTPStart hook finished cleanup")
	case <-time.After(200 * time.Millisecond):
	}

	close(release)
	<-waited
	if !cleaned.Load() {
		t.Fatal("onHTTPStart cleanup did not run")
	}
}

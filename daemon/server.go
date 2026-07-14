package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/yasyf/cc-interact/event"
	"github.com/yasyf/cc-interact/paths"
	"github.com/yasyf/cc-interact/sse"
	"github.com/yasyf/cc-interact/store"
	"github.com/yasyf/cc-interact/subject"
	"github.com/yasyf/cc-interact/version"
)

// handleTimeout bounds a single control RPC. It is generous because a domain
// handler may shell out (cc-review's start snapshots the tree); core ops are
// sub-second.
const handleTimeout = 35 * time.Second

// defaultEvictTimeout bounds each phase of evicting a version-skewed holder: the
// graceful-shutdown wait, the post-SIGKILL wait, and the process-exit wait.
const defaultEvictTimeout = 5 * time.Second

// attachGrace is how recently a subject's last SSE attachment must have dropped
// for it to still report as connected in status.
const attachGrace = 10 * time.Second

// Server is the running daemon: the control-plane unix-socket server plus the
// realtime HTTP/SSE plane it boots. It implements sse.Backend.
type Server struct {
	appName  string
	version  string
	store    *store.Store
	db       *sql.DB
	bus      *event.Bus
	activity *Activity
	subjects subject.Resolver
	sse      *sse.Server

	scopeResolve    func(ctx context.Context, raw string) string
	gate            GateFunc
	gateErrorReason string
	gateObserve     func(ctx context.Context, s subject.Subject, tool ToolCall, allow bool, reason string)
	bootReconcile   func(ctx context.Context, s *Server) error

	paths  paths.Paths
	socket string
	log    *log.Logger

	handlers map[Op]HandlerFunc

	fixedPort      int
	httpPort       int
	bindAddr       string
	httpToken      string
	onHTTPStart    func(ctx context.Context, port int)
	extraListeners []func(ctx context.Context) (net.Listener, error)
	evictTimeout   time.Duration

	repoMu    sync.Mutex
	repoLocks map[string]*sync.Mutex

	triggerShutdown context.CancelFunc
	wg              sync.WaitGroup
}

// New opens the store, builds the bus, resolver, presence registry, and SSE
// plane, and returns a Server ready for the consumer to Register domain ops and
// mount routes on Mux before calling Serve.
func New(cfg Config) (*Server, error) {
	if err := validateBindAuth(bindHostOrDefault(cfg.BindAddr), cfg.HTTPToken, len(cfg.ExtraHTTPListeners) > 0); err != nil {
		return nil, err
	}
	if err := cfg.Paths.EnsureStateDir(); err != nil {
		return nil, err
	}
	st, err := store.Open(cfg.Paths.DBPath(), cfg.Migrate)
	if err != nil {
		return nil, err
	}
	scopeResolve := cfg.ScopeResolve
	if scopeResolve == nil {
		scopeResolve = func(_ context.Context, raw string) string { return raw }
	}
	s := &Server{
		appName:         cfg.AppName,
		version:         cfg.Version,
		store:           st,
		db:              st.DB(),
		bus:             event.NewBus(),
		activity:        NewActivity(),
		scopeResolve:    scopeResolve,
		gate:            cfg.Gate,
		gateErrorReason: cfg.GateErrorReason,
		gateObserve:     cfg.GateObserve,
		bootReconcile:   cfg.BootReconcile,
		paths:           cfg.Paths,
		socket:          cfg.Paths.SocketPath(),
		log:             log.New(os.Stderr, "["+cfg.AppName+"] ", log.LstdFlags),
		handlers:        make(map[Op]HandlerFunc),
		fixedPort:       cfg.FixedPort,
		bindAddr:        cfg.BindAddr,
		httpToken:       cfg.HTTPToken,
		onHTTPStart:     cfg.OnHTTPStart,
		extraListeners:  cfg.ExtraHTTPListeners,
		evictTimeout:    defaultEvictTimeout,
		repoLocks:       make(map[string]*sync.Mutex),
	}
	s.subjects = subject.Resolver{
		Store: store.NewSubjectStore(s.db),
		Policy: subject.Policy{
			Active: func(sub subject.Subject) bool {
				for _, st := range cfg.ActiveStatuses {
					if sub.Status == st {
						return true
					}
				}
				return false
			},
		},
	}
	var ssePresence func(ctx context.Context, subjectID string, connected bool)
	if cfg.OnPresenceChange != nil {
		onPresence := cfg.OnPresenceChange
		ssePresence = func(ctx context.Context, subjectID string, connected bool) {
			onPresence(ctx, s, subjectID, connected)
		}
	}
	s.sse = sse.NewServer(s, sse.Config{
		OnPresenceChange:  ssePresence,
		PresenceEventType: cfg.PresenceEventType,
		PresenceDebounce:  cfg.PresenceDebounce,
	})
	// Core ops ride the same registry as domain ops. A scope with no canonical
	// form reaches them as the resolver's fallback value, which matches no
	// subject — so guard-edit allows, session-record no-ops, status reports bare
	// daemon liveness, and resolve returns no subject. Degradation falls out of
	// resolution, not per-op policy.
	s.Register(OpResolve, s.handleResolve)
	s.Register(OpSessionRecord, s.handleSessionRecord)
	s.Register(OpGuardEdit, s.handleGuardEdit)
	s.Register(OpStatus, s.handleStatus)
	s.Register(OpChannelAck, s.handleChannelAck)
	return s, nil
}

// Mux exposes the SSE server's mux so the consumer can mount its REST surface
// and the opt-in static handler before Serve. GET /events is already mounted.
func (s *Server) Mux() *http.ServeMux { return s.sse.Mux() }

// DB exposes the underlying connection so a lifecycle hook or domain handler can
// query the consumer's own tables.
func (s *Server) DB() *sql.DB { return s.db }

// Serve binds (and evicts any older holder of) the control socket, runs the boot
// reconcile, boots the HTTP plane, then serves control RPCs until ctx is
// cancelled or a shutdown op arrives. It closes the store on return.
func (s *Server) Serve(parent context.Context) error {
	defer s.store.Close()

	ctx, stop := signal.NotifyContext(parent, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	s.triggerShutdown = stop

	// Bind the control socket before publishing the HTTP handshake; connections
	// queue in the listener backlog until the accept loop starts, so nothing
	// observes the gap.
	ln, err := s.listen()
	if err != nil {
		return err
	}
	var once sync.Once
	closeListener := func() { once.Do(func() { _ = ln.Close() }) }
	defer closeListener()

	if s.bootReconcile != nil {
		if err := s.bootReconcile(ctx, s); err != nil {
			return err
		}
	}
	if err := s.startHTTP(ctx); err != nil {
		return err
	}

	s.log.Printf("daemon %s started; socket=%s http=%s", s.version, s.socket, net.JoinHostPort(s.bindHost(), strconv.Itoa(s.httpPort)))

	go func() {
		<-ctx.Done()
		closeListener()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				break
			}
			s.log.Printf("accept: %v", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handle(ctx, conn)
		}()
	}

	s.wg.Wait()
	s.log.Printf("daemon stopped")
	return nil
}

// listen binds the control socket, first evicting any strictly older daemon
// holding it. A stale socket left by a crashed daemon is removed before binding;
// the lazy-start flock prevents two live daemons from racing here.
func (s *Server) listen() (net.Listener, error) {
	if err := s.evictHolder(); err != nil {
		return nil, err
	}
	_ = os.Remove(s.socket)
	if err := os.MkdirAll(filepath.Dir(s.socket), 0o700); err != nil {
		return nil, err
	}
	ln, err := net.Listen("unix", s.socket)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(s.socket, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// evictHolder clears a strictly older daemon holding the socket: ask it to step
// down, then SIGKILL the exact socket peer if it wedges. A same-or-newer holder
// is never evicted — refusing the tie is what prevents two daemons from evicting
// each other, and refusing a newer holder makes a spawned older daemon exit
// while its spawning client converges on the newer holder.
func (s *Server) evictHolder() error {
	c := NewClient(s.socket)
	resp, err := c.Health()
	if err != nil {
		return nil // no live holder; a stale socket file is removed by listen
	}
	if !version.Newer(s.version, resp.DaemonVersion) {
		return fmt.Errorf("%s daemon %s already holds the socket (this binary is %s)", s.appName, resp.DaemonVersion, s.version)
	}
	s.log.Printf("evicting older daemon (%s) holding the socket", resp.DaemonVersion)
	pid, _ := c.peerPID() // grab before shutdown: the peer is gone afterwards
	if _, err := c.Shutdown(); err != nil {
		return fmt.Errorf("evict holder %s: %w", resp.DaemonVersion, err)
	}
	if !c.WaitGone(s.evictTimeout) {
		if _, err := c.KillHolder(); err != nil {
			s.log.Printf("kill holder: %v", err)
		}
		if !c.WaitGone(s.evictTimeout) {
			return fmt.Errorf("holder %s did not release the socket within %s", resp.DaemonVersion, s.evictTimeout)
		}
	}
	// Wait for the peer process to exit so a successor's handshake is not
	// clobbered by a dying predecessor, and so the port can be reused.
	if pid > 1 && pid != os.Getpid() {
		deadline := time.Now().Add(s.evictTimeout)
		for time.Now().Before(deadline) {
			if err := killProc(pid, syscall.Signal(0)); errors.Is(err, syscall.ESRCH) {
				return nil
			}
			time.Sleep(100 * time.Millisecond)
		}
		s.log.Printf("holder pid %d still exiting; the handshake may be rewritten once", pid)
	}
	return nil
}

// bindHost is the address the HTTP plane binds, defaulting to loopback-only
// 127.0.0.1 when Config leaves BindAddr empty.
func (s *Server) bindHost() string { return bindHostOrDefault(s.bindAddr) }

// bindHostOrDefault applies the loopback-only 127.0.0.1 default to a configured
// bind address so New can resolve the effective host before a Server exists.
func bindHostOrDefault(addr string) string {
	if addr == "" {
		return "127.0.0.1"
	}
	return addr
}

// listenHTTP binds the HTTP plane on bindHost. A fixed dev port binds exactly or
// fails loud; otherwise the port last published to the handshake is tried first
// so printed URLs survive a daemon swap, falling back to an ephemeral port.
func (s *Server) listenHTTP() (net.Listener, error) {
	host := s.bindHost()
	if s.fixedPort != 0 {
		return net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(s.fixedPort)))
	}
	if prev := s.readHTTPInfo().Port; prev != 0 {
		if ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(prev))); err == nil {
			return ln, nil
		}
	}
	return net.Listen("tcp", net.JoinHostPort(host, "0"))
}

// startHTTP binds the HTTP plane on bindHost plus every extra listener, publishes
// the port handshake, and serves until ctx is cancelled. Every listener carries
// the same server, so the auth middleware wraps the whole mux everywhere and one
// graceful Shutdown drains them all. Request contexts derive from ctx
// (BaseContext), so cancelling it ends every parked SSE handler before the
// graceful Shutdown drains them — and before Serve closes the store.
func (s *Server) startHTTP(ctx context.Context) error {
	ln, err := s.listenHTTP()
	if err != nil {
		return err
	}
	listeners := []net.Listener{ln}
	closeAll := func() {
		for _, l := range listeners {
			_ = l.Close()
		}
	}
	for _, factory := range s.extraListeners {
		extra, err := factory(ctx)
		if err != nil {
			closeAll()
			return fmt.Errorf("extra HTTP listener: %w", err)
		}
		listeners = append(listeners, extra)
	}
	s.httpPort = ln.Addr().(*net.TCPAddr).Port
	if err := s.writeHTTPInfo(HTTPInfo{Port: s.httpPort, Bind: s.bindHost()}); err != nil {
		closeAll()
		return err
	}
	if s.onHTTPStart != nil {
		go s.onHTTPStart(ctx, s.httpPort)
	}
	srv := &http.Server{
		Handler:     authHandler(s.httpToken, s.sse.Handler()),
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	for _, l := range listeners {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
				s.log.Printf("http serve %s: %v", l.Addr(), err)
			}
		}()
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	return nil
}

func (s *Server) handle(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(handleTimeout))
	var env Envelope
	if err := json.NewDecoder(conn).Decode(&env); err != nil {
		s.writeReply(conn, Reply{OK: false, Error: "bad request: " + err.Error()})
		return
	}
	s.writeReply(conn, s.dispatch(ctx, env))
}

// repoLock returns the mutex serializing scope-bound work (cc-review: working-tree
// snapshots) so a turn boundary and a capture never describe interleaved state.
func (s *Server) repoLock(scope string) *sync.Mutex {
	s.repoMu.Lock()
	defer s.repoMu.Unlock()
	mu, ok := s.repoLocks[scope]
	if !ok {
		mu = &sync.Mutex{}
		s.repoLocks[scope] = mu
	}
	return mu
}

// dispatch answers health and shutdown before anything else (cross-version
// eviction depends on both working regardless of protocol version), then routes
// every other op through the registry: Config.ScopeResolve canonicalizes the
// scope once and the handler runs with the result.
func (s *Server) dispatch(ctx context.Context, env Envelope) Reply {
	switch env.Op {
	case OpHealth:
		return Reply{OK: true, DaemonVersion: s.version}
	case OpShutdown:
		s.triggerShutdown()
		return Reply{OK: true}
	}
	if env.Proto != ProtocolVersion {
		return errReply(fmt.Sprintf(
			"%s protocol skew: daemon speaks v%d, request is v%d — this session is pinned to an older plugin version; restart the session to pick up the current one",
			s.appName, ProtocolVersion, env.Proto))
	}
	handler, ok := s.handlers[env.Op]
	if !ok {
		return errReply("unknown op: " + string(env.Op))
	}
	scope := s.scopeResolve(ctx, env.Scope)
	return handler(HandlerCtx{
		Ctx:      ctx,
		Env:      env,
		Window:   subject.Window{Session: env.Session, ClaudePID: env.ClaudePID},
		Scope:    scope,
		Subjects: s.subjects,
		DB:       s.db,
		Append:   s.Append,
		HTTPPort: s.httpPort,
		Activity: s.activity,
		RepoLock: s.repoLock(scope),
	})
}

func (s *Server) writeReply(conn net.Conn, r Reply) {
	r.Proto = ProtocolVersion
	_ = json.NewEncoder(conn).Encode(r)
}

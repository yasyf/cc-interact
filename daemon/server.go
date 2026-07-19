package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/yasyf/cc-interact/event"
	"github.com/yasyf/cc-interact/sse"
	"github.com/yasyf/cc-interact/store"
	"github.com/yasyf/cc-interact/subject"
	"github.com/yasyf/daemonkit/drain"
	"github.com/yasyf/daemonkit/paths"
	"github.com/yasyf/daemonkit/proc"
	"github.com/yasyf/daemonkit/wire"
)

// handleTimeout bounds a single control RPC. It is generous because a domain
// handler may shell out (cc-review's start snapshots the tree); core ops are
// sub-second.
const handleTimeout = 35 * time.Second

// maxFrameBytes caps one request frame at 64 MiB so a guard-edit carrying a
// whole Write payload remains visible to the gate.
const maxFrameBytes = 64 << 20

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

	agentGate     AgentGateFunc
	agentGreeting AgentGreetingFunc
	parks         *parkRegistry
	gateBlocks    *gateBlockCounter

	paths         paths.Paths
	socket        string
	maxFrameBytes int
	log           *log.Logger

	handlers map[Op]HandlerFunc

	fixedPort      int
	httpPort       int
	bindAddr       string
	httpToken      string
	trustedPeer    func(ip netip.Addr) bool
	trustedOrigin  func(host string) bool
	onHTTPStart    func(ctx context.Context, port int)
	extraListeners []func(ctx context.Context) (net.Listener, error)
	publicHandler  http.Handler
	evictTimeout   time.Duration

	repoMu    sync.Mutex
	repoLocks map[string]*sync.Mutex

	drain *drain.Simple

	serveCtxMu      sync.Mutex
	serveCtx        context.Context
	triggerShutdown context.CancelFunc
	wg              sync.WaitGroup
}

// New opens the store, builds the bus, resolver, presence registry, and SSE
// plane, and returns a Server ready for the consumer to Register domain ops and
// mount routes on Mux before calling Serve.
func New(cfg Config) (*Server, error) {
	if err := validateBindAuth(bindHostOrDefault(cfg.BindAddr), cfg.HTTPToken, len(cfg.ExtraHTTPListeners) > 0, cfg.TrustedPeer != nil); err != nil {
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
	frameBytes := cfg.MaxFrameBytes
	if frameBytes == 0 {
		frameBytes = maxFrameBytes
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
		agentGate:       cfg.AgentGate,
		agentGreeting:   cfg.AgentGreeting,
		parks:           newParkRegistry(),
		gateBlocks:      newGateBlockCounter(),
		paths:           cfg.Paths,
		socket:          cfg.Paths.SocketPath(),
		maxFrameBytes:   frameBytes,
		log:             log.New(os.Stderr, "["+cfg.AppName+"] ", log.LstdFlags),
		handlers:        make(map[Op]HandlerFunc),
		fixedPort:       cfg.FixedPort,
		bindAddr:        cfg.BindAddr,
		httpToken:       cfg.HTTPToken,
		trustedPeer:     cfg.TrustedPeer,
		trustedOrigin:   cfg.TrustedOrigin,
		onHTTPStart:     cfg.OnHTTPStart,
		extraListeners:  cfg.ExtraHTTPListeners,
		publicHandler:   cfg.PublicHandler,
		evictTimeout:    defaultEvictTimeout,
		repoLocks:       make(map[string]*sync.Mutex),
		drain:           &drain.Simple{},
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
		Admit:             s.drain.Admit,
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
	s.Register(OpAgentStart, s.handleAgentStart)
	s.Register(OpAgentStop, s.handleAgentStop)
	s.Register(OpAgentInject, s.handleAgentInject)
	s.Register(OpAgentDirect, s.handleAgentDirect)
	s.Register(OpAgentReconcile, s.handleAgentReconcile)
	// The agent participant plane rides the SSE mux beside GET /events: a
	// long-poll await for directives and a roster read, both under the same auth.
	s.sse.Mux().HandleFunc("GET /agents/await", s.handleAgentAwait)
	s.sse.Mux().HandleFunc("GET /agents", s.handleAgentRoster)
	return s, nil
}

// Mux exposes the SSE server's mux so the consumer can mount its REST surface
// and the opt-in static handler before Serve. GET /events is already mounted.
func (s *Server) Mux() *http.ServeMux { return s.sse.Mux() }

// DB exposes the underlying connection so a lifecycle hook or domain handler can
// query the consumer's own tables.
func (s *Server) DB() *sql.DB { return s.db }

// Background runs fn as daemon-lifecycle work: fn receives the serve context
// (cancelled at shutdown) and Serve waits for it to return before closing the
// store, so consumer fan-out never outlives the daemon or writes to a closed
// DB. Call it from a handler — before Serve there is no serve context.
func (s *Server) Background(fn func(context.Context)) {
	s.serveCtxMu.Lock()
	ctx := s.serveCtx
	s.serveCtxMu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		fn(ctx)
	}()
}

// Serve binds (and evicts any older holder of) the control socket, runs the boot
// reconcile, boots the HTTP plane, then serves control RPCs until ctx is
// cancelled or a shutdown op arrives. It closes the store on return.
func (s *Server) Serve(parent context.Context) error {
	defer s.store.Close()
	defer s.wg.Wait()

	ctx, stop := signal.NotifyContext(parent, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	s.triggerShutdown = stop
	s.serveCtxMu.Lock()
	s.serveCtx = ctx
	s.serveCtxMu.Unlock()

	// Bind the control socket before publishing the HTTP handshake; connections
	// queue in the listener backlog until the accept loop starts, so nothing
	// observes the gap.
	ln, lock, err := s.listen(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }() // held for the listener's life; see SingleEntrant
	var once sync.Once
	closeListener := func() { once.Do(func() { _ = ln.Close() }) }
	defer closeListener()

	if s.bootReconcile != nil {
		if err := s.bootReconcile(ctx, s); err != nil {
			return err
		}
	}
	if err := s.reconcileDirectives(ctx); err != nil {
		return err
	}
	if err := s.startHTTP(ctx); err != nil {
		return err
	}

	s.log.Printf("daemon %s started; socket=%s http=%s", s.version, s.socket, net.JoinHostPort(s.bindHost(), strconv.Itoa(s.httpPort)))

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		<-ctx.Done()
		// Settle in-flight SSE streams and refuse new admissions; startHTTP closes HTTP.
		drainCtx, cancel := context.WithTimeout(context.Background(), handleTimeout)
		defer cancel()
		_ = s.drain.Drain(drainCtx, drain.SimpleConfig{
			Deactivate:      func(context.Context) error { closeListener(); return nil },
			MarkClosing:     func() {},
			CancelExecutors: func() {},
		})
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

// listen binds the control socket single-entrant, evicting any strictly older
// holder through the version-gated takeover (s.evict). The returned lock is held
// for the listener's life.
func (s *Server) listen(ctx context.Context) (net.Listener, *os.File, error) {
	se := proc.SingleEntrant{
		Socket:  s.socket,
		Evict:   func() (bool, error) { return s.evict(ctx) },
		Timeout: s.evictTimeout,
	}
	return se.Listen(ctx)
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
	var extraAddrs []string
	for _, l := range listeners[1:] {
		extraAddrs = append(extraAddrs, l.Addr().String())
	}
	if err := s.writeHTTPInfo(HTTPInfo{Port: s.httpPort, Bind: s.bindHost(), ExtraAddrs: extraAddrs}); err != nil {
		closeAll()
		return err
	}
	if s.onHTTPStart != nil {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.onHTTPStart(ctx, s.httpPort)
		}()
	}
	handler := authHandler(s.httpToken, s.trustedPeer, s.trustedOrigin, peerReauthInterval, s.sse.Handler())
	if s.publicHandler != nil {
		handler = publicFallback(s.sse.Mux(), handler, s.publicHandler)
	}
	srv := &http.Server{
		Handler:     handler,
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
	f := wire.NewFraming(conn)
	f.MaxLine = s.maxFrameBytes
	peer, err := wire.PeerFromConn(conn.(*net.UnixConn))
	if err != nil {
		s.writeReply(conn, Reply{OK: false, Error: "peer credentials: " + err.Error()})
		return
	}
	if peer.UID != os.Geteuid() {
		s.log.Printf("refusing peer uid %d (daemon euid %d)", peer.UID, os.Geteuid())
		s.writeReply(conn, Reply{OK: false, Error: "untrusted peer"})
		return
	}
	var env Envelope
	if err := f.ReadJSON(&env); err != nil {
		if errors.Is(err, wire.ErrFrameTooLarge) {
			s.writeReply(conn, Reply{OK: false, Error: wire.ErrFrameTooLarge.Error()})
			return
		}
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
			s.appName, ProtocolVersion, env.Proto,
		))
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

// Dispatch answers a single envelope through the daemon's op table, exactly as a
// socket connection would. It exists for consumer-mounted HTTP bridges; callers
// stamp Session, ClaudePID, and Scope themselves.
func (s *Server) Dispatch(ctx context.Context, env Envelope) Reply {
	return s.dispatch(ctx, env)
}

func (s *Server) writeReply(conn net.Conn, r Reply) {
	r.Proto = ProtocolVersion
	_ = wire.NewFraming(conn).WriteJSON(r)
}

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

// attachGrace is how recently a pid-less subject's last SSE attachment must have
// dropped for Policy.Held to still consider it occupied.
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

	windowAlive     func(pid int) bool
	scopeResolve    func(ctx context.Context, raw string) (string, error)
	gate            GateFunc
	gateErrorReason string
	gateObserve     func(ctx context.Context, s subject.Subject, tool ToolCall, allow bool, reason string)
	bootReconcile   func(ctx context.Context, s *Server) error

	paths  paths.Paths
	socket string
	log    *log.Logger

	handlers map[Op]HandlerFunc

	fixedPort    int
	httpPort     int
	evictTimeout time.Duration

	repoMu    sync.Mutex
	repoLocks map[string]*sync.Mutex

	triggerShutdown context.CancelFunc
	wg              sync.WaitGroup
}

// New opens the store, builds the bus, resolver, presence registry, and SSE
// plane, and returns a Server ready for the consumer to Register domain ops and
// mount routes on Mux before calling Serve.
func New(cfg Config) (*Server, error) {
	if err := cfg.Paths.EnsureStateDir(); err != nil {
		return nil, err
	}
	st, err := store.Open(cfg.Paths.DBPath(), cfg.Migrate)
	if err != nil {
		return nil, err
	}
	scopeResolve := cfg.ScopeResolve
	if scopeResolve == nil {
		scopeResolve = func(_ context.Context, raw string) (string, error) { return raw, nil }
	}
	s := &Server{
		appName:         cfg.AppName,
		version:         cfg.Version,
		store:           st,
		db:              st.DB(),
		bus:             event.NewBus(),
		activity:        NewActivity(),
		windowAlive:     cfg.WindowAlive,
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
		evictTimeout:    defaultEvictTimeout,
		repoLocks:       make(map[string]*sync.Mutex),
	}
	s.subjects = subject.Resolver{
		Store: store.NewSubjectStore(s.db, cfg.ActiveStatuses),
		Policy: subject.Policy{
			Held: func(_ context.Context, sub subject.Subject) bool {
				if sub.ClaudePID != 0 {
					return s.windowAlive(sub.ClaudePID)
				}
				return s.activity.AttachedWithin(sub.ID, attachGrace)
			},
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

	s.log.Printf("daemon %s started; socket=%s http=127.0.0.1:%d", s.version, s.socket, s.httpPort)

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

// listenHTTP binds the HTTP plane. A fixed dev port binds exactly or fails loud;
// otherwise the port last published to the handshake is tried first so printed
// URLs survive a daemon swap, falling back to an ephemeral port.
func (s *Server) listenHTTP() (net.Listener, error) {
	if s.fixedPort != 0 {
		return net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", s.fixedPort))
	}
	if prev := s.readHTTPInfo().Port; prev != 0 {
		if ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", prev)); err == nil {
			return ln, nil
		}
	}
	return net.Listen("tcp", "127.0.0.1:0")
}

// startHTTP binds the HTTP plane on 127.0.0.1, publishes the port handshake, and
// serves until ctx is cancelled. Request contexts derive from ctx (BaseContext),
// so cancelling it ends every parked SSE handler before the graceful Shutdown
// drains them — and before Serve closes the store.
func (s *Server) startHTTP(ctx context.Context) error {
	ln, err := s.listenHTTP()
	if err != nil {
		return err
	}
	s.httpPort = ln.Addr().(*net.TCPAddr).Port
	if err := s.writeHTTPInfo(HTTPInfo{Port: s.httpPort}); err != nil {
		ln.Close()
		return err
	}
	srv := &http.Server{
		Handler:     s.sse.Handler(),
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Printf("http serve: %v", err)
		}
	}()
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

// dispatch answers the core ops internally and routes everything else to the
// registered handlers. health and shutdown answer regardless of protocol version
// (cross-version eviction depends on both); channel-ack needs no scope; every
// other op runs Config.ScopeResolve once and reaches its handler through a
// HandlerCtx with the resolved scope.
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
	if env.Op == OpChannelAck {
		return s.handleChannelAck(env)
	}
	scope, err := s.scopeResolve(ctx, env.Scope)
	if err != nil {
		// An unresolvable scope is "not in a guarded domain": guard-edit allows,
		// session-record is a no-op, status still reports daemon liveness; every
		// other op is a real error.
		switch env.Op {
		case OpGuardEdit:
			return Reply{OK: true, Allow: true}
		case OpSessionRecord:
			return Reply{OK: true}
		case OpStatus:
			return Reply{OK: true, DaemonVersion: s.version, HTTPPort: s.httpPort}
		default:
			return errReply(err.Error())
		}
	}
	hc := HandlerCtx{
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
	}
	switch env.Op {
	case OpResolve:
		return s.handleResolve(hc)
	case OpSessionRecord:
		return s.handleSessionRecord(hc)
	case OpGuardEdit:
		return s.handleGuardEdit(hc)
	case OpStatus:
		return s.handleStatus(hc)
	default:
		if h, ok := s.handlers[env.Op]; ok {
			return h(hc)
		}
		return errReply("unknown op: " + string(env.Op))
	}
}

func (s *Server) writeReply(conn net.Conn, r Reply) {
	r.Proto = ProtocolVersion
	_ = json.NewEncoder(conn).Encode(r)
}

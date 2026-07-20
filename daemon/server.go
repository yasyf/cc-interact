package daemon

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/yasyf/cc-interact/event"
	"github.com/yasyf/cc-interact/sse"
	"github.com/yasyf/cc-interact/store"
	"github.com/yasyf/cc-interact/subject"
	dkdaemon "github.com/yasyf/daemonkit/daemon"
	"github.com/yasyf/daemonkit/drain"
	"github.com/yasyf/daemonkit/paths"
	"github.com/yasyf/daemonkit/wire"
)

// handleTimeout bounds a single control RPC. It is generous because a domain
// handler may shell out (cc-review's start snapshots the tree); core ops are
// sub-second.
const handleTimeout = 35 * time.Second

// maxFrameBytes caps one request frame at 64 MiB so a guard-edit carrying a
// whole Write payload remains visible to the gate.
const maxFrameBytes = 64 << 20

// attachGrace is how recently a subject's last SSE attachment must have dropped
// for it to still report as connected in status.
const attachGrace = 10 * time.Second

// subscriberPresenceWindow is how long after a subscriber's last mailbox drain it
// still counts as present for muting, bridging the gap between a delivering await
// and the next park so a live handler's events stay muted from the channel.
const subscriberPresenceWindow = 90 * time.Second

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
	subscribe     SubscribeFunc
	subscriptions *subscriptionRegistry
	muteConsumer  string
	parks         *parkRegistry
	gateBlocks    *gateBlockCounter

	paths         paths.Paths
	socket        string
	maxFrameBytes int
	log           *log.Logger

	handlersMu          sync.RWMutex
	handlers            map[Op]HandlerFunc
	registrationsClosed bool

	fixedPort      int
	httpPort       int
	bindAddr       string
	httpToken      string
	trustedPeer    func(ip netip.Addr) bool
	trustedOrigin  func(host string) bool
	onHTTPStart    func(ctx context.Context, port int)
	extraListeners []func(ctx context.Context) (net.Listener, error)
	publicHandler  http.Handler

	httpMu     sync.Mutex
	httpSrv    *http.Server
	extraAddrs []string

	repoMu    sync.Mutex
	repoLocks map[string]*sync.Mutex

	wireIntake *drain.Intake
	httpIntake *drain.Intake

	serveCtxMu    sync.Mutex
	serveCtx      context.Context
	serveCancel   context.CancelFunc
	workersClosed bool
	wg            sync.WaitGroup
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
		subscribe:       cfg.Subscribe,
		subscriptions:   newSubscriptionRegistry(),
		muteConsumer:    cfg.MuteConsumer,
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
		repoLocks:       make(map[string]*sync.Mutex),
		wireIntake:      &drain.Intake{},
		httpIntake:      &drain.Intake{},
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
	var muteFrame func(subjectID, consumer string, e event.Event) bool
	if cfg.Subscribe != nil && cfg.MuteConsumer != "" {
		muteFrame = s.muteFrame
	}
	s.sse = sse.NewServer(s, sse.Config{
		OnPresenceChange:  ssePresence,
		PresenceEventType: cfg.PresenceEventType,
		PresenceDebounce:  cfg.PresenceDebounce,
		Admit:             s.httpIntake.Admit,
		MuteFrame:         muteFrame,
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
	s.Register(OpAgentReport, s.handleAgentReport)
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
	closed := s.workersClosed
	if ctx == nil {
		s.serveCtxMu.Unlock()
		panic("daemon: Background called before Serve")
	}
	if closed {
		s.serveCtxMu.Unlock()
		panic("daemon: Background called while draining")
	}
	s.wg.Add(1)
	s.serveCtxMu.Unlock()
	go func() {
		defer s.wg.Done()
		fn(ctx)
	}()
}

// Serve runs the exact daemonkit lifecycle and persistent control session.
func (s *Server) Serve(parent context.Context) error {
	wireServer, runtime, err := s.runtime()
	if err != nil {
		_ = s.store.Close()
		return err
	}
	wireServer.RegisterLifecycle(runtime)
	err = runtime.Run(parent)
	if parent.Err() != nil && errors.Is(err, parent.Err()) {
		return nil
	}
	return err
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
	handler := authHandler(s.httpToken, s.trustedPeer, s.trustedOrigin, peerReauthInterval, s.sse.Handler())
	if s.publicHandler != nil {
		handler = publicFallback(s.sse.Mux(), handler, s.publicHandler)
	}
	srv := &http.Server{
		Handler:     handler,
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	s.httpMu.Lock()
	s.httpSrv = srv
	s.extraAddrs = extraAddrs
	writeErr := s.writeHTTPInfo(s.httpInfoLocked())
	s.httpMu.Unlock()
	if writeErr != nil {
		closeAll()
		return writeErr
	}
	if s.onHTTPStart != nil {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.onHTTPStart(ctx, s.httpPort)
		}()
	}
	for _, l := range listeners {
		s.wg.Add(1)
		go s.serveOn(l)
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

// serveOn runs the shared HTTP server on ln until a graceful Shutdown closes it.
func (s *Server) serveOn(ln net.Listener) {
	defer s.wg.Done()
	if err := s.httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		s.log.Printf("http serve %s: %v", ln.Addr(), err)
	}
}

// httpInfoLocked builds the handshake for the live port and extra addrs; the
// caller holds httpMu.
func (s *Server) httpInfoLocked() HTTPInfo {
	return HTTPInfo{Port: s.httpPort, Bind: s.bindHost(), ExtraAddrs: s.extraAddrs}
}

// AddHTTPListener serves the HTTP/SSE plane on ln at runtime, republishing the
// handshake with ln's address before serving. It refuses — closing ln — before
// start, while draining, or for a tokenless untrusted daemon (per
// validateBindAuth). Legs are never removed: a vanished address is inert, auth
// being per-request and watchPeerTrust already closing revoked streams.
func (s *Server) AddHTTPListener(ln net.Listener) error {
	if err := validateBindAuth(s.bindHost(), s.httpToken, true, s.trustedPeer != nil); err != nil {
		_ = ln.Close()
		return err
	}
	s.httpMu.Lock()
	defer s.httpMu.Unlock()
	if s.httpSrv == nil {
		_ = ln.Close()
		return errors.New("daemon: AddHTTPListener before HTTP start")
	}
	s.serveCtxMu.Lock()
	if s.workersClosed {
		s.serveCtxMu.Unlock()
		_ = ln.Close()
		return errors.New("daemon: AddHTTPListener while draining")
	}
	s.wg.Add(1)
	s.serveCtxMu.Unlock()
	s.extraAddrs = append(s.extraAddrs, ln.Addr().String())
	if err := s.writeHTTPInfo(s.httpInfoLocked()); err != nil {
		s.extraAddrs = s.extraAddrs[:len(s.extraAddrs)-1]
		s.wg.Done()
		_ = ln.Close()
		return err
	}
	go s.serveOn(ln)
	return nil
}

// HTTPExtraAddrs returns a copy of the extra HTTP listeners' bound addresses,
// empty before startHTTP runs.
func (s *Server) HTTPExtraAddrs() []string {
	s.httpMu.Lock()
	defer s.httpMu.Unlock()
	return slices.Clone(s.extraAddrs)
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

func (s *Server) dispatch(ctx context.Context, env Envelope) Reply {
	return s.dispatchPeer(ctx, env, wire.Peer{}, "")
}

func (s *Server) dispatchPeer(ctx context.Context, env Envelope, peer wire.Peer, build string) Reply {
	s.handlersMu.RLock()
	handler, ok := s.handlers[env.Op]
	s.handlersMu.RUnlock()
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
		Peer:     peer,
		Build:    build,
	})
}

// Dispatch answers a single envelope through the daemon's op table, exactly as a
// socket connection would. It exists for consumer-mounted HTTP bridges; callers
// stamp Session, ClaudePID, and Scope themselves.
func (s *Server) Dispatch(ctx context.Context, env Envelope) Reply {
	return s.dispatch(ctx, env)
}

func (s *Server) runtime() (*wire.Server, *dkdaemon.Runtime, error) {
	handlers := s.freezeHandlers()
	serverDeadlines := make(map[wire.Op]time.Duration, len(handlers))
	clientDeadlines := make(map[wire.Op]time.Duration, len(handlers))
	for op := range handlers {
		serverDeadlines[wire.Op(op)] = handleTimeout
		clientDeadlines[wire.Op(op)] = handleTimeout + time.Second
	}
	ladder, err := wire.NewLadder(serverDeadlines, clientDeadlines)
	if err != nil {
		return nil, nil, err
	}
	wireServer := &wire.Server{
		Build: s.version, MaxFrame: s.maxFrameBytes, Ladder: ladder,
		Trust: func(peer wire.Peer) error {
			if peer.UID != os.Geteuid() {
				return fmt.Errorf("%w: peer uid %d, daemon uid %d", wire.ErrUntrustedPeer, peer.UID, os.Geteuid())
			}
			return nil
		},
	}
	for op := range handlers {
		op := op
		wireServer.RegisterConcurrent(wire.Op(op), func(ctx context.Context, req wire.Request) (any, error) {
			env, err := decodeEnvelope(req.Payload)
			if err != nil {
				return nil, err
			}
			env.Op = op
			return s.dispatchPeer(ctx, env, req.Peer, req.Build), nil
		})
	}
	peer := &wire.LifecyclePeer{Config: wire.ClientConfig{
		Dial: wire.UnixDialer(s.socket), Build: s.version, MaxFrame: s.maxFrameBytes,
	}}
	runtime, err := dkdaemon.NewRuntime(dkdaemon.RuntimeConfig{
		Socket: s.socket, Build: s.version, Protocol: int(wire.ProtocolVersion),
		Peer: peer, Contract: dkdaemon.RequestDaemon, WaitMode: dkdaemon.PIDExit,
		Admission: s.wireIntake, Server: &sessionServer{owner: s, wire: wireServer},
		Workers: &serverWorkers{owner: s}, State: s.store, Resources: lifecycleResource{peer},
	})
	if err != nil {
		_ = peer.Close()
		return nil, nil, err
	}
	return wireServer, runtime, nil
}

func (s *Server) freezeHandlers() map[Op]HandlerFunc {
	s.handlersMu.Lock()
	defer s.handlersMu.Unlock()
	s.registrationsClosed = true
	handlers := make(map[Op]HandlerFunc, len(s.handlers))
	for op, handler := range s.handlers {
		handlers[op] = handler
	}
	return handlers
}

func decodeEnvelope(payload []byte) (Envelope, error) {
	var env Envelope
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&env); err != nil {
		return Envelope{}, fmt.Errorf("daemon: decode envelope: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return Envelope{}, errors.New("daemon: trailing envelope value")
		}
		return Envelope{}, fmt.Errorf("daemon: decode trailing envelope: %w", err)
	}
	return env, nil
}

type sessionServer struct {
	owner *Server
	wire  *wire.Server
}

func (s *sessionServer) Serve(
	ctx context.Context,
	listener net.Listener,
	admit, admitLifecycle func() (func(), error),
) error {
	workerCtx, cancel := context.WithCancel(ctx)
	s.owner.serveCtxMu.Lock()
	s.owner.serveCtx = workerCtx
	s.owner.serveCancel = cancel
	s.owner.serveCtxMu.Unlock()
	if s.owner.bootReconcile != nil {
		if err := s.owner.bootReconcile(workerCtx, s.owner); err != nil {
			return err
		}
	}
	if err := s.owner.reconcileDirectives(workerCtx); err != nil {
		return err
	}
	if err := s.owner.reconcileSubscriptions(workerCtx); err != nil {
		return err
	}
	if err := s.owner.startHTTP(workerCtx); err != nil {
		return err
	}
	s.owner.log.Printf("daemon %s started; socket=%s http=%s", s.owner.version, s.owner.socket, net.JoinHostPort(s.owner.bindHost(), strconv.Itoa(s.owner.httpPort)))
	err := s.wire.Serve(ctx, listener, admit, admitLifecycle)
	s.owner.log.Printf("daemon stopped")
	return err
}

func (s *sessionServer) CloseIntake() error { return s.wire.CloseIntake() }

type serverWorkers struct{ owner *Server }

func (w *serverWorkers) Close() {
	w.owner.serveCtxMu.Lock()
	w.owner.workersClosed = true
	w.owner.serveCtxMu.Unlock()
	w.owner.httpIntake.Close()
}

func (w *serverWorkers) Cancel() {
	w.owner.serveCtxMu.Lock()
	cancel := w.owner.serveCancel
	w.owner.serveCtxMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (w *serverWorkers) Wait(ctx context.Context) error {
	settleErr := w.owner.httpIntake.Settle(ctx)
	if settleErr != nil {
		if err := w.owner.httpIntake.Settle(context.WithoutCancel(ctx)); err != nil {
			settleErr = errors.Join(settleErr, err)
		}
	}
	done := make(chan struct{})
	go func() {
		w.owner.wg.Wait()
		close(done)
	}()
	<-done
	return settleErr
}

type lifecycleResource struct{ peer *wire.LifecyclePeer }

func (r lifecycleResource) Close() error { return r.peer.Close() }

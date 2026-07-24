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
	"github.com/yasyf/daemonkit/paths"
	"github.com/yasyf/daemonkit/proc"
	"github.com/yasyf/daemonkit/trust"
	"github.com/yasyf/daemonkit/wire"
	"github.com/yasyf/daemonkit/worker"
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
	appName       string
	wireBuild     string
	runtimeBuild  string
	trustPolicy   trust.TrustPolicy
	roles         Roles
	daemonRuntime *dkdaemon.Runtime
	publication   *dkdaemon.PublicationSlot[*Server]
	stateMu       sync.RWMutex
	store         *store.Store
	db            *sql.DB
	bus           *event.Bus
	activity      *Activity
	subjects      subject.Resolver
	sse           *sse.Server

	scopeResolve      func(ctx context.Context, raw string) string
	gate              GateFunc
	gateErrorReason   string
	gateObserve       func(ctx context.Context, s subject.Subject, tool ToolCall, allow bool, reason string)
	bootReconcile     func(ctx context.Context, s *Server) error
	storeSchema       store.Schema
	unsupportedSchema store.UnsupportedSchemaPolicy
	activeStatuses    []string

	agentGate           AgentGateFunc
	agentGreeting       AgentGreetingFunc
	subscribe           SubscribeFunc
	subscriptions       *subscriptionRegistry
	muteConsumer        string
	singletonSubscriber bool
	registerMu          sync.Mutex
	parks               *parkRegistry
	gateBlocks          *gateBlockCounter

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

	serveCtxMu  sync.Mutex
	serveCtx    context.Context
	serveCancel context.CancelFunc
	workers     *workerGroup
}

// New builds the daemon composition without acquiring generation-owned state.
// Consumers may register domain ops and mount routes before Serve; the store is
// opened only after the runtime owns the listener.
func New(cfg Config) (*Server, error) {
	if cfg.WireBuild != WireBuild {
		return nil, fmt.Errorf("daemon: wire build %q, want exactly %q", cfg.WireBuild, WireBuild)
	}
	if cfg.RuntimeBuild == "" {
		return nil, errors.New("daemon: runtime build is required")
	}
	if err := cfg.Roles.validate(cfg.TrustPolicy); err != nil {
		return nil, err
	}
	if err := cfg.StoreSchema.Validate(); err != nil {
		return nil, err
	}
	if err := validateBindAuth(bindHostOrDefault(cfg.BindAddr), cfg.HTTPToken, len(cfg.ExtraHTTPListeners) > 0, cfg.TrustedPeer != nil); err != nil {
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
		appName:             cfg.AppName,
		wireBuild:           cfg.WireBuild,
		runtimeBuild:        cfg.RuntimeBuild,
		trustPolicy:         cfg.TrustPolicy,
		roles:               cfg.Roles,
		bus:                 event.NewBus(),
		activity:            NewActivity(),
		scopeResolve:        scopeResolve,
		gate:                cfg.Gate,
		gateErrorReason:     cfg.GateErrorReason,
		gateObserve:         cfg.GateObserve,
		bootReconcile:       cfg.BootReconcile,
		storeSchema:         cfg.StoreSchema,
		unsupportedSchema:   cfg.UnsupportedSchema,
		activeStatuses:      slices.Clone(cfg.ActiveStatuses),
		agentGate:           cfg.AgentGate,
		agentGreeting:       cfg.AgentGreeting,
		subscribe:           cfg.Subscribe,
		subscriptions:       newSubscriptionRegistry(),
		muteConsumer:        cfg.MuteConsumer,
		singletonSubscriber: cfg.SingletonSubscriber,
		parks:               newParkRegistry(),
		gateBlocks:          newGateBlockCounter(),
		paths:               cfg.Paths,
		socket:              cfg.Paths.SocketPath(),
		maxFrameBytes:       frameBytes,
		log:                 log.New(os.Stderr, "["+cfg.AppName+"] ", log.LstdFlags),
		handlers:            make(map[Op]HandlerFunc),
		fixedPort:           cfg.FixedPort,
		bindAddr:            cfg.BindAddr,
		httpToken:           cfg.HTTPToken,
		trustedPeer:         cfg.TrustedPeer,
		trustedOrigin:       cfg.TrustedOrigin,
		onHTTPStart:         cfg.OnHTTPStart,
		extraListeners:      cfg.ExtraHTTPListeners,
		publicHandler:       cfg.PublicHandler,
		repoLocks:           make(map[string]*sync.Mutex),
		workers:             newWorkerGroup(),
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
		Admit:             s.workers.Admit,
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

// DB exposes the activated runtime's underlying connection.
func (s *Server) DB() *sql.DB {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	return s.db
}

// Background runs fn as daemon-lifecycle work. fn must return when its context
// is cancelled; daemonkit's shutdown deadline bounds settlement before state
// closure. Call it from a handler — before Serve there is no serve context.
func (s *Server) Background(fn func(context.Context)) {
	s.serveCtxMu.Lock()
	ctx := s.serveCtx
	if ctx == nil {
		s.serveCtxMu.Unlock()
		panic("daemon: Background called before Serve")
	}
	started := s.workers.Start(func() { fn(ctx) })
	s.serveCtxMu.Unlock()
	if !started {
		panic("daemon: Background called while draining")
	}
}

// Serve runs the exact daemonkit lifecycle and persistent control session.
func (s *Server) Serve(parent context.Context) error {
	_, runtime, err := s.runtime()
	if err != nil {
		_ = s.closeState()
		return err
	}
	activation, err := runtime.Begin(parent)
	if err != nil {
		return errors.Join(err, runtime.Wait(context.Background()), s.closeState())
	}
	settlement, err := activation.ClaimProductSettlement()
	if err != nil {
		_ = activation.Fail(err)
		return errors.Join(err, runtime.Wait(context.Background()), s.closeState())
	}
	settled := make(chan error, 1)
	go func() {
		<-activation.Context().Done()
		settled <- s.settleProduct(settlement)
	}()
	if err := s.activateState(activation.Context()); err != nil {
		_ = activation.Fail(err)
		return errors.Join(err, runtime.Wait(context.Background()), <-settled)
	}
	if err := s.activateServing(activation.Context()); err != nil {
		_ = activation.Fail(err)
		return errors.Join(err, runtime.Wait(context.Background()), <-settled)
	}
	publication, err := s.publication.Stage(activation, s)
	if err != nil {
		_ = activation.Fail(err)
		return errors.Join(err, runtime.Wait(context.Background()), <-settled)
	}
	if err := activation.CommitReady(publication); err != nil {
		_ = activation.Fail(err)
		return errors.Join(err, runtime.Wait(context.Background()), <-settled)
	}
	stopContext := context.AfterFunc(parent, func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = runtime.Shutdown(shutdownCtx)
	})
	defer stopContext()
	err = errors.Join(runtime.Wait(context.Background()), <-settled)
	s.log.Printf("daemon stopped")
	if parent.Err() != nil && errors.Is(err, parent.Err()) {
		return nil
	}
	return err
}

func (s *Server) settleProduct(settlement dkdaemon.ProductSettlement) error {
	s.serveCtxMu.Lock()
	cancel := s.serveCancel
	s.serveCtxMu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.workers.Close()
	settleCtx, settleCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer settleCancel()
	return errors.Join(s.workers.Wait(settleCtx), s.closeState(), settlement.Complete())
}

func (s *Server) activateState(startup context.Context) error {
	st, err := store.Open(startup, store.Path(s.paths), s.storeSchema, store.WithUnsupportedSchema(s.unsupportedSchema))
	if err != nil {
		return err
	}
	s.stateMu.Lock()
	s.store = st
	s.db = st.DB()
	s.subjects = subject.Resolver{
		Store: store.NewSubjectStore(s.db),
		Policy: subject.Policy{Active: func(sub subject.Subject) bool {
			return slices.Contains(s.activeStatuses, sub.Status)
		}},
	}
	s.stateMu.Unlock()
	return nil
}

func (s *Server) activateServing(lifetime context.Context) error {
	workerCtx, cancel := context.WithCancel(lifetime)
	s.serveCtxMu.Lock()
	s.serveCtx = workerCtx
	s.serveCancel = cancel
	s.serveCtxMu.Unlock()
	if err := workerCtx.Err(); err != nil {
		return err
	}
	if s.bootReconcile != nil {
		if err := s.bootReconcile(workerCtx, s); err != nil {
			return err
		}
	}
	if err := s.reconcileDirectives(workerCtx); err != nil {
		return err
	}
	if err := s.reconcileSubscriptions(workerCtx); err != nil {
		return err
	}
	if err := s.startHTTP(workerCtx); err != nil {
		return err
	}
	s.log.Printf("daemon %s started; socket=%s http=%s", s.runtimeBuild, s.socket, net.JoinHostPort(s.bindHost(), strconv.Itoa(s.httpPort)))
	return nil
}

func (s *Server) closeState() error {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.store == nil {
		return nil
	}
	err := s.store.Close()
	s.store = nil
	s.db = nil
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
		if !s.workers.Start(func() { s.onHTTPStart(ctx, s.httpPort) }) {
			closeAll()
			return errors.New("daemon: HTTP startup while draining")
		}
	}
	for _, l := range listeners {
		listener := l
		if !s.workers.Start(func() { s.serveOn(listener) }) {
			closeAll()
			return errors.New("daemon: HTTP listener startup while draining")
		}
	}
	if !s.workers.Start(func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}) {
		closeAll()
		return errors.New("daemon: HTTP shutdown worker startup while draining")
	}
	return nil
}

// serveOn runs the shared HTTP server on ln until a graceful Shutdown closes it.
func (s *Server) serveOn(ln net.Listener) {
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
	s.extraAddrs = append(s.extraAddrs, ln.Addr().String())
	if err := s.writeHTTPInfo(s.httpInfoLocked()); err != nil {
		s.extraAddrs = s.extraAddrs[:len(s.extraAddrs)-1]
		_ = ln.Close()
		return err
	}
	if !s.workers.Start(func() { s.serveOn(ln) }) {
		s.extraAddrs = s.extraAddrs[:len(s.extraAddrs)-1]
		_ = s.writeHTTPInfo(s.httpInfoLocked())
		_ = ln.Close()
		return errors.New("daemon: AddHTTPListener while draining")
	}
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

func (s *Server) dispatchPeer(ctx context.Context, env Envelope, peer wire.Peer, wireBuild string) Reply {
	s.handlersMu.RLock()
	handler, ok := s.handlers[env.Op]
	s.handlersMu.RUnlock()
	if !ok {
		return errReply("unknown op: " + string(env.Op))
	}
	scope := s.scopeResolve(ctx, env.Scope)
	return handler(HandlerCtx{
		Ctx:       ctx,
		Env:       env,
		Window:    subject.Window{Session: env.Session, ClaudePID: env.ClaudePID},
		Scope:     scope,
		Subjects:  s.subjects,
		DB:        s.db,
		Append:    s.Append,
		HTTPPort:  s.httpPort,
		Activity:  s.activity,
		RepoLock:  s.repoLock(scope),
		Peer:      peer,
		WireBuild: wireBuild,
	})
}

// Dispatch answers a single envelope through the daemon's op table, exactly as a
// socket connection would. It exists for consumer-mounted HTTP bridges; callers
// stamp Session, ClaudePID, and Scope themselves.
func (s *Server) Dispatch(ctx context.Context, env Envelope) Reply {
	published, release, err := s.publication.Acquire()
	if err != nil {
		return errReply(err.Error())
	}
	defer release()
	return published.dispatch(ctx, env)
}

func (s *Server) runtime() (*wire.Server, *dkdaemon.Runtime, error) {
	handlers := s.freezeHandlers()
	serverDeadlines := make(map[wire.Op]time.Duration, len(handlers)+1)
	clientDeadlines := make(map[wire.Op]time.Duration, len(handlers)+1)
	for op := range handlers {
		serverDeadlines[wire.Op(op)] = handleTimeout
		clientDeadlines[wire.Op(op)] = handleTimeout + time.Second
	}
	serverDeadlines[wire.Op(OpRuntimeHealth)] = time.Second
	clientDeadlines[wire.Op(OpRuntimeHealth)] = 2 * time.Second
	ladder, err := wire.NewLadder(serverDeadlines, clientDeadlines)
	if err != nil {
		return nil, nil, err
	}
	wireServer := &wire.Server{
		WireBuild: s.wireBuild, MaxFrame: s.maxFrameBytes, Ladder: ladder,
	}
	for op := range handlers {
		op := op
		wireServer.Register(wire.HandlerSpec{Op: wire.Op(op), Concurrent: true, Handler: func(ctx context.Context, req wire.Request) (any, error) {
			published, err := s.publication.Value(req.Publication)
			if err != nil {
				return nil, err
			}
			env, err := decodeEnvelope(req.Payload)
			if err != nil {
				return nil, err
			}
			env.Op = op
			return published.dispatchPeer(ctx, env, req.Peer, req.WireBuild), nil
		}})
	}
	generation, err := proc.ProcessGeneration()
	if err != nil {
		return nil, nil, fmt.Errorf("daemon: process generation: %w", err)
	}
	reaper := &proc.Reaper{Store: processStore(s.paths), Generation: generation}
	children, err := proc.NewManager(64, reaper)
	if err != nil {
		return nil, nil, err
	}
	disposable, err := worker.NewPool(worker.Config{
		Capacity: 1, QueueCapacity: 0, MaxTotalRun: handleTimeout,
		MaxStdinBytes: 1, MaxStdoutBytes: 1, MaxStderrBytes: 1,
	}, reaper)
	if err != nil {
		return nil, nil, err
	}
	runtime, err := wire.NewRuntime(wire.RuntimeConfig{
		Socket: s.socket, RuntimeBuild: s.runtimeBuild, RuntimeProtocol: int(wire.ProtocolVersion),
		Wire: wireServer, TrustPolicy: s.trustPolicy,
		StopControlStore: stopProcessStore(s.paths),
		Observations: []wire.ObservationRoute{{
			Op: wire.Op(OpRuntimeHealth), MaxResponseBytes: min(s.maxFrameBytes, 1024),
			Handler: s.observeRuntimeHealth,
		}},
		Workers: disposable, Children: children, ShutdownTimeout: 30 * time.Second,
	})
	if err != nil {
		return nil, nil, err
	}
	s.daemonRuntime = runtime
	s.publication = dkdaemon.NewPublicationSlot[*Server](runtime)
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

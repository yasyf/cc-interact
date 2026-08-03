package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-interact/daemon"
	"github.com/yasyf/cc-interact/internal/testhome"
	"github.com/yasyf/daemonkit"
	"github.com/yasyf/daemonkit/paths"
)

const testClaudePID = 4242

// fakeLabel is shared with the guard-edit re-exec child, which rebuilds the
// spec from this label plus the inherited DAEMONKIT_HOME.
const fakeLabel = "cci-cmd-fake"

// probeOp is the readiness probe the fake daemon answers without recording.
const probeOp daemon.Op = "probe"

func testSpec(label string) daemonkit.Daemon {
	return daemon.Spec(daemonkit.Daemon{
		Label: daemonkit.Label(label),
		Trust: daemonkit.Trust{Serving: daemonkit.ServingSameUser()},
	})
}

// recorder collects every envelope a fake daemon receives.
type recorder struct {
	mu   sync.Mutex
	envs []daemon.Envelope
}

func (r *recorder) record(e daemon.Envelope) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.envs = append(r.envs, e)
}

func (r *recorder) last() daemon.Envelope {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.envs[len(r.envs)-1]
}

// fakeProduct answers every op with reply, recording each envelope except the
// readiness probe.
type fakeProduct struct {
	rec   *recorder
	reply func(daemon.Envelope) daemon.Reply
}

func (p fakeProduct) Handle(_ context.Context, req daemonkit.Request) (daemonkit.Reply, error) {
	var env daemon.Envelope
	if err := json.Unmarshal(req.Body, &env); err != nil {
		return daemonkit.Reply{}, err
	}
	env.Op = daemon.Op(req.Op)
	if env.Op == probeOp {
		return daemonkit.Reply{Body: []byte(`{"ok":true}`)}, nil
	}
	p.rec.record(env)
	body, err := json.Marshal(p.reply(env))
	if err != nil {
		return daemonkit.Reply{}, err
	}
	return daemonkit.Reply{Body: body}, nil
}

func (fakeProduct) Drain(daemonkit.Budget) error { return nil }

func (fakeProduct) Close(daemonkit.Budget) error { return nil }

// fakeDaemon serves the label's socket in-process, recording each envelope and
// replying via reply. It returns the shared spec and the recorder.
func fakeDaemon(t *testing.T, reply func(daemon.Envelope) daemon.Reply) (daemonkit.Daemon, *recorder) {
	t.Helper()
	shortHome(t)
	rec := &recorder{}
	spec := testSpec(fakeLabel)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := daemonkit.Serve(ctx, spec, func(daemonkit.Ctx) (daemonkit.Product, error) {
			return fakeProduct{rec: rec, reply: reply}, nil
		})
		done <- err
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("fake daemon Serve: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("fake daemon did not stop")
		}
	})
	awaitReady(t, spec, done)
	return spec, rec
}

// shortHome points HOME and DAEMONKIT_HOME at a short-prefix temp dir so the
// daemon's unix socket path stays under the sun_path length limit.
func shortHome(t *testing.T) {
	t.Helper()
	home, err := os.MkdirTemp("/tmp", "cci-cmd-test-")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	testhome.Set(t, home)
}

// awaitReady polls the business lane until the daemon dispatches, so a test
// never races the bind.
func awaitReady(t *testing.T, spec daemonkit.Daemon, served <-chan error) {
	t.Helper()
	client, err := daemon.NewClient(spec)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer func() { _ = client.Close() }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		probeCtx, probeCancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		_, probeErr := client.Do(probeCtx, daemon.Envelope{Op: probeOp})
		probeCancel()
		if probeErr == nil {
			return
		}
		select {
		case err := <-served:
			t.Fatalf("serve daemon: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon did not become ready: %v", probeErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func testDeps(spec daemonkit.Daemon) Deps {
	return Deps{
		Paths:   paths.Paths{App: ".cc-interact-test"},
		Version: "9.9.9",
		NewClient: func(context.Context) (*daemon.Client, error) {
			return daemon.NewClient(spec)
		},
		EnsureCurrent:          func(context.Context) error { return nil },
		EnsureCurrentIfRunning: func(context.Context) error { return nil },
		Stop:                   func(context.Context) error { return nil },
		ClaudePID:              func() int { return testClaudePID },
		TerminalEvent:          func(t string) bool { return t == "submit" },
	}
}

// absentSpec is a spec whose socket provably has no listener.
func absentSpec(t *testing.T) daemonkit.Daemon {
	t.Helper()
	shortHome(t)
	return testSpec("cci-cmd-absent")
}

func liveDaemon(t *testing.T, maxFrame daemonkit.Bytes) daemonkit.Daemon {
	t.Helper()
	shortHome(t)
	spec := daemon.Spec(daemonkit.Daemon{
		Label:    "cci-cmd-live",
		MaxFrame: maxFrame,
		Trust:    daemonkit.Trust{Serving: daemonkit.ServingSameUser()},
	})
	s, err := daemon.New(daemon.Config{
		AppName:        "cc-interact-test",
		Paths:          paths.Paths{App: ".cc-interact-test"},
		Daemon:         spec,
		RuntimeBuild:   "9.9.9",
		ActiveStatuses: []string{"open"},
	})
	if err != nil {
		t.Fatalf("new daemon: %v", err)
	}
	if err := (paths.Paths{App: ".cc-interact-test"}).EnsureStateDir(); err != nil {
		t.Fatalf("ensure state dir: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- s.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-served:
			if err != nil {
				t.Errorf("serve daemon: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("daemon did not stop")
		}
	})

	client, err := daemon.NewClient(spec)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer func() { _ = client.Close() }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		probeCtx, probeCancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		reply, probeErr := client.Do(probeCtx, daemon.Envelope{Op: daemon.OpStatus})
		probeCancel()
		if probeErr == nil && reply.DaemonVersion == "9.9.9" {
			return spec
		}
		select {
		case err := <-served:
			t.Fatalf("serve daemon: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon did not start: %v", probeErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestGuardEditAllowSendsEnvelope proves the allow path returns without exiting
// and stamps the window pid, scope, and the {tool_name, tool_input} body the
// daemon's guard-edit handler expects.
func TestGuardEditAllowSendsEnvelope(t *testing.T) {
	spec, rec := fakeDaemon(t, func(daemon.Envelope) daemon.Reply {
		return daemon.Reply{OK: true, Allow: true}
	})
	cmd := GuardEditCmd(testDeps(spec))
	cmd.SetIn(strings.NewReader(`{"session_id":"s1","cwd":"/repo","tool_name":"Edit","tool_input":{"path":"a.go"}}`))
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("guard-edit allow: %v", err)
	}
	env := rec.last()
	if env.Op != daemon.OpGuardEdit {
		t.Fatalf("op = %q, want %q", env.Op, daemon.OpGuardEdit)
	}
	if env.Session != "s1" || env.Scope != "/repo" || env.ClaudePID != testClaudePID {
		t.Fatalf("envelope identity = %+v", env)
	}
	var body struct {
		ToolName  string          `json:"tool_name"`
		ToolInput json.RawMessage `json:"tool_input"`
	}
	if err := json.Unmarshal(env.Body, &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if body.ToolName != "Edit" || string(body.ToolInput) != `{"path":"a.go"}` {
		t.Fatalf("body = %+v", body)
	}
}

// TestGuardEditDaemonDownAllows proves a missing daemon fails open: no error,
// no exit.
func TestGuardEditDaemonDownAllows(t *testing.T) {
	cmd := GuardEditCmd(testDeps(absentSpec(t)))
	cmd.SetIn(strings.NewReader(`{"session_id":"s1","cwd":"/repo","tool_name":"Edit"}`))
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("guard-edit daemon-down: %v", err)
	}
}

// TestGuardEditOversizeLogsAndAllows pins fail-open visibility at a lowered cap.
func TestGuardEditOversizeLogsAndAllows(t *testing.T) {
	spec := liveDaemon(t, 8<<10)
	input := fmt.Sprintf(`{"session_id":"s1","cwd":"/repo","tool_name":"Write","tool_input":{"content":%q}}`, strings.Repeat("x", 4096))
	cmd := GuardEditCmd(testDeps(spec))
	cmd.SetIn(strings.NewReader(input))
	cmd.SetOut(&bytes.Buffer{})
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("guard-edit oversize: %v", err)
	}

	var in hookInput
	if err := json.Unmarshal([]byte(input), &in); err != nil {
		t.Fatalf("hook input: %v", err)
	}
	body, err := json.Marshal(guardEditBody{ToolName: in.ToolName, ToolInput: in.ToolInput})
	if err != nil {
		t.Fatalf("guard-edit body: %v", err)
	}
	frame, err := json.Marshal(daemon.Envelope{
		Op: daemon.OpGuardEdit, Session: in.SessionID,
		ClaudePID: testClaudePID, Scope: in.Cwd, Body: body,
	})
	if err != nil {
		t.Fatalf("guard-edit frame: %v", err)
	}
	want := fmt.Sprintf("guard-edit: frame-too-large: request frame is %d bytes; allowing edit", len(frame))
	if got := strings.TrimSpace(stderr.String()); got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
}

// TestGuardEditBlockExits2 runs guard-edit in a child process so the os.Exit(2)
// block signal is observable, and asserts the reason reaches stderr.
func TestGuardEditBlockExits2(t *testing.T) {
	if os.Getenv("GUARD_EDIT_HELPER") == "1" {
		cmd := GuardEditCmd(testDeps(testSpec(fakeLabel)))
		cmd.SetIn(strings.NewReader(`{"session_id":"s1","cwd":"/repo","tool_name":"Edit"}`))
		_ = cmd.ExecuteContext(context.Background())
		return
	}
	_, _ = fakeDaemon(t, func(daemon.Envelope) daemon.Reply {
		return daemon.Reply{OK: true, Allow: false, Reason: "review open: edits blocked"}
	})
	child := exec.Command(os.Args[0], "-test.run=TestGuardEditBlockExits2")
	child.Env = append(os.Environ(), "GUARD_EDIT_HELPER=1")
	var stderr bytes.Buffer
	child.Stderr = &stderr
	err := child.Run()
	var exit *exec.ExitError
	if !asExitError(err, &exit) {
		t.Fatalf("expected exit error, got %v", err)
	}
	if exit.ExitCode() != 2 {
		t.Fatalf("exit code = %d, want 2", exit.ExitCode())
	}
	if !strings.Contains(stderr.String(), "review open: edits blocked") {
		t.Fatalf("stderr = %q, want the block reason", stderr.String())
	}
}

func asExitError(err error, target **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError)
	if ok {
		*target = e
	}
	return ok
}

// TestChannelAckErrorPropagates proves a not-OK reply surfaces as a command error.
func TestChannelAckErrorPropagates(t *testing.T) {
	spec, rec := fakeDaemon(t, func(daemon.Envelope) daemon.Reply {
		return daemon.Reply{OK: false, Error: "no window"}
	})
	cmd := ChannelAckCmd(testDeps(spec))
	cmd.SetArgs([]string{"--session", "s1", "--cwd", "/repo"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	err := cmd.ExecuteContext(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no window") {
		t.Fatalf("err = %v, want 'no window'", err)
	}
	if got := rec.last(); got.Op != daemon.OpChannelAck || got.ClaudePID != testClaudePID {
		t.Fatalf("envelope = %+v", got)
	}
}

// TestStatusReportsSubject proves status renders the daemon version, port, and
// the bound subject.
func TestStatusReportsSubject(t *testing.T) {
	spec, _ := fakeDaemon(t, func(daemon.Envelope) daemon.Reply {
		return daemon.Reply{
			OK: true, DaemonVersion: "1.2.3", HTTPPort: 5678, SubjectID: "sub-9", Status: "open",
			Body: json.RawMessage(`{"consumer_connected":true,"consumers":{"watch":1,"watch-123":2,"watchdog":9,"channel":1}}`),
		}
	})
	cmd := StatusCmd(testDeps(spec))
	var out bytes.Buffer
	cmd.SetArgs([]string{"--session", "s1", "--cwd", "/repo"})
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("status: %v", err)
	}
	want := "daemon: running (1.2.3)\nhttp:   127.0.0.1:5678\nsubject: sub-9 (open)\nwatchers: 3\n"
	if got := out.String(); got != want {
		t.Fatalf("status output = %q, want %q", got, want)
	}
}

// TestStatusNotRunning proves a stopped daemon is reported, not spawned.
func TestStatusNotRunning(t *testing.T) {
	deps := testDeps(absentSpec(t))
	deps.EnsureCurrentIfRunning = func(context.Context) error { return daemon.ErrNoPeer }
	cmd := StatusCmd(deps)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(out.String(), "daemon: not running") {
		t.Fatalf("status output = %q", out.String())
	}
}

// TestStatusLegacyDaemon proves a pre-0.21 daemon reads as running-but-stranded
// rather than as the absence its silent endpoint would otherwise look like, and
// that status names the verb that retires it.
func TestStatusLegacyDaemon(t *testing.T) {
	deps := testDeps(absentSpec(t))
	deps.EnsureCurrentIfRunning = func(context.Context) error {
		return fmt.Errorf("%w on /tmp/legacy.sock", daemon.ErrLegacyDaemon)
	}
	root := &cobra.Command{Use: "cci"}
	root.AddCommand(StatusCmd(deps))
	var out bytes.Buffer
	root.SetArgs([]string{"status"})
	root.SetOut(&out)
	root.SetErr(&bytes.Buffer{})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("status: %v", err)
	}
	got := out.String()
	if strings.Contains(got, "not running") {
		t.Fatalf("status output = %q, want a pre-0.21 daemon reported as running", got)
	}
	if !strings.Contains(got, "pre-0.21") || !strings.Contains(got, `"cci stop"`) {
		t.Fatalf("status output = %q, want the pre-0.21 state and the retiring verb", got)
	}
}

// TestStopIsUniformOverAnAlreadyStoppedDaemon pins the postcondition daemonkit
// now guarantees: stop reports the same success either way, because an absent
// daemon can still have left a LaunchAgent that this call took down.
func TestStopIsUniformOverAnAlreadyStoppedDaemon(t *testing.T) {
	deps := testDeps(testSpec("cci-cmd-unused"))
	called := 0
	deps.Stop = func(context.Context) error { called++; return nil }
	command := StopCmd(deps)
	var out bytes.Buffer
	command.SetOut(&out)
	if err := command.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if called != 1 || out.String() != "daemon: stopped\n" {
		t.Fatalf("stop calls=%d output=%q", called, out.String())
	}
}

// TestWatchStreamsUntilTerminal drives the full watch path: resolve the subject
// against a fake daemon, stream events from a fake SSE plane, print each, and
// stop on the terminal event.
func TestWatchStreamsUntilTerminal(t *testing.T) {
	testhome.Isolate(t)
	wantConsumer := fmt.Sprintf("%s-%d", watchConsumer, os.Getpid())
	sse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("consumer"); got != wantConsumer {
			t.Errorf("consumer = %q, want %q", got, wantConsumer)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "id: 1\ndata: {\"type\":\"comment.created\"}\n\n")
		fmt.Fprint(w, "id: 2\ndata: {\"type\":\"submit\"}\n\n")
	}))
	t.Cleanup(sse.Close)
	ssePort := mustPort(t, sse)

	spec, _ := fakeDaemon(t, func(env daemon.Envelope) daemon.Reply {
		return daemon.Reply{OK: true, SubjectID: "sub-1", HTTPPort: ssePort}
	})
	d := testDeps(spec)

	cmd := WatchCmd(d)
	var out bytes.Buffer
	cmd.SetArgs([]string{"--session", "s1", "--cwd", "/repo"})
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("watch: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "comment.created") || !strings.Contains(lines[1], "submit") {
		t.Fatalf("watch output = %q", out.String())
	}
}

// TestWatchOnceExitsAfterFirstEvent proves --once stops after the first emitted
// event (not the terminal one) and advances the cursor, so a second --once run
// resumes past the event it already delivered.
func TestWatchOnceExitsAfterFirstEvent(t *testing.T) {
	testhome.Isolate(t)
	sse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		switch r.Header.Get("Last-Event-ID") {
		case "": // first run resumes from nothing: a non-terminal event then the terminal one
			fmt.Fprint(w, "id: 1\ndata: {\"type\":\"comment.created\"}\n\n")
			fmt.Fprint(w, "id: 2\ndata: {\"type\":\"submit\"}\n\n")
		case "1": // second run resumes from the cursor the first --once persisted
			fmt.Fprint(w, "id: 2\ndata: {\"type\":\"submit\"}\n\n")
		default:
			t.Errorf("Last-Event-ID = %q, want \"\" or \"1\"", r.Header.Get("Last-Event-ID"))
		}
	}))
	t.Cleanup(sse.Close)
	ssePort := mustPort(t, sse)

	spec, _ := fakeDaemon(t, func(daemon.Envelope) daemon.Reply {
		return daemon.Reply{OK: true, SubjectID: "sub-1", HTTPPort: ssePort}
	})
	d := testDeps(spec)

	run := func() string {
		t.Helper()
		cmd := WatchCmd(d)
		var out bytes.Buffer
		cmd.SetArgs([]string{"--once", "--session", "s1", "--cwd", "/repo"})
		cmd.SetOut(&out)
		cmd.SetErr(&bytes.Buffer{})
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := cmd.ExecuteContext(ctx); err != nil {
			t.Fatalf("watch --once: %v", err)
		}
		return strings.TrimSpace(out.String())
	}

	first := run()
	if lines := strings.Split(first, "\n"); len(lines) != 1 || !strings.Contains(first, "comment.created") {
		t.Fatalf("first --once output = %q, want exactly the comment.created line", first)
	}
	if strings.Contains(first, "submit") {
		t.Fatalf("first --once leaked the terminal event: %q", first)
	}

	second := run()
	if lines := strings.Split(second, "\n"); len(lines) != 1 || !strings.Contains(second, "submit") {
		t.Fatalf("second --once output = %q, want exactly the resumed submit line", second)
	}
}

func mustPort(t *testing.T, srv *httptest.Server) int {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	return port
}

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yasyf/cc-interact/daemon"
	"github.com/yasyf/cc-interact/paths"
)

const testClaudePID = 4242

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

// fakeDaemon serves the control socket, recording each envelope and replying via
// reply. It returns the socket path and the recorder.
func fakeDaemon(t *testing.T, reply func(daemon.Envelope) daemon.Reply) (string, *recorder) {
	t.Helper()
	socket := filepath.Join(shortTempDir(t), "d.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	rec := &recorder{}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				var env daemon.Envelope
				if err := json.NewDecoder(conn).Decode(&env); err != nil {
					return
				}
				rec.record(env)
				_ = json.NewEncoder(conn).Encode(reply(env))
			}()
		}
	}()
	return socket, rec
}

// shortTempDir returns a temp dir with a short path; the test-name-based
// t.TempDir() blows past the ~104-byte unix-socket path limit on macOS.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ccx")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func testDeps(socket string) Deps {
	return Deps{
		Paths:                  paths.Paths{App: ".cc-interact-test"},
		Version:                "9.9.9",
		NewClient:              func() *daemon.Client { return daemon.NewClient(socket) },
		EnsureCurrent:          func(context.Context) error { return nil },
		EnsureCurrentIfRunning: func() error { return nil },
		ClaudePID:              func() int { return testClaudePID },
		TerminalEvent:          func(t string) bool { return t == "submit" },
	}
}

func liveDaemon(t *testing.T, maxFrameBytes int) string {
	t.Helper()
	home, err := os.MkdirTemp("/tmp", "cci-cmd-test-")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)

	p := paths.Paths{App: ".cc-interact-test"}
	s, err := daemon.New(daemon.Config{
		AppName:        "cc-interact-test",
		Paths:          p,
		Version:        "9.9.9",
		ActiveStatuses: []string{"open"},
		MaxFrameBytes:  maxFrameBytes,
	})
	if err != nil {
		t.Fatalf("new daemon: %v", err)
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

	client := daemon.NewClient(p.SocketPath())
	deadline := time.Now().Add(5 * time.Second)
	for {
		if reply, err := client.Health(); err == nil && reply.OK {
			return p.SocketPath()
		}
		select {
		case err := <-served:
			t.Fatalf("serve daemon: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("daemon did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestGuardEditAllowSendsEnvelope proves the allow path returns without exiting
// and stamps the window pid, scope, and the {tool_name, tool_input} body the
// daemon's guard-edit handler expects.
func TestGuardEditAllowSendsEnvelope(t *testing.T) {
	socket, rec := fakeDaemon(t, func(daemon.Envelope) daemon.Reply {
		return daemon.Reply{OK: true, Allow: true}
	})
	cmd := GuardEditCmd(testDeps(socket))
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
	cmd := GuardEditCmd(testDeps(filepath.Join(t.TempDir(), "absent.sock")))
	cmd.SetIn(strings.NewReader(`{"session_id":"s1","cwd":"/repo","tool_name":"Edit"}`))
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("guard-edit daemon-down: %v", err)
	}
}

// TestGuardEditOversizeLogsAndAllows pins fail-open visibility at a lowered cap.
func TestGuardEditOversizeLogsAndAllows(t *testing.T) {
	const maxFrameBytes = 256
	socket := liveDaemon(t, maxFrameBytes)
	input := fmt.Sprintf(`{"session_id":"s1","cwd":"/repo","tool_name":"Write","tool_input":{"content":%q}}`, strings.Repeat("x", 512))
	cmd := GuardEditCmd(testDeps(socket))
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
		Proto: daemon.ProtocolVersion, Op: daemon.OpGuardEdit, Session: in.SessionID,
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
		socket := os.Getenv("GUARD_EDIT_SOCKET")
		cmd := GuardEditCmd(testDeps(socket))
		cmd.SetIn(strings.NewReader(`{"session_id":"s1","cwd":"/repo","tool_name":"Edit"}`))
		_ = cmd.ExecuteContext(context.Background())
		return
	}
	socket, _ := fakeDaemon(t, func(daemon.Envelope) daemon.Reply {
		return daemon.Reply{OK: true, Allow: false, Reason: "review open: edits blocked"}
	})
	child := exec.Command(os.Args[0], "-test.run=TestGuardEditBlockExits2")
	child.Env = append(os.Environ(), "GUARD_EDIT_HELPER=1", "GUARD_EDIT_SOCKET="+socket)
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
	socket, rec := fakeDaemon(t, func(daemon.Envelope) daemon.Reply {
		return daemon.Reply{OK: false, Error: "no window"}
	})
	cmd := ChannelAckCmd(testDeps(socket))
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
	socket, _ := fakeDaemon(t, func(daemon.Envelope) daemon.Reply {
		return daemon.Reply{OK: true, DaemonVersion: "1.2.3", HTTPPort: 5678, SubjectID: "sub-9", Status: "open"}
	})
	cmd := StatusCmd(testDeps(socket))
	var out bytes.Buffer
	cmd.SetArgs([]string{"--session", "s1", "--cwd", "/repo"})
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("status: %v", err)
	}
	got := out.String()
	for _, want := range []string{"daemon: running (1.2.3)", "127.0.0.1:5678", "subject: sub-9 (open)"} {
		if !strings.Contains(got, want) {
			t.Fatalf("status output %q missing %q", got, want)
		}
	}
}

// TestStatusNotRunning proves a stopped daemon is reported, not spawned.
func TestStatusNotRunning(t *testing.T) {
	cmd := StatusCmd(testDeps(filepath.Join(t.TempDir(), "absent.sock")))
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

// TestWatchStreamsUntilTerminal drives the full watch path: resolve the subject
// against a fake daemon, stream events from a fake SSE plane, print each, and
// stop on the terminal event.
func TestWatchStreamsUntilTerminal(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	sse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("consumer"); got != watchConsumer {
			t.Errorf("consumer = %q, want %q", got, watchConsumer)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "id: 1\ndata: {\"type\":\"comment.created\"}\n\n")
		fmt.Fprint(w, "id: 2\ndata: {\"type\":\"submit\"}\n\n")
	}))
	t.Cleanup(sse.Close)
	ssePort := mustPort(t, sse)

	socket, _ := fakeDaemon(t, func(env daemon.Envelope) daemon.Reply {
		return daemon.Reply{OK: true, SubjectID: "sub-1", HTTPPort: ssePort}
	})
	d := testDeps(socket)
	if err := d.Paths.EnsureSubjectDir("sub-1"); err != nil {
		t.Fatalf("subject dir: %v", err)
	}

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
	t.Setenv("HOME", t.TempDir())
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

	socket, _ := fakeDaemon(t, func(daemon.Envelope) daemon.Reply {
		return daemon.Reply{OK: true, SubjectID: "sub-1", HTTPPort: ssePort}
	})
	d := testDeps(socket)
	if err := d.Paths.EnsureSubjectDir("sub-1"); err != nil {
		t.Fatalf("subject dir: %v", err)
	}

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

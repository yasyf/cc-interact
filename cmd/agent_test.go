package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-interact/agent"
	"github.com/yasyf/cc-interact/daemon"
)

func executeAgentHook(t *testing.T, command *cobra.Command, input string) (string, string) {
	t.Helper()
	root := &cobra.Command{Use: "echo-test", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(command)
	root.SetArgs([]string{command.Name()})
	root.SetIn(strings.NewReader(input))
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("execute %s: %v", command.Name(), err)
	}
	return stdout.String(), stderr.String()
}

func envelopeCount(rec *recorder) int {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return len(rec.envs)
}

func TestAgentStartSendsEnvelope(t *testing.T) {
	socket, rec := fakeDaemon(t, func(daemon.Envelope) daemon.Reply { return daemon.Reply{OK: true} })
	d := testDeps(socket)
	ensured := 0
	d.EnsureCurrentIfRunning = func() error { ensured++; return nil }
	stdout, stderr := executeAgentHook(t, AgentStartCmd(d), `{
		"session_id":"s1","cwd":"/repo","agent_id":"a1","agent_type":"Explore",
		"transcript_path":"/tmp/projects/repo/session.jsonl"
	}`)
	if stdout != "" || stderr != "" {
		t.Fatalf("output = stdout %q stderr %q, want silent", stdout, stderr)
	}
	if ensured != 1 {
		t.Fatalf("ensure calls = %d, want 1", ensured)
	}
	env := rec.last()
	if env.Op != daemon.OpAgentStart || env.Session != "s1" || env.Scope != "/repo" || env.ClaudePID != testClaudePID {
		t.Fatalf("envelope = %+v", env)
	}
	var body agentStartBody
	if err := json.Unmarshal(env.Body, &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if body != (agentStartBody{
		AgentID: "a1", AgentType: "Explore", SessionID: "s1",
		TranscriptPath: "/tmp/projects/repo/session/subagents/agent-a1.jsonl",
	}) {
		t.Fatalf("body = %+v", body)
	}
}

func TestAgentStartWithoutSessionTranscriptLeavesAgentPathEmpty(t *testing.T) {
	socket, rec := fakeDaemon(t, func(daemon.Envelope) daemon.Reply { return daemon.Reply{OK: true} })
	stdout, stderr := executeAgentHook(t, AgentStartCmd(testDeps(socket)), `{
		"session_id":"s1","cwd":"/repo","agent_id":"a1","agent_type":"Explore"
	}`)
	if stdout != "" || stderr != "" {
		t.Fatalf("output = stdout %q stderr %q, want silent", stdout, stderr)
	}
	var body agentStartBody
	if err := json.Unmarshal(rec.last().Body, &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if body.TranscriptPath != "" {
		t.Fatalf("transcript path = %q, want empty", body.TranscriptPath)
	}
}

func TestAgentInjectEmitsExactAdditionalContext(t *testing.T) {
	replyBody, err := json.Marshal(struct {
		Directives []agent.Directive `json:"directives"`
	}{Directives: []agent.Directive{
		{ID: 7, Origin: "human", Text: "inspect the race"},
		{ID: 9, Origin: "system", Text: "run focused tests"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	socket, rec := fakeDaemon(t, func(daemon.Envelope) daemon.Reply {
		return daemon.Reply{OK: true, Body: replyBody}
	})
	d := testDeps(socket)
	ensureCalls := 0
	d.EnsureCurrentIfRunning = func() error {
		ensureCalls++
		_, err := d.NewClient().Health()
		return err
	}
	stdout, stderr := executeAgentHook(t, AgentInjectCmd(d), `{
		"session_id":"s1","cwd":"/repo","tool_name":"Read"
	}`)
	want := "{\"hookSpecificOutput\":{\"hookEventName\":\"PreToolUse\",\"additionalContext\":" +
		"\"Directives from the echo-test steering channel (operator-authorized):\\n" +
		"- [human #7] inspect the race\\n- [system #9] run focused tests\\n" +
		"Act on each directive once, then continue your task.\"}}\n"
	if stdout != want || stderr != "" {
		t.Fatalf("output = stdout %q stderr %q, want stdout %q and silent stderr", stdout, stderr, want)
	}
	if ensureCalls != 0 {
		t.Fatalf("ensure calls = %d, want 0", ensureCalls)
	}
	if got := envelopeCount(rec); got != 1 {
		t.Fatalf("daemon envelopes = %d, want exactly 1", got)
	}
	env := rec.last()
	if env.Op != daemon.OpAgentInject || env.Session != "s1" || env.Scope != "/repo" || env.ClaudePID != testClaudePID {
		t.Fatalf("envelope = %+v", env)
	}
	var body agentInjectBody
	if err := json.Unmarshal(env.Body, &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if body.AgentID != "" {
		t.Fatalf("top-level agent id = %q, want empty", body.AgentID)
	}
}

func TestAgentInjectEmptyDrainIsSilent(t *testing.T) {
	socket, _ := fakeDaemon(t, func(daemon.Envelope) daemon.Reply {
		return daemon.Reply{OK: true, Body: json.RawMessage(`{"directives":[]}`)}
	})
	stdout, stderr := executeAgentHook(t, AgentInjectCmd(testDeps(socket)), `{"session_id":"s1","agent_id":"a1"}`)
	if stdout != "" || stderr != "" {
		t.Fatalf("output = stdout %q stderr %q, want silent", stdout, stderr)
	}
}

func TestAgentStopAllowSendsEnvelope(t *testing.T) {
	socket, rec := fakeDaemon(t, func(daemon.Envelope) daemon.Reply {
		return daemon.Reply{OK: true, Allow: true}
	})
	stdout, stderr := executeAgentHook(t, AgentStopCmd(testDeps(socket)), `{
		"session_id":"s1","cwd":"/repo","agent_id":"a1",
		"last_assistant_message":"done","agent_transcript_path":"/tmp/a1.jsonl"
	}`)
	if stdout != "" || stderr != "" {
		t.Fatalf("output = stdout %q stderr %q, want silent", stdout, stderr)
	}
	env := rec.last()
	if env.Op != daemon.OpAgentStop || env.Session != "s1" || env.Scope != "/repo" || env.ClaudePID != testClaudePID {
		t.Fatalf("envelope = %+v", env)
	}
	var body agentStopBody
	if err := json.Unmarshal(env.Body, &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if body != (agentStopBody{
		AgentID: "a1", LastAssistantMessage: "done", AgentTranscriptPath: "/tmp/a1.jsonl",
	}) {
		t.Fatalf("body = %+v", body)
	}
}

func TestAgentStopBlockWritesExactDecision(t *testing.T) {
	socket, _ := fakeDaemon(t, func(daemon.Envelope) daemon.Reply {
		return daemon.Reply{OK: true, Allow: false, Reason: "finish the requested check"}
	})
	stdout, stderr := executeAgentHook(t, AgentStopCmd(testDeps(socket)), `{"session_id":"s1","agent_id":"a1"}`)
	const want = "{\"decision\":\"block\",\"reason\":\"finish the requested check\"}\n"
	if stdout != want || stderr != "" {
		t.Fatalf("output = stdout %q stderr %q, want stdout %q and silent stderr", stdout, stderr, want)
	}
}

func TestAgentReportPassesRawObservation(t *testing.T) {
	socket, rec := fakeDaemon(t, func(daemon.Envelope) daemon.Reply { return daemon.Reply{OK: true} })
	stdout, stderr := executeAgentHook(t, AgentReportCmd(testDeps(socket)), `{
		"session_id":"s1","cwd":"/repo","tool_name":"Task","tool_use_id":"tool-7",
		"tool_input":{"prompt":"inspect"},
		"tool_response":{"agentId":"a1","outputFile":"/tmp/a1.out","nested":{"opaque":true}}
	}`)
	if stdout != "" || stderr != "" {
		t.Fatalf("output = stdout %q stderr %q, want silent", stdout, stderr)
	}
	env := rec.last()
	if env.Op != daemon.OpAgentReport || env.Session != "s1" || env.Scope != "/repo" || env.ClaudePID != testClaudePID {
		t.Fatalf("envelope = %+v", env)
	}
	var body agentReportBody
	if err := json.Unmarshal(env.Body, &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if body.Session != "s1" || body.Scope != "/repo" || body.ToolName != "Task" || body.ToolUseID != "tool-7" {
		t.Fatalf("body identity = %+v", body)
	}
	if string(body.ToolInput) != `{"prompt":"inspect"}` {
		t.Fatalf("tool_input = %s", body.ToolInput)
	}
	if string(body.ToolResponse) != `{"agentId":"a1","outputFile":"/tmp/a1.out","nested":{"opaque":true}}` {
		t.Fatalf("tool_response = %s", body.ToolResponse)
	}
}

func TestAgentHookDialFailuresFailOpen(t *testing.T) {
	input := `{"session_id":"s1","cwd":"/repo","agent_id":"a1","tool_name":"Task"}`
	for _, tc := range []struct {
		name string
		cmd  func(Deps) *cobra.Command
	}{
		{name: "start", cmd: AgentStartCmd},
		{name: "inject", cmd: AgentInjectCmd},
		{name: "stop", cmd: AgentStopCmd},
		{name: "report", cmd: AgentReportCmd},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDeps(filepath.Join(t.TempDir(), "absent.sock"))
			stdout, stderr := executeAgentHook(t, tc.cmd(d), input)
			if stdout != "" || stderr != "" {
				t.Fatalf("output = stdout %q stderr %q, want silent fail-open", stdout, stderr)
			}
		})
	}
}

func TestAgentHookDaemonErrorsFailOpen(t *testing.T) {
	socket, _ := fakeDaemon(t, func(daemon.Envelope) daemon.Reply {
		return daemon.Reply{OK: false, Error: "subject resolution failed"}
	})
	input := `{"session_id":"s1","cwd":"/repo","agent_id":"a1","tool_name":"Task"}`
	for _, tc := range []struct {
		name string
		cmd  func(Deps) *cobra.Command
	}{
		{name: "start", cmd: AgentStartCmd},
		{name: "inject", cmd: AgentInjectCmd},
		{name: "stop", cmd: AgentStopCmd},
		{name: "report", cmd: AgentReportCmd},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr := executeAgentHook(t, tc.cmd(testDeps(socket)), input)
			if stdout != "" || stderr != "" {
				t.Fatalf("output = stdout %q stderr %q, want silent fail-open", stdout, stderr)
			}
		})
	}
}

func TestAgentStopOversizeLogsAndAllows(t *testing.T) {
	const maxFrameBytes = 256
	socket := liveDaemon(t, maxFrameBytes)
	input := `{"session_id":"s1","cwd":"/repo","agent_id":"a1","last_assistant_message":"` + strings.Repeat("x", 512) + `"}`
	d := testDeps(socket)
	stdout, stderr := executeAgentHook(t, AgentStopCmd(d), input)
	if stdout != "" {
		t.Fatalf("stdout = %q, want allow without block output", stdout)
	}
	var in hookInput
	if err := json.Unmarshal([]byte(input), &in); err != nil {
		t.Fatalf("hook input: %v", err)
	}
	body, err := json.Marshal(agentStopBody{
		AgentID: in.AgentID, LastAssistantMessage: in.LastAssistantMessage,
		AgentTranscriptPath: in.AgentTranscriptPath,
	})
	if err != nil {
		t.Fatalf("agent-stop body: %v", err)
	}
	frame, err := json.Marshal(daemon.Envelope{
		Proto: daemon.ProtocolVersion, Op: daemon.OpAgentStop, Session: in.SessionID,
		ClaudePID: testClaudePID, Scope: in.Cwd, Body: body,
	})
	if err != nil {
		t.Fatalf("agent-stop frame: %v", err)
	}
	want := "agent-stop: frame-too-large: request frame is " + fmt.Sprint(len(frame)) + " bytes; allowing stop\n"
	if stderr != want {
		t.Fatalf("stderr = %q, want %q", stderr, want)
	}
}

func TestAgentHookMalformedInputAllows(t *testing.T) {
	for _, tc := range []struct {
		name string
		cmd  func(Deps) *cobra.Command
	}{
		{name: "start", cmd: AgentStartCmd},
		{name: "inject", cmd: AgentInjectCmd},
		{name: "stop", cmd: AgentStopCmd},
		{name: "report", cmd: AgentReportCmd},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDeps(filepath.Join(t.TempDir(), "absent.sock"))
			stdout, stderr := executeAgentHook(t, tc.cmd(d), "{")
			if stdout != "" || stderr != "" {
				t.Fatalf("output = stdout %q stderr %q, want silent allow", stdout, stderr)
			}
		})
	}
}

func TestAgentEmptyIDShortCircuitsStartAndStop(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		cmd   func(Deps) *cobra.Command
	}{
		{name: "start empty agent", input: `{"session_id":"s1"}`, cmd: AgentStartCmd},
		{name: "start empty session", input: `{"agent_id":"a1"}`, cmd: AgentStartCmd},
		{name: "stop empty agent", input: `{"session_id":"s1"}`, cmd: AgentStopCmd},
	} {
		t.Run(tc.name, func(t *testing.T) {
			socket, rec := fakeDaemon(t, func(daemon.Envelope) daemon.Reply { return daemon.Reply{OK: true} })
			stdout, stderr := executeAgentHook(t, tc.cmd(testDeps(socket)), tc.input)
			if stdout != "" || stderr != "" || envelopeCount(rec) != 0 {
				t.Fatalf("stdout %q stderr %q envelopes %d, want silent short-circuit", stdout, stderr, envelopeCount(rec))
			}
		})
	}
}

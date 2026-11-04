package channel

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func decodeReplies(t *testing.T, out *bytes.Buffer) []map[string]any {
	t.Helper()
	dec := json.NewDecoder(out)
	var replies []map[string]any
	for {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			break
		}
		replies = append(replies, m)
	}
	return replies
}

// replyByID finds the reply carrying the given JSON-RPC id. tools/call replies
// are written from their own goroutine, so the output order no longer tracks the
// request order — a reply must be matched by id, not position.
func replyByID(t *testing.T, replies []map[string]any, id float64) map[string]any {
	t.Helper()
	for _, r := range replies {
		if rid, ok := r["id"].(float64); ok && rid == id {
			return r
		}
	}
	t.Fatalf("no reply with id %v; got %+v", id, replies)
	return nil
}

func TestServerInitializeListCall(t *testing.T) {
	var gotArgs string
	tools := []Tool{{
		Name:        "echo",
		Description: "echo back",
		InputSchema: map[string]any{"type": "object"},
		Handler: func(_ context.Context, args json.RawMessage, _ func(string)) (string, bool) {
			gotArgs = string(args)
			return "pong", false
		},
	}}
	srv := NewServer(ServerInfo{Name: "cc-test", Version: "v1.2.3"}, tools)

	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"x":1}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":4,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":5,"method":"bogus"}`,
	}, "\n")
	var out bytes.Buffer
	if err := srv.Serve(context.Background(), strings.NewReader(in), &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}

	replies := decodeReplies(t, &out)
	if len(replies) != 5 {
		t.Fatalf("got %d replies, want 5 (client notification answered nothing)", len(replies))
	}

	initResult := replyByID(t, replies, 1)["result"].(map[string]any)
	if initResult["protocolVersion"] != mcpProtocolVersion {
		t.Fatalf("protocolVersion = %v, want %s", initResult["protocolVersion"], mcpProtocolVersion)
	}
	info := initResult["serverInfo"].(map[string]any)
	if info["name"] != "cc-test" || info["version"] != "v1.2.3" {
		t.Fatalf("serverInfo = %+v, want cc-test/v1.2.3", info)
	}
	if _, hasInstr := initResult["instructions"]; hasInstr {
		t.Fatalf("initialize must omit instructions when unset: %+v", initResult)
	}
	if tools := initResult["capabilities"].(map[string]any)["tools"].(map[string]any); tools["listChanged"] != true {
		t.Fatalf("capabilities.tools = %+v, want listChanged so a client refetches tools when a resumed server announces them", tools)
	}

	listTools := replyByID(t, replies, 2)["result"].(map[string]any)["tools"].([]any)
	if len(listTools) != 1 {
		t.Fatalf("tools/list returned %d tools, want 1", len(listTools))
	}
	tool := listTools[0].(map[string]any)
	if tool["name"] != "echo" || tool["description"] != "echo back" {
		t.Fatalf("listed tool = %+v, want echo/echo back", tool)
	}

	callContent := replyByID(t, replies, 3)["result"].(map[string]any)["content"].([]any)
	if text := callContent[0].(map[string]any)["text"]; text != "pong" {
		t.Fatalf("tools/call text = %v, want pong", text)
	}
	if gotArgs != `{"x":1}` {
		t.Fatalf("handler saw args %q, want {\"x\":1}", gotArgs)
	}

	if ping := replyByID(t, replies, 4)["result"].(map[string]any); len(ping) != 0 {
		t.Fatalf("ping result = %+v, want empty object", ping)
	}

	errObj := replyByID(t, replies, 5)["error"].(map[string]any)
	if errObj["code"].(float64) != -32601 {
		t.Fatalf("bogus method error code = %v, want -32601", errObj["code"])
	}
}

func TestServerInitializeInstructions(t *testing.T) {
	srv := NewServer(ServerInfo{Name: "x", Version: "v1", Instructions: "handshakes need no reply"}, nil)
	var out bytes.Buffer
	if err := srv.Serve(context.Background(), strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`), &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	replies := decodeReplies(t, &out)
	if len(replies) != 1 {
		t.Fatalf("got %d replies, want 1", len(replies))
	}
	if instr := replies[0]["result"].(map[string]any)["instructions"]; instr != "handshakes need no reply" {
		t.Fatalf("initialize instructions = %v, want the configured text", instr)
	}
}

func TestServerResumedAnnouncesToolsChanged(t *testing.T) {
	srv := NewServer(ServerInfo{Name: "x", Version: "v2", Resumed: true}, []Tool{{Name: "echo"}})
	var out bytes.Buffer
	if err := srv.Serve(context.Background(), strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`), &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	replies := decodeReplies(t, &out)
	if len(replies) != 2 {
		t.Fatalf("resumed server wrote %+v, want the announcement then the tools/list reply", replies)
	}
	if _, hasID := replies[0]["id"]; hasID || replies[0]["method"] != "notifications/tools/list_changed" {
		t.Fatalf("first frame = %+v, want a notifications/tools/list_changed notification", replies[0])
	}
	if params, ok := replies[0]["params"].(map[string]any); !ok || len(params) != 0 {
		t.Fatalf("announcement params = %v, want an empty object", replies[0]["params"])
	}
	if tools := replyByID(t, replies, 1)["result"].(map[string]any)["tools"].([]any); len(tools) != 1 {
		t.Fatalf("tools/list after the announcement = %+v, want the resumed server's tool", tools)
	}
}

func TestServerToolCallErrors(t *testing.T) {
	tools := []Tool{{
		Name: "boom",
		Handler: func(_ context.Context, _ json.RawMessage, _ func(string)) (string, bool) {
			return "it failed", true
		},
	}}
	srv := NewServer(ServerInfo{Name: "x", Version: "v1"}, tools)

	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"boom"}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"missing"}}`,
	}, "\n")
	var out bytes.Buffer
	if err := srv.Serve(context.Background(), strings.NewReader(in), &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}

	replies := decodeReplies(t, &out)
	if len(replies) != 2 {
		t.Fatalf("got %d replies, want 2", len(replies))
	}

	boom := replyByID(t, replies, 1)["result"].(map[string]any)
	if boom["isError"] != true {
		t.Fatalf("handler isErr=true should map to isError result: %+v", boom)
	}
	if text := boom["content"].([]any)[0].(map[string]any)["text"]; text != "it failed" {
		t.Fatalf("error text = %v, want it failed", text)
	}

	missing := replyByID(t, replies, 2)["result"].(map[string]any)
	if missing["isError"] != true {
		t.Fatalf("unknown tool should be an error result: %+v", missing)
	}
	if text := missing["content"].([]any)[0].(map[string]any)["text"]; text != "unknown tool: missing" {
		t.Fatalf("unknown tool text = %v", text)
	}
}

func TestServerNotify(t *testing.T) {
	srv := NewServer(ServerInfo{Name: "x", Version: "v1"}, nil)
	var out bytes.Buffer
	if err := srv.Serve(context.Background(), strings.NewReader(""), &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if err := srv.Notify("notifications/test", map[string]any{"k": "v"}); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal(out.Bytes(), &m); err != nil {
		t.Fatalf("unmarshal notification: %v", err)
	}
	if m["method"] != "notifications/test" {
		t.Fatalf("method = %v, want notifications/test", m["method"])
	}
	if _, hasID := m["id"]; hasID {
		t.Fatalf("notification must carry no id: %+v", m)
	}
	if m["params"].(map[string]any)["k"] != "v" {
		t.Fatalf("params = %+v, want {k:v}", m["params"])
	}
}

func TestServerNotifyBeforeServe(t *testing.T) {
	srv := NewServer(ServerInfo{Name: "x", Version: "v1"}, nil)
	if err := srv.Notify("notifications/test", nil); err == nil {
		t.Fatal("Notify before Serve must error, not panic on a nil writer")
	}
}

func TestEventType(t *testing.T) {
	if got := eventType(`{"type":"comment.created","x":1}`); got != "comment.created" {
		t.Fatalf("eventType = %q, want comment.created", got)
	}
	if got := eventType(`not json`); got != "" {
		t.Fatalf("eventType on garbage = %q, want empty", got)
	}
}

// safeBuffer is an io.Writer safe for the concurrent writes a parked handler's
// progress, a sibling tools/call reply, and Notify all make to the same pipe.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitFor polls out until cond holds or the deadline fires.
func waitFor(t *testing.T, out *safeBuffer, cond func(string) bool) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		if cond(out.String()) {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("condition not met before deadline; out = %q", out.String())
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// TestServerParkUnderLoad pins the async-dispatch contract: a tools/call whose
// handler parks must not stall the loop. While "block" is parked, a sibling
// tools/call ("ping") still replies, a Notify push still goes out, and the parked
// call's own progress notification still lands. Serve's deferred wait then drains
// the released handler before returning, so the buffer is stable for accounting.
func TestServerParkUnderLoad(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	tools := []Tool{
		{
			Name: "block",
			Handler: func(_ context.Context, _ json.RawMessage, progress func(string)) (string, bool) {
				progress("working")
				close(started)
				<-release
				return "unblocked", false
			},
		},
		{
			Name: "ping",
			Handler: func(_ context.Context, _ json.RawMessage, _ func(string)) (string, bool) {
				return "pong", false
			},
		},
	}
	srv := NewServer(ServerInfo{Name: "x", Version: "v1"}, tools)

	inR, inW := io.Pipe()
	out := &safeBuffer{}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(context.Background(), inR, out) }()

	if _, err := io.WriteString(inW, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"block","_meta":{"progressToken":"tok-1"}}}`+"\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("blocking handler never started; the loop is frozen")
	}

	// (a) a sibling tools/call replies while "block" is parked.
	if _, err := io.WriteString(inW, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"ping"}}`+"\n"); err != nil {
		t.Fatal(err)
	}
	// (b) a Notify push goes out while "block" is parked.
	if err := srv.Notify("notifications/test", map[string]any{"k": "v"}); err != nil {
		t.Fatalf("Notify while parked: %v", err)
	}

	// (a)+(b)+(c): all three land before the parked call is ever released.
	waitFor(t, out, func(s string) bool {
		return strings.Contains(s, "pong") &&
			strings.Contains(s, "notifications/test") &&
			strings.Contains(s, "notifications/progress")
	})

	close(release)
	if err := inW.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after EOF and release")
	}

	replies := decodeReplies(t, bytes.NewBufferString(out.String()))
	if text := replyByID(t, replies, 1)["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]; text != "unblocked" {
		t.Fatalf("block reply text = %v, want unblocked", text)
	}
	if text := replyByID(t, replies, 2)["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]; text != "pong" {
		t.Fatalf("ping reply text = %v, want pong", text)
	}

	var progress map[string]any
	for _, r := range replies {
		if r["method"] == "notifications/progress" {
			progress = r["params"].(map[string]any)
		}
	}
	if progress == nil {
		t.Fatalf("no notifications/progress frame; got %+v", replies)
	}
	if progress["progressToken"] != "tok-1" {
		t.Fatalf("progressToken = %v, want tok-1", progress["progressToken"])
	}
	if progress["progress"].(float64) != 1 {
		t.Fatalf("progress = %v, want 1 (monotonic per call)", progress["progress"])
	}
	if progress["message"] != "working" {
		t.Fatalf("progress message = %v, want working", progress["message"])
	}
}

func TestAToolsMetaIsListedOnlyWhenSet(t *testing.T) {
	meta := map[string]any{"anthropic/alwaysLoad": true}
	srv := NewServer(ServerInfo{Name: "x", Version: "1"}, []Tool{{Name: "pinned", Meta: meta}, {Name: "plain"}})
	listed := srv.toolSchemas()
	if got := listed[0].(map[string]any)["_meta"]; !reflect.DeepEqual(got, meta) {
		t.Fatalf("pinned _meta = %v, want %v", got, meta)
	}
	if _, ok := listed[1].(map[string]any)["_meta"]; ok {
		t.Fatalf("plain tool listed _meta: %v", listed[1])
	}
}

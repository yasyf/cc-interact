package channel

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
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

func TestServerInitializeListCall(t *testing.T) {
	var gotArgs string
	tools := []Tool{{
		Name:        "echo",
		Description: "echo back",
		InputSchema: map[string]any{"type": "object"},
		Handler: func(_ context.Context, args json.RawMessage) (string, bool) {
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

	initResult := replies[0]["result"].(map[string]any)
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

	listTools := replies[1]["result"].(map[string]any)["tools"].([]any)
	if len(listTools) != 1 {
		t.Fatalf("tools/list returned %d tools, want 1", len(listTools))
	}
	tool := listTools[0].(map[string]any)
	if tool["name"] != "echo" || tool["description"] != "echo back" {
		t.Fatalf("listed tool = %+v, want echo/echo back", tool)
	}

	callContent := replies[2]["result"].(map[string]any)["content"].([]any)
	if text := callContent[0].(map[string]any)["text"]; text != "pong" {
		t.Fatalf("tools/call text = %v, want pong", text)
	}
	if gotArgs != `{"x":1}` {
		t.Fatalf("handler saw args %q, want {\"x\":1}", gotArgs)
	}

	if ping := replies[3]["result"].(map[string]any); len(ping) != 0 {
		t.Fatalf("ping result = %+v, want empty object", ping)
	}

	errObj := replies[4]["error"].(map[string]any)
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

func TestServerToolCallErrors(t *testing.T) {
	tools := []Tool{{
		Name: "boom",
		Handler: func(_ context.Context, _ json.RawMessage) (string, bool) {
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

	boom := replies[0]["result"].(map[string]any)
	if boom["isError"] != true {
		t.Fatalf("handler isErr=true should map to isError result: %+v", boom)
	}
	if text := boom["content"].([]any)[0].(map[string]any)["text"]; text != "it failed" {
		t.Fatalf("error text = %v, want it failed", text)
	}

	missing := replies[1]["result"].(map[string]any)
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

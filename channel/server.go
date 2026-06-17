// Package channel is cc-interact's opt-in stdio MCP server: a generic JSON-RPC
// loop that advertises a set of tools, dispatches tools/call to their handlers,
// and pushes server-initiated notifications down the same stdio pipe. A consumer
// wires StreamEvents' notify through (*Server).Notify to turn a subject's event
// log into live channel notifications while the loop answers tool calls.
package channel

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
)

// mcpProtocolVersion is the MCP version advertised when the client omits one.
const mcpProtocolVersion = "2025-06-18"

// Tool is one MCP tool: its advertised name, description, and JSON input schema,
// plus the handler that runs on tools/call. The handler returns the result text
// and whether it is an error; the server maps that to the MCP content/isError
// result shape.
type Tool struct {
	Name        string
	Description string
	InputSchema map[string]any
	Handler     func(ctx context.Context, args json.RawMessage) (text string, isErr bool)
}

// ServerInfo carries the initialize handshake fields: the serverInfo name and
// version, plus optional top-level instructions folded into the client's prompt.
type ServerInfo struct {
	Name         string
	Version      string
	Instructions string
}

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Server is a running stdio MCP server: the initialize/tools.list/tools.call
// loop plus a synchronized writer the consumer pushes notifications through. The
// output writer is bound by Serve, so Notify must be called once Serve has run.
type Server struct {
	info  ServerInfo
	tools map[string]Tool
	list  []Tool

	mu  sync.Mutex
	out io.Writer
}

// NewServer builds a Server advertising tools. tools/list preserves the given
// order; tools/call dispatches by Name.
func NewServer(info ServerInfo, tools []Tool) *Server {
	m := make(map[string]Tool, len(tools))
	for _, t := range tools {
		m[t.Name] = t
	}
	return &Server{info: info, tools: m, list: tools}
}

// Serve runs the JSON-RPC loop over in, writing replies and notifications to
// out, until in reaches EOF or errors. It answers initialize, tools/list,
// tools/call, and ping; client notifications (messages without an id) are
// ignored, and any other method returns method-not-found.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	s.mu.Lock()
	s.out = out
	s.mu.Unlock()

	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var msg rpcMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}
		if len(msg.ID) == 0 {
			continue // notification from the client; nothing to answer
		}
		switch msg.Method {
		case "initialize":
			res := map[string]any{
				"protocolVersion": mcpProtocolVersion,
				"capabilities":    map[string]any{"tools": map[string]any{}, "experimental": map[string]any{"claude/channel": map[string]any{}}},
				"serverInfo":      map[string]any{"name": s.info.Name, "version": s.info.Version},
			}
			if s.info.Instructions != "" {
				res["instructions"] = s.info.Instructions
			}
			s.reply(msg.ID, res)
		case "tools/list":
			s.reply(msg.ID, map[string]any{"tools": s.toolSchemas()})
		case "tools/call":
			s.reply(msg.ID, s.handleToolCall(ctx, msg.Params))
		case "ping":
			s.reply(msg.ID, map[string]any{})
		default:
			s.replyError(msg.ID, -32601, "method not found: "+msg.Method)
		}
	}
	return sc.Err()
}

// Notify writes a server-initiated JSON-RPC notification (no id) down the stdio
// pipe. StreamEvents wires its notify callback through this so each event in a
// subject's log becomes a live channel notification. The write error propagates
// so the consumer's cursor does not advance past an undelivered event.
func (s *Server) Notify(method string, params any) error {
	return s.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (s *Server) toolSchemas() []any {
	out := make([]any, 0, len(s.list))
	for _, t := range s.list {
		out = append(out, map[string]any{
			"name":        t.Name,
			"description": t.Description,
			"inputSchema": t.InputSchema,
		})
	}
	return out
}

func (s *Server) handleToolCall(ctx context.Context, params json.RawMessage) map[string]any {
	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &call); err != nil {
		return toolError("bad tool arguments: " + err.Error())
	}
	if len(call.Arguments) == 0 {
		call.Arguments = json.RawMessage("{}")
	}
	tool, ok := s.tools[call.Name]
	if !ok {
		return toolError("unknown tool: " + call.Name)
	}
	text, isErr := tool.Handler(ctx, call.Arguments)
	if isErr {
		return toolError(text)
	}
	return toolOK(text)
}

func (s *Server) reply(id json.RawMessage, result any) {
	_ = s.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (s *Server) replyError(id json.RawMessage, code int, message string) {
	_ = s.write(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})
}

func (s *Server) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.out == nil {
		return errors.New("channel server is not serving")
	}
	if _, err := s.out.Write(b); err != nil {
		return err
	}
	_, err = s.out.Write([]byte("\n"))
	return err
}

func toolOK(text string) map[string]any {
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}
}

func toolError(msg string) map[string]any {
	return map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": msg}}}
}

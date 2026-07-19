package channel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/yasyf/cc-interact/agent"
)

// awaitChunk bounds each daemon long-poll so the handler can nudge progress
// between chunks — well under the MCP stdio idle timer. Tests shrink it.
var awaitChunk = 25 * time.Second

// awaitHeader prefixes delivered directives, naming the steering channel.
const awaitHeader = "Steering channel — directives addressed to you:"

// awaitNoDirective is returned, not as an error, when the window elapses empty.
const awaitNoDirective = "no directive — re-park with await or continue"

// awaitProgress is the between-chunks nudge, emitted only when the caller sent a
// progressToken.
const awaitProgress = "await: no directive yet — still watching the steering channel"

// maxAwaitSeconds caps timeout_seconds at a day, far below int64-nanosecond overflow.
const maxAwaitSeconds = 86400

// AwaitSpec parameterizes NewAwaitTool: how the channel resolves its subject and
// the daemon port, and the default long-poll window when the caller passes none.
type AwaitSpec struct {
	// Resolve returns the current subject id and daemon HTTP port. It matches cmd's
	// resolveSubject once the caller closes over session/scope/consumer, so a
	// consumer wires it without an adapter.
	Resolve func(ctx context.Context) (subjectID string, port int, err error)
	// Timeout is the total long-poll window when the caller omits timeout_seconds.
	Timeout time.Duration
}

// NewAwaitTool builds the "await" MCP tool: a child long-polls the daemon for
// directives addressed to its agent_id (learned from the greeting directive,
// since the stdio session is shared and calls are anonymous). Directives return
// as readable text, an empty window as a re-park notice (both isErr=false), and a
// resolve or HTTP failure as isErr=true.
func NewAwaitTool(spec AwaitSpec) Tool {
	return Tool{
		Name:        "await",
		Description: "Long-poll the steering channel for directives addressed to you. Blocks until a directive arrives or the poll window elapses, then returns any pending directives as text or a notice to re-park. Pass your own agent_id, as named in the greeting directive.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"agent_id": map[string]any{
					"type":        "string",
					"description": "Your agent id, as named in the greeting directive.",
				},
				"timeout_seconds": map[string]any{
					"type":        "integer",
					"description": "Total seconds to wait before returning a no-directive notice. Defaults to the channel's long-poll window.",
				},
			},
			"required": []string{"agent_id"},
		},
		Handler: func(ctx context.Context, args json.RawMessage, progress func(string)) (string, bool) {
			var in struct {
				AgentID        string `json:"agent_id"`
				TimeoutSeconds int    `json:"timeout_seconds"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return fmt.Sprintf("await: bad arguments: %v", err), true
			}
			if in.AgentID == "" {
				return "await: agent_id is required", true
			}
			subjectID, port, err := spec.Resolve(ctx)
			if err != nil {
				return fmt.Sprintf("await: resolve subject: %v", err), true
			}
			total := spec.Timeout
			if in.TimeoutSeconds > maxAwaitSeconds {
				// Clamp untrusted input: past ~292 years the nanosecond
				// conversion wraps int64 and the deadline lands in the past.
				in.TimeoutSeconds = maxAwaitSeconds
			}
			if in.TimeoutSeconds > 0 {
				total = time.Duration(in.TimeoutSeconds) * time.Second
			}
			deadline := time.Now().Add(total)
			for {
				if err := ctx.Err(); err != nil {
					return fmt.Sprintf("await: %v", err), true
				}
				remaining := time.Until(deadline)
				if remaining <= 0 {
					return awaitNoDirective, false
				}
				directives, err := awaitPoll(ctx, port, subjectID, in.AgentID, min(remaining, awaitChunk))
				if err != nil {
					return fmt.Sprintf("await: %v", err), true
				}
				if len(directives) > 0 {
					return formatDirectives(directives), false
				}
				if time.Until(deadline) > 0 {
					progress(awaitProgress)
				}
			}
		},
	}
}

// awaitPoll runs one long-poll chunk against the daemon: 200 decodes directives,
// 204 returns none, any other status is an error. Cancellation propagates through
// the request context.
func awaitPoll(ctx context.Context, port int, subjectID, agentID string, timeout time.Duration) ([]agent.Directive, error) {
	endpoint := fmt.Sprintf("http://127.0.0.1:%d/agents/await?subject=%s&agent=%s&timeout=%s",
		port, url.QueryEscape(subjectID), url.QueryEscape(agentID), timeout)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build await request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("await request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
		var reply struct {
			Directives []agent.Directive `json:"directives"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
			return nil, fmt.Errorf("decode await directives: %w", err)
		}
		return reply.Directives, nil
	case http.StatusNoContent:
		return nil, nil
	default:
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("await status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
}

// formatDirectives renders directives as one "[<origin> #<id>] <text>" block
// each, prefixed by awaitHeader.
func formatDirectives(directives []agent.Directive) string {
	var b strings.Builder
	b.WriteString(awaitHeader)
	for _, d := range directives {
		fmt.Fprintf(&b, "\n\n[%s #%d] %s", d.Origin, d.ID, d.Text)
	}
	return b.String()
}

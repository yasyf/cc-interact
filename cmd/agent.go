package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-interact/agent"
	"github.com/yasyf/cc-interact/daemon"
	"github.com/yasyf/cc-interact/transcript"
	"github.com/yasyf/daemonkit/wire"
)

// AgentStartCmd is the hidden SubagentStart hook handler.
func AgentStartCmd(d Deps) *cobra.Command {
	return &cobra.Command{
		Use:    "agent-start",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			in := readHookInput(cmd.InOrStdin())
			if in.SessionID == "" || in.AgentID == "" {
				return nil
			}
			if err := d.EnsureCurrentIfRunning(); err != nil {
				return nil
			}
			transcriptPath := ""
			if in.TranscriptPath != "" && in.AgentID != "" {
				transcriptPath = filepath.Join(transcript.SubagentsDir(in.TranscriptPath), "agent-"+in.AgentID+".jsonl")
			}
			body, _ := json.Marshal(agentStartBody{
				AgentID: in.AgentID, AgentType: in.AgentType, SessionID: in.SessionID,
				TranscriptPath: transcriptPath,
			})
			_, _ = d.NewClient().Do(cmd.Context(), daemon.Envelope{
				Op: daemon.OpAgentStart, Session: in.SessionID, ClaudePID: d.ClaudePID(), Scope: in.Cwd, Body: body,
			})
			return nil
		},
	}
}

// AgentInjectCmd is the hidden PreToolUse hook handler for directive delivery.
func AgentInjectCmd(d Deps) *cobra.Command {
	return &cobra.Command{
		Use:    "agent-inject",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			in := readHookInput(cmd.InOrStdin())
			body, _ := json.Marshal(agentInjectBody{AgentID: in.AgentID})
			reply, err := d.NewClient().Do(cmd.Context(), daemon.Envelope{
				Op: daemon.OpAgentInject, Session: in.SessionID, ClaudePID: d.ClaudePID(), Scope: in.Cwd, Body: body,
			})
			if err != nil || !reply.OK || len(reply.Body) == 0 {
				return nil
			}
			var drained directivesReply
			if err := json.Unmarshal(reply.Body, &drained); err != nil || len(drained.Directives) == 0 {
				return nil
			}
			text := fmt.Sprintf("Directives from the %s steering channel (operator-authorized):", cmd.Root().Name())
			for _, directive := range drained.Directives {
				text += fmt.Sprintf("\n- [%s #%d] %s", directive.Origin, directive.ID, directive.Text)
			}
			text += "\nAct on each directive once, then continue your task."
			out := hookSpecificOutput{
				HookSpecificOutput: preToolUseOutput{HookEventName: "PreToolUse", AdditionalContext: text},
			}
			_ = json.NewEncoder(cmd.OutOrStdout()).Encode(out)
			return nil
		},
	}
}

// AgentStopCmd is the hidden SubagentStop hook handler.
func AgentStopCmd(d Deps) *cobra.Command {
	return &cobra.Command{
		Use:    "agent-stop",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			in := readHookInput(cmd.InOrStdin())
			if in.AgentID == "" {
				return nil
			}
			body, _ := json.Marshal(agentStopBody{
				AgentID: in.AgentID, LastAssistantMessage: in.LastAssistantMessage,
				AgentTranscriptPath: in.AgentTranscriptPath,
			})
			env := daemon.Envelope{
				Op: daemon.OpAgentStop, Session: in.SessionID, ClaudePID: d.ClaudePID(), Scope: in.Cwd, Body: body,
			}
			reply, err := d.NewClient().Do(cmd.Context(), env)
			if err != nil {
				if !errors.Is(err, daemon.ErrDaemonUnavailable) {
					_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "agent-stop: malformed daemon reply: %v; allowing stop\n", err)
				}
				return nil
			}
			if !reply.OK {
				if reply.Error == wire.ErrFrameTooLarge.Error() {
					env.Proto = daemon.ProtocolVersion
					frame, _ := json.Marshal(env)
					_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "agent-stop: frame-too-large: request frame is %d bytes; allowing stop\n", len(frame))
				}
				return nil
			}
			if !reply.Allow {
				_ = json.NewEncoder(cmd.OutOrStdout()).Encode(stopDecision{Decision: "block", Reason: reply.Reason})
			}
			return nil
		},
	}
}

// AgentReportCmd is the hidden PostToolUse(Task|Agent) hook handler.
func AgentReportCmd(d Deps) *cobra.Command {
	return &cobra.Command{
		Use:    "agent-report",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			in := readHookInput(cmd.InOrStdin())
			body, _ := json.Marshal(agentReportBody{
				Session: in.SessionID, Scope: in.Cwd, ToolName: in.ToolName, ToolUseID: in.ToolUseID,
				ToolInput: in.ToolInput, ToolResponse: in.ToolResponse,
			})
			_, _ = d.NewClient().Do(cmd.Context(), daemon.Envelope{
				Op: daemon.OpAgentReport, Session: in.SessionID, ClaudePID: d.ClaudePID(), Scope: in.Cwd, Body: body,
			})
			return nil
		},
	}
}

type agentStartBody struct {
	AgentID        string `json:"agent_id"`
	AgentType      string `json:"agent_type"`
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
}

type agentInjectBody struct {
	AgentID string `json:"agent_id"`
}

type agentStopBody struct {
	AgentID              string `json:"agent_id"`
	LastAssistantMessage string `json:"last_assistant_message"`
	AgentTranscriptPath  string `json:"agent_transcript_path"`
}

type agentReportBody struct {
	Session      string          `json:"session"`
	Scope        string          `json:"scope"`
	ToolName     string          `json:"tool_name"`
	ToolUseID    string          `json:"tool_use_id"`
	ToolInput    json.RawMessage `json:"tool_input"`
	ToolResponse json.RawMessage `json:"tool_response"`
}

type directivesReply struct {
	Directives []agent.Directive `json:"directives"`
}

type hookSpecificOutput struct {
	HookSpecificOutput preToolUseOutput `json:"hookSpecificOutput"`
}

type preToolUseOutput struct {
	HookEventName     string `json:"hookEventName"`
	AdditionalContext string `json:"additionalContext"`
}

type stopDecision struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}

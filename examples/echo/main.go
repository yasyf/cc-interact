// Command echo is the smallest complete cc-interact consumer: a headless
// human-in-the-loop loop with no browser, no SPA, and no sse.StaticHandler. A
// human POSTs an item to the daemon's REST plane; the agent's watch and MCP
// channel see it stream off the same /events plane a browser would read; the
// agent replies through a channel tool; the reply streams back. Items and
// replies live purely as events — there are no domain tables, so Config.Migrate
// is nil.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-interact/channel"
	"github.com/yasyf/cc-interact/cmd"
	"github.com/yasyf/cc-interact/daemon"
	"github.com/yasyf/cc-interact/event"
	"github.com/yasyf/cc-interact/subject"
	"github.com/yasyf/daemonkit/paths"
)

// appVersion is the ldflags stamp target: -X main.appVersion=<version>.
// A var, not a const — -X on a const is a silent no-op.
var appVersion = "0.0.0"

const (
	appName = "echo"
	appDir  = ".cc-echo"

	// defaultSession keys every echo subject. A headless demo spans several
	// short-lived CLI processes with no stable window pid between them, so the
	// session id is the cross-process ownership key (with the scope).
	defaultSession = "echo"

	statusOpen   = "open"
	statusClosed = "closed"

	// opStart creates or resumes the scope's subject; opReply appends the agent's
	// reply. Both are domain ops the daemon routes to the handlers below.
	opStart daemon.Op = "start"
	opReply daemon.Op = "reply"

	eventItem  = "echo.item"  // human-posted, OriginHuman
	eventReply = "echo.reply" // agent-posted over the channel, OriginAgent
	eventDone  = "echo.done"  // terminal event a watch stops on

	notifyMethod = "notifications/echo/channel"
)

var lifecycle = subject.Lifecycle{Initial: statusOpen, Closed: statusClosed}

// itemBody is the REST and channel-reply payload: which subject (id or slug) and
// the text to append as an event.
type itemBody struct {
	Subject string `json:"subject"`
	Text    string `json:"text"`
}

func appPaths() paths.Paths { return paths.Paths{App: appDir} }

func newClient() *daemon.Client { return daemon.NewClient(appPaths().SocketPath()) }

func launcher() daemon.Launcher {
	return daemon.Launcher{Paths: appPaths(), Version: appVersion, Args: []string{"daemon"}}
}

// cwdOr resolves the scope: the explicit flag, else the process working directory.
func cwdOr(cwd string) string {
	if cwd != "" {
		return cwd
	}
	d, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	return d
}

// slugFor is the subject's stable, printable name: deterministic per scope so a
// repeated start prints the same slug and never collides across scopes.
func slugFor(scope string) string {
	sum := sha256.Sum256([]byte(scope))
	return "echo-" + hex.EncodeToString(sum[:4])
}

// resolveRef maps a subject ref (canonical id or slug) to the canonical id,
// mirroring the daemon's own sse.Backend.ResolveSubject. Both the REST mount and
// the reply op key the events table through it.
func resolveRef(ctx context.Context, db *sql.DB, ref string) (string, bool, error) {
	var id string
	err := db.QueryRowContext(ctx, `SELECT id FROM subjects WHERE id=? OR slug=?`, ref, ref).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("resolve subject %q: %w", ref, err)
	}
	return id, true, nil
}

func payload(typ, text string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"type": typ, "text": text})
	return b
}

// buildServer composes the daemon: domain ops, the REST mount, and the channel
// presence lifecycle. It registers no static handler and imports no web package —
// headless is the whole point.
func buildServer() (*daemon.Server, error) {
	c := channel.Connectivity{}
	s, err := daemon.New(daemon.Config{
		AppName:        appName,
		Paths:          appPaths(),
		Version:        appVersion,
		ActiveStatuses: []string{statusOpen},
		// c.Type() (not c.EventType) so the SSE plane filters the same presence
		// type the hooks emit — correct even for the Connectivity zero value.
		PresenceEventType: c.Type(),
		OnPresenceChange:  c.OnPresenceChange,
		BootReconcile:     c.BootReconcile,
		// ScopeResolve nil → identity; Gate nil → allow every edit; Migrate nil → no
		// domain tables.
	})
	if err != nil {
		return nil, err
	}
	s.Register(opStart, handleStart)
	s.Register(opReply, handleReply)
	mountREST(s)
	return s, nil
}

// handleStart resolves or creates the scope's subject and reports it back to the
// `start` command. The slug is deterministic per scope so the reply needs no slug
// field — the client recomputes it.
func handleStart(hc daemon.HandlerCtx) daemon.Reply {
	s, _, err := hc.Subjects.Start(hc.Ctx, hc.Window, hc.Scope, slugFor(hc.Scope), lifecycle, false)
	if err != nil {
		return daemon.Reply{OK: false, Error: err.Error()}
	}
	return daemon.Reply{OK: true, SubjectID: s.ID, HTTPPort: hc.HTTPPort}
}

// handleReply is the agent side of the round trip: it resolves the subject ref
// the channel tool passed and appends the reply as an OriginAgent event through
// the daemon's single Append chokepoint.
func handleReply(hc daemon.HandlerCtx) daemon.Reply {
	var b itemBody
	if err := json.Unmarshal(hc.Env.Body, &b); err != nil {
		return daemon.Reply{OK: false, Error: "bad reply body: " + err.Error()}
	}
	id, ok, err := resolveRef(hc.Ctx, hc.DB, b.Subject)
	if err != nil {
		return daemon.Reply{OK: false, Error: err.Error()}
	}
	if !ok {
		return daemon.Reply{OK: false, Error: "unknown subject: " + b.Subject}
	}
	seq, err := hc.Append(hc.Ctx, &event.Event{
		SubjectID: id, Origin: event.OriginAgent, Type: eventReply, Payload: payload(eventReply, b.Text),
	})
	if err != nil {
		return daemon.Reply{OK: false, Error: err.Error()}
	}
	body, _ := json.Marshal(map[string]int64{"seq": seq})
	return daemon.Reply{OK: true, Body: body}
}

// mountREST is the consumer's RESTMount equivalent: POST /api/items appends a
// human item to a subject's log, keying it through the same subject resolver the
// SSE plane uses. It runs inside the daemon process, so it Appends directly.
func mountREST(s *daemon.Server) {
	s.Mux().HandleFunc("POST /api/items", func(w http.ResponseWriter, r *http.Request) {
		var b itemBody
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		id, ok, err := resolveRef(r.Context(), s.DB(), b.Subject)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !ok {
			http.Error(w, "unknown subject: "+b.Subject, http.StatusNotFound)
			return
		}
		seq, err := s.Append(r.Context(), &event.Event{
			SubjectID: id, Origin: event.OriginHuman, Type: eventItem, Payload: payload(eventItem, b.Text),
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"seq": seq, "subject_id": id})
	})
}

// channelTools advertises the one domain tool — reply — to the agent's MCP
// channel. The handler round-trips to the daemon via opReply because the channel
// server is a separate stdio process and cannot Append directly.
func channelTools(ctx context.Context, session, scope string) ([]channel.Tool, string, string, error) {
	client := newClient()
	pid := os.Getpid()
	reply := channel.Tool{
		Name:        "reply",
		Description: "Reply to an echo item; appends an echo.reply event (OriginAgent) to the subject's log.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"subject": map[string]any{"type": "string", "description": "subject id or slug"},
				"text":    map[string]any{"type": "string", "description": "reply text"},
			},
			"required": []string{"subject", "text"},
		},
		Handler: func(ctx context.Context, args json.RawMessage, _ func(string)) (string, bool) {
			r, err := client.Do(ctx, daemon.Envelope{
				Op: opReply, Session: session, ClaudePID: pid, Scope: scope, Body: args,
			})
			if err != nil {
				return "reply failed: " + err.Error(), true
			}
			if !r.OK {
				return r.Error, true
			}
			return string(r.Body), false
		},
	}
	return []channel.Tool{reply}, notifyMethod, "", nil
}

func deps() cmd.Deps {
	return cmd.Deps{
		Paths:                  appPaths(),
		Version:                appVersion,
		NewClient:              newClient,
		EnsureCurrent:          func(context.Context) error { return launcher().EnsureCurrent(daemon.UpgradeTimeout) },
		EnsureCurrentIfRunning: func() error { return launcher().EnsureCurrentIfRunning() },
		ClaudePID:              os.Getpid,
		TerminalEvent:          func(t string) bool { return t == eventDone },
		Serve:                  func(ctx context.Context) error { return serve(ctx) },
		ChannelTools:           channelTools,
	}
}

func serve(ctx context.Context) error {
	s, err := buildServer()
	if err != nil {
		return err
	}
	return s.Serve(ctx)
}

// startCmd creates or resumes the scope's subject via opStart and prints its id,
// slug, and the HTTP port the REST/SSE plane is on.
func startCmd(d cmd.Deps) *cobra.Command {
	var session, cwd string
	c := &cobra.Command{
		Use:   "start",
		Short: "Create or resume the echo subject for this scope",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			ctx := c.Context()
			if err := d.EnsureCurrent(ctx); err != nil {
				return err
			}
			scope := cwdOr(cwd)
			reply, err := d.NewClient().Do(ctx, daemon.Envelope{
				Op: opStart, Session: session, ClaudePID: d.ClaudePID(), Scope: scope,
			})
			if err != nil {
				return err
			}
			if !reply.OK {
				return errors.New(reply.Error)
			}
			out := c.OutOrStdout()
			fmt.Fprintf(out, "subject: %s\n", reply.SubjectID)
			fmt.Fprintf(out, "slug:    %s\n", slugFor(scope))
			fmt.Fprintf(out, "http:    127.0.0.1:%d\n", reply.HTTPPort)
			return nil
		},
	}
	c.Flags().StringVar(&session, "session", defaultSession, "session id (the cross-process ownership key)")
	c.Flags().StringVar(&cwd, "cwd", "", "working directory / scope (defaults to the current directory)")
	return c
}

// itemCmd POSTs a human item to the daemon's REST plane, resolving the subject
// and HTTP port through the core resolve op first. It is a convenience over a raw
// curl to POST /api/items.
func itemCmd(d cmd.Deps) *cobra.Command {
	var session, cwd string
	c := &cobra.Command{
		Use:   "item <text>",
		Short: "Post a human item to the subject over REST",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			ctx := c.Context()
			if err := d.EnsureCurrent(ctx); err != nil {
				return err
			}
			reply, err := d.NewClient().Do(ctx, daemon.Envelope{
				Op: daemon.OpResolve, Session: session, ClaudePID: d.ClaudePID(), Scope: cwdOr(cwd), Consumer: "item",
			})
			if err != nil {
				return err
			}
			if reply.SubjectID == "" {
				return errors.New("no subject for this scope; run `echo start` first")
			}
			body, _ := json.Marshal(itemBody{Subject: reply.SubjectID, Text: args[0]})
			resp, err := http.Post(
				fmt.Sprintf("http://127.0.0.1:%d/api/items", reply.HTTPPort),
				"application/json", bytes.NewReader(body))
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				msg, _ := io.ReadAll(resp.Body)
				return fmt.Errorf("POST /api/items: %s: %s", resp.Status, msg)
			}
			_, err = io.Copy(c.OutOrStdout(), resp.Body)
			return err
		},
	}
	c.Flags().StringVar(&session, "session", defaultSession, "session id")
	c.Flags().StringVar(&cwd, "cwd", "", "working directory / scope (defaults to the current directory)")
	return c
}

// withSessionDefault re-defaults a framework command's --session flag to the
// echo session, so the headless multi-process demo resolves without passing
// --session on every command.
func withSessionDefault(c *cobra.Command) *cobra.Command {
	if f := c.Flags().Lookup("session"); f != nil {
		_ = f.Value.Set(defaultSession)
		f.DefValue = defaultSession
	}
	return c
}

func root() *cobra.Command {
	d := deps()
	r := &cobra.Command{
		Use:           appName,
		Short:         "Headless cc-interact echo example",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	r.AddCommand(
		cmd.DaemonCmd(d),
		withSessionDefault(cmd.WatchCmd(d)),
		withSessionDefault(cmd.StatusCmd(d)),
		cmd.StopCmd(d),
		cmd.SessionRecordCmd(d),
		cmd.GuardEditCmd(d),
		withSessionDefault(cmd.ChannelAckCmd(d)),
		withSessionDefault(cmd.ChannelCmd(d)),
		startCmd(d),
		itemCmd(d),
	)
	return r
}

func main() {
	if err := root().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

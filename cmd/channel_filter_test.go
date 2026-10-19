package cmd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/cc-interact/channel"
	"github.com/yasyf/cc-interact/daemon"
	"github.com/yasyf/cc-interact/internal/testhome"
)

func TestChannelEventFilter(t *testing.T) {
	for _, tc := range []struct {
		name        string
		filter      func(string) bool
		wantStarted bool
	}{
		{"nil pushes every type", nil, true},
		{"filter drops unadmitted types", func(typ string) bool { return typ == "comment.created" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testhome.Isolate(t)

			sse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprint(w, "id: 1\ndata: {\"type\":\"agent.started\",\"agent_id\":\"a1\"}\n\n")
				_, _ = fmt.Fprint(w, "id: 2\ndata: {\"type\":\"comment.created\",\"id\":\"c1\"}\n\n")
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				<-r.Context().Done()
			}))
			t.Cleanup(sse.Close)

			socket, _ := fakeDaemon(t, func(daemon.Envelope) daemon.Reply {
				return daemon.Reply{OK: true, SubjectID: "sub-1", HTTPPort: mustPort(t, sse)}
			})
			d := testDeps(socket)
			d.WindowAlive = func(int) bool { return true }
			d.ChannelEvent = tc.filter
			d.ChannelTools = func(context.Context, string, string) ([]channel.Tool, string, string, error) {
				return []channel.Tool{{Name: "noop", InputSchema: map[string]any{}}}, "notifications/test/channel", "", nil
			}

			inR, inW := io.Pipe()
			out := &safeBuffer{}
			cmd := ChannelCmd(d)
			cmd.SetIn(inR)
			cmd.SetOut(out)
			cmd.SetErr(&bytes.Buffer{})

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- cmd.ExecuteContext(ctx) }()

			if _, err := io.WriteString(inW, `{"jsonrpc":"2.0","id":1,"method":"ping"}`+"\n"); err != nil {
				t.Fatal(err)
			}

			deadline := time.After(5 * time.Second)
			for !strings.Contains(out.String(), "comment.created") {
				select {
				case <-deadline:
					t.Fatalf("admitted event not pushed before deadline; stdout = %q", out.String())
				case <-time.After(10 * time.Millisecond):
				}
			}
			if got := strings.Contains(out.String(), "agent.started"); got != tc.wantStarted {
				t.Fatalf("agent.started pushed = %v, want %v; stdout = %q", got, tc.wantStarted, out.String())
			}

			_ = inW.Close()
			cancel()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("channel command did not exit after stdin EOF")
			}
		})
	}
}

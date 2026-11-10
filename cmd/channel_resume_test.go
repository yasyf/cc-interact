package cmd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/cc-interact/channel"
	"github.com/yasyf/cc-interact/daemon"
	"github.com/yasyf/cc-interact/internal/statepath"
	"github.com/yasyf/cc-interact/internal/testhome"
)

func TestChannelResumesLiveOverAStaleCursor(t *testing.T) {
	for _, tc := range []struct {
		name        string
		backlog     int
		resumesLive bool
		wantPushed  int
	}{
		{"one backlogged event stays unpushed", 1, true, 1},
		{"a thousand backlogged events stay unpushed", 1000, true, 1},
		{"without the field the backlog replays", 1000, false, 1001},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testhome.Isolate(t)

			head := int64(tc.backlog)
			sse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				since, _ := strconv.ParseInt(r.Header.Get("Last-Event-ID"), 10, 64)
				for seq := since + 1; seq <= head; seq++ {
					_, _ = fmt.Fprintf(w, "id: %d\ndata: {\"type\":\"slack.message\",\"text\":\"backlog-%d\"}\n\n", seq, seq)
				}
				_, _ = fmt.Fprintf(w, "event: caught-up\ndata: {\"seq\":%d}\n\n", max(since, head))
				if r.URL.Query().Get("consumer") != "" {
					_, _ = fmt.Fprintf(w, "id: %d\ndata: {\"type\":\"slack.message\",\"text\":\"live\"}\n\n", head+1)
				}
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				<-r.Context().Done()
			}))
			t.Cleanup(sse.Close)

			socket, _ := fakeDaemon(t, func(daemon.Envelope) daemon.Reply {
				return daemon.Reply{OK: true, SubjectID: "stale-subject", HTTPPort: mustPort(t, sse)}
			})
			d := testDeps(socket)
			d.ChannelResumesLive = tc.resumesLive
			d.WindowAlive = func(int) bool { return true }
			d.ChannelTools = func(context.Context, string, string) ([]channel.Tool, string, string, error) {
				return []channel.Tool{{Name: "noop", InputSchema: map[string]any{}}}, "notifications/test/channel", "", nil
			}
			if err := statepath.EnsureSubjectDir(d.Paths, "stale-subject"); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(statepath.Cursor(d.Paths, "stale-subject", daemon.ChannelConsumer), []byte("0"), 0o600); err != nil {
				t.Fatal(err)
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

			deadline := time.After(10 * time.Second)
			for !strings.Contains(out.String(), `\"text\":\"live\"`) {
				select {
				case <-deadline:
					t.Fatalf("the event appended after attach never arrived; stdout = %.2000q", out.String())
				case <-time.After(10 * time.Millisecond):
				}
			}
			if got := strings.Count(out.String(), "notifications/test/channel"); got != tc.wantPushed {
				t.Fatalf("pushed %d frames, want %d", got, tc.wantPushed)
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

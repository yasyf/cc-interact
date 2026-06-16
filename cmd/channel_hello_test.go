package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yasyf/cc-interact/channel"
	"github.com/yasyf/cc-interact/daemon"
)

// safeBuffer is an io.Writer safe for the concurrent writes the channel server's
// Serve replies and the stream goroutine's Notify make to the same pipe.
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

// TestChannelPushesHelloAtAttach proves the eager channel.hello tag is pushed at
// attach — before any subject event — so a consumer can prove the channel live
// even on a subject that produces no traffic.
func TestChannelPushesHelloAtAttach(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	sse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done() // hold the stream open with no events
	}))
	t.Cleanup(sse.Close)

	socket, _ := fakeDaemon(t, func(daemon.Envelope) daemon.Reply {
		return daemon.Reply{OK: true, SubjectID: "sub-1", HTTPPort: mustPort(t, sse)}
	})
	d := testDeps(socket)
	d.WindowAlive = func(int) bool { return true }
	d.ChannelTools = func(context.Context, string, string) ([]channel.Tool, string, error) {
		return []channel.Tool{{Name: "noop", InputSchema: map[string]any{}}}, "notifications/test/channel", nil
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

	// One request keeps Serve's output bound while the stream goroutine attaches.
	if _, err := io.WriteString(inW, `{"jsonrpc":"2.0","id":1,"method":"ping"}`+"\n"); err != nil {
		t.Fatal(err)
	}

	deadline := time.After(5 * time.Second)
	for !strings.Contains(out.String(), "channel.hello") {
		select {
		case <-deadline:
			t.Fatalf("hello not pushed before deadline; stdout = %q", out.String())
		case <-time.After(10 * time.Millisecond):
		}
	}

	var hello map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if strings.Contains(line, "channel.hello") {
			if err := json.Unmarshal([]byte(line), &hello); err != nil {
				t.Fatalf("hello line is not JSON: %v (%q)", err, line)
			}
			break
		}
	}
	if hello["method"] != "notifications/test/channel" {
		t.Fatalf("hello method = %v, want notifications/test/channel", hello["method"])
	}
	params, _ := hello["params"].(map[string]any)
	meta, _ := params["meta"].(map[string]any)
	if meta["type"] != "channel.hello" || meta["subject_id"] != "sub-1" {
		t.Fatalf("hello meta = %v, want {type: channel.hello, subject_id: sub-1}", meta)
	}

	_ = inW.Close() // EOF ends Serve
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("channel command did not exit after stdin EOF")
	}
}

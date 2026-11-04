package cmd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yasyf/cc-interact/channel"
	"github.com/yasyf/cc-interact/daemon"
	"github.com/yasyf/cc-interact/event"
	"github.com/yasyf/cc-interact/internal/testhome"
)

func TestChannelFollowsItsWindowToTheResumedSubject(t *testing.T) {
	testhome.Isolate(t)

	sse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if strings.Contains(r.URL.String(), "boot-subject") {
			fmt.Fprintf(w, "data: {\"type\":%q}\n\n", event.TypeWindowMoved)
		} else {
			fmt.Fprint(w, "id: 1\ndata: {\"type\":\"slack.message\",\"text\":\"from the owner\"}\n\n")
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))
	t.Cleanup(sse.Close)

	var resolves atomic.Int32
	socket, _ := fakeDaemon(t, func(daemon.Envelope) daemon.Reply {
		subjectID := "resumed-subject"
		if resolves.Add(1) == 1 {
			subjectID = "boot-subject"
		}
		return daemon.Reply{OK: true, SubjectID: subjectID, HTTPPort: mustPort(t, sse)}
	})
	d := testDeps(socket)
	d.WindowAlive = func(int) bool { return true }
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
	for !strings.Contains(out.String(), "from the owner") {
		select {
		case <-deadline:
			t.Fatalf("the resumed subject's event never arrived; stdout = %q", out.String())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if strings.Contains(out.String(), event.TypeWindowMoved) {
		t.Fatalf("the move frame reached the model; stdout = %q", out.String())
	}
	if !strings.Contains(out.String(), "resumed-subject") {
		t.Fatalf("the event names the wrong subject; stdout = %q", out.String())
	}

	_ = inW.Close()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("channel command did not exit after stdin EOF")
	}
}

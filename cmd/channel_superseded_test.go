package cmd

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yasyf/cc-interact/channel"
	"github.com/yasyf/cc-interact/daemon"
	"github.com/yasyf/cc-interact/internal/testhome"
	"github.com/yasyf/cc-interact/procs"
)

func TestChannelExitsOnceALaterChannelHoldsItsWindow(t *testing.T) {
	testhome.Isolate(t)

	var sawStarted atomic.Value
	sse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawStarted.Store(r.URL.Query().Get("started"))
		http.Error(w, "superseded", http.StatusConflict)
	}))
	t.Cleanup(sse.Close)

	socket, _ := fakeDaemon(t, func(daemon.Envelope) daemon.Reply {
		return daemon.Reply{OK: true, SubjectID: "subject", HTTPPort: mustPort(t, sse)}
	})
	d := testDeps(socket)
	d.WindowAlive = func(int) bool { return true }
	d.ChannelTools = func(context.Context, string, string) ([]channel.Tool, string, string, error) {
		return []channel.Tool{{Name: "noop", InputSchema: map[string]any{}}}, "notifications/test/channel", "", nil
	}

	inR, inW := io.Pipe()
	t.Cleanup(func() { _ = inW.Close() })
	cmd := ChannelCmd(d)
	cmd.SetIn(inR)
	cmd.SetOut(&safeBuffer{})
	cmd.SetErr(&bytes.Buffer{})

	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a superseded channel server exited with %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a superseded channel server kept running with its stdin open")
	}

	start, err := procs.StartTime(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := sawStarted.Load(), strconv.FormatInt(start, 10); got != want {
		t.Fatalf("started param = %v, want the process start %s", got, want)
	}
}

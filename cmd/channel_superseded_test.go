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

func TestAToollessChannelExitsOnceAnOutrankingChannelHoldsItsWindow(t *testing.T) {
	testhome.Isolate(t)

	var saw atomic.Value
	sse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		saw.Store(r.URL.Query().Get("started") + "/" + r.URL.Query().Get("toolless"))
		http.Error(w, "superseded", http.StatusConflict)
	}))
	t.Cleanup(sse.Close)

	inW, done := runSupersededChannel(t, sse, "--no-tools")
	t.Cleanup(func() { _ = inW.Close() })
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a superseded toolless channel server exited with %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a superseded toolless channel server kept running with its stdin open")
	}

	start, err := procs.StartTime(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := saw.Load(), strconv.FormatInt(start, 10)+"/1"; got != want {
		t.Fatalf("started/toolless params = %v, want the process start and toolless %s", got, want)
	}
}

func TestAChannelServingToolsKeepsServingThemOnceOutranked(t *testing.T) {
	testhome.Isolate(t)

	var refused atomic.Bool
	sse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refused.Store(r.URL.Query().Get("toolless") == "")
		http.Error(w, "superseded", http.StatusConflict)
	}))
	t.Cleanup(sse.Close)

	inW, done := runSupersededChannel(t, sse)
	deadline := time.Now().Add(5 * time.Second)
	for !refused.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !refused.Load() {
		t.Fatal("the channel server never attached")
	}
	select {
	case err := <-done:
		t.Fatalf("an outranked channel server serving tools exited with %v while its stdin was open", err)
	case <-time.After(500 * time.Millisecond):
	}
	_ = inW.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("channel server exited with %v at stdin EOF, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("channel server kept running after stdin closed")
	}
}

func runSupersededChannel(t *testing.T, sse *httptest.Server, args ...string) (io.WriteCloser, <-chan error) {
	t.Helper()
	socket, _ := fakeDaemon(t, func(daemon.Envelope) daemon.Reply {
		return daemon.Reply{OK: true, SubjectID: "subject", HTTPPort: mustPort(t, sse)}
	})
	d := testDeps(socket)
	d.WindowAlive = func(int) bool { return true }
	d.ChannelTools = func(context.Context, string, string) ([]channel.Tool, string, string, error) {
		return []channel.Tool{{Name: "noop", InputSchema: map[string]any{}}}, "notifications/test/channel", "", nil
	}

	inR, inW := io.Pipe()
	cmd := ChannelCmd(d)
	cmd.SetArgs(append([]string{}, args...))
	cmd.SetIn(inR)
	cmd.SetOut(&safeBuffer{})
	cmd.SetErr(&bytes.Buffer{})

	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(context.Background()) }()
	return inW, done
}

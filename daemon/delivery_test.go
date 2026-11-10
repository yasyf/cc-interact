package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/yasyf/cc-interact/agent"
	"github.com/yasyf/cc-interact/consume"
	"github.com/yasyf/cc-interact/event"
	"github.com/yasyf/cc-interact/internal/statepath"
	"github.com/yasyf/cc-interact/procs"
	"github.com/yasyf/cc-interact/subject"
)

const deliveredType = "slack.message"

type channelFrame struct {
	seq  int64
	text string
}

func startChannel(t *testing.T, s *Server, sub subject.Subject) <-chan channelFrame {
	t.Helper()
	srv := httptest.NewServer(s.sse.Handler())
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	frames := make(chan channelFrame, 2048)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = consume.ConsumeEvents(ctx, consume.StreamSource{
			Port: port, SubjectID: sub.ID, Consumer: ChannelConsumer, ClaudePID: sub.ClaudePID,
			ExcludeOrigin: event.OriginAgent, Paths: s.paths,
		}, func(seq int64, data string) (bool, error) {
			var e struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if err := json.Unmarshal([]byte(data), &e); err != nil {
				return false, err
			}
			if e.Type == deliveredType {
				frames <- channelFrame{seq, e.Text}
			}
			return false, nil
		})
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		srv.Close()
	})
	waitFor(t, "the channel to attach", func() bool { return s.activity.Attached(sub.ID, ChannelConsumer, sub.ClaudePID) })
	return frames
}

func appendDirected(t *testing.T, s *Server, sub subject.Subject, text string, linked bool) int64 {
	t.Helper()
	ctx := context.Background()
	seq, err := s.Append(ctx, &event.Event{
		SubjectID: sub.ID, Origin: event.OriginHuman, Type: deliveredType,
		Payload: []byte(fmt.Sprintf(`{"type":%q,"text":%q}`, deliveredType, text)),
	})
	if err != nil {
		t.Fatalf("append %s: %v", text, err)
	}
	if linked {
		_, err = s.DirectEvent(ctx, sub.ID, event.OriginHuman, text, seq)
	} else {
		_, err = s.Direct(ctx, sub.ID, agent.TopLevel, event.OriginHuman, text)
	}
	if err != nil {
		t.Fatalf("direct %s: %v", text, err)
	}
	return seq
}

func hookDrain(t *testing.T, s *Server, sub subject.Subject) []string {
	t.Helper()
	return hookDrainFrom(t, s, agentEnvelope(OpAgentInject, sub, agentInjectBody{AgentID: agent.TopLevel}))
}

func hookDrainFrom(t *testing.T, s *Server, env Envelope) []string {
	t.Helper()
	r := s.dispatch(context.Background(), env)
	if !r.OK {
		t.Fatalf("inject = %+v, want ok", r)
	}
	var reply directivesReply
	if err := json.Unmarshal(r.Body, &reply); err != nil {
		t.Fatalf("inject body: %v", err)
	}
	texts := make([]string, len(reply.Directives))
	for i, d := range reply.Directives {
		texts[i] = d.Text
	}
	return texts
}

func prove(t *testing.T, s *Server, sub subject.Subject) {
	t.Helper()
	if r := s.dispatch(context.Background(), agentEnvelope(OpChannelAck, sub, struct{}{})); !r.OK {
		t.Fatalf("channel-ack = %+v, want ok", r)
	}
}

func waitPushed(t *testing.T, s *Server, sub subject.Subject, frames <-chan channelFrame, seq int64) channelFrame {
	t.Helper()
	var frame channelFrame
	select {
	case frame = <-frames:
	case <-time.After(time.Minute):
		t.Fatalf("the channel never pushed event %d", seq)
	}
	if frame.seq != seq {
		t.Fatalf("channel pushed %+v, want event %d", frame, seq)
	}
	waitFor(t, "the channel cursor to pass the event", func() bool {
		cursor, err := consume.Cursor(s.paths, sub.ID, ChannelConsumer)
		return err == nil && cursor >= seq
	})
	return frame
}

func assertNoFrame(t *testing.T, frames <-chan channelFrame) {
	t.Helper()
	select {
	case frame := <-frames:
		t.Fatalf("channel pushed %+v, want nothing", frame)
	default:
	}
}

func TestProvenChannelEventSkipsTheHook(t *testing.T) {
	s := newTestServer(t, Config{})
	sub := seedSubject(t, s, "id1", "slug1", "sess1", "scopeA", os.Getpid(), "open")
	frames := startChannel(t, s, sub)
	prove(t, s, sub)

	seq := appendDirected(t, s, sub, "hello", true)
	if frame := waitPushed(t, s, sub, frames, seq); frame.text != "hello" {
		t.Fatalf("pushed %+v, want hello", frame)
	}
	if got := hookDrain(t, s, sub); len(got) != 0 {
		t.Fatalf("hook delivered %v after the proven channel pushed it, want nothing", got)
	}
	assertNoFrame(t, frames)
}

func TestAHookRunOutsideTheWindowSkipsWhatItsChannelPushed(t *testing.T) {
	s := newTestServer(t, Config{})
	sub := seedSubject(t, s, "id1", "slug1", "sess1", "scopeA", os.Getpid(), "open")
	frames := startChannel(t, s, sub)
	prove(t, s, sub)

	seq := appendDirected(t, s, sub, "hello", true)
	waitPushed(t, s, sub, frames, seq)
	outside := agentEnvelope(OpAgentInject, sub, agentInjectBody{AgentID: agent.TopLevel})
	outside.ClaudePID = 0
	if got := hookDrainFrom(t, s, outside); len(got) != 0 {
		t.Fatalf("a hook with no claude pid delivered %v the proven channel pushed, want nothing", got)
	}
}

func writeProof(t *testing.T, s *Server, pid int, start int64) {
	t.Helper()
	if err := os.MkdirAll(statepath.WindowProofs(s.paths), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statepath.WindowProof(s.paths, pid), []byte(strconv.FormatInt(start, 10)), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestProofAnEarlierDaemonRecordedHolds(t *testing.T) {
	s := newTestServer(t, Config{})
	sub := seedSubject(t, s, "id1", "slug1", "sess1", "scopeA", os.Getpid(), "open")
	start, err := procs.StartTime(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	writeProof(t, s, sub.ClaudePID, start)

	frames := startChannel(t, s, sub)
	if got := s.ChannelState(HandlerCtx{Env: Envelope{ClaudePID: os.Getpid()}, Scope: "scopeA"}, sub.ID, ChannelConsumer); got != ChannelActive {
		t.Fatalf("channel state under an earlier daemon's proof = %q, want active with no new probe", got)
	}
	waitPushed(t, s, sub, frames, appendDirected(t, s, sub, "hello", true))
	if got := hookDrain(t, s, sub); len(got) != 0 {
		t.Fatalf("hook delivered %v under an earlier daemon's proof, want nothing", got)
	}
}

func TestARecycledPidInheritsNoProof(t *testing.T) {
	s := newTestServer(t, Config{})
	sub := seedSubject(t, s, "id1", "slug1", "sess1", "scopeA", os.Getpid(), "open")
	start, err := procs.StartTime(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	writeProof(t, s, sub.ClaudePID, start-1)

	frames := startChannel(t, s, sub)
	waitPushed(t, s, sub, frames, appendDirected(t, s, sub, "hello", true))
	if got := hookDrain(t, s, sub); len(got) != 1 || got[0] != "hello" {
		t.Fatalf("hook delivered %v under another process's proof, want [hello]", got)
	}
}

func TestAnotherWindowsChannelOnTheSubjectStopsTheSkip(t *testing.T) {
	s := newTestServer(t, Config{})
	sub := seedSubject(t, s, "id1", "slug1", "sess1", "scopeA", os.Getpid(), "open")
	frames := startChannel(t, s, sub)
	prove(t, s, sub)
	other := sub
	other.ClaudePID = os.Getppid()
	startChannel(t, s, other)

	waitPushed(t, s, sub, frames, appendDirected(t, s, sub, "hello", true))
	if got := hookDrain(t, s, sub); len(got) != 1 || got[0] != "hello" {
		t.Fatalf("hook delivered %v with a second window's channel sharing the cursor, want [hello]", got)
	}
}

func TestHookDeliveredBacklogNeverReachesALaterChannel(t *testing.T) {
	for _, backlog := range []int{1, 1000} {
		t.Run(strconv.Itoa(backlog), func(t *testing.T) {
			s := newTestServer(t, Config{})
			sub := seedSubject(t, s, "id1", "slug1", "sess1", "scopeA", os.Getpid(), "open")
			for i := range backlog {
				appendDirected(t, s, sub, fmt.Sprintf("backlog-%d", i), true)
			}
			if got := hookDrain(t, s, sub); len(got) != backlog {
				t.Fatalf("hook delivered %d directives with no channel attached, want %d", len(got), backlog)
			}

			frames := startChannel(t, s, sub)
			prove(t, s, sub)
			seq := appendDirected(t, s, sub, "live", true)
			if frame := waitPushed(t, s, sub, frames, seq); frame.text != "live" {
				t.Fatalf("first push %+v, want the event appended after attach", frame)
			}
			assertNoFrame(t, frames)
			if got := hookDrain(t, s, sub); len(got) != 0 {
				t.Fatalf("hook delivered %v the channel pushed, want nothing", got)
			}
		})
	}
}

func TestHookStillDeliversWhatTheChannelDidNotProvablyPush(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prove   bool
		resumed bool
		linked  bool
	}{
		{name: "attached but unproven", linked: true},
		{name: "proven but resumed past the event", prove: true, resumed: true, linked: true},
		{name: "proven with no event link", prove: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t, Config{})
			sub := seedSubject(t, s, "id1", "slug1", "sess1", "scopeA", os.Getpid(), "open")
			if tc.resumed {
				seq := appendDirected(t, s, sub, "hello", tc.linked)
				if err := consume.RaiseCursor(s.paths, sub.ID, ChannelConsumer, seq); err != nil {
					t.Fatal(err)
				}
				startChannel(t, s, sub)
			} else {
				frames := startChannel(t, s, sub)
				waitPushed(t, s, sub, frames, appendDirected(t, s, sub, "hello", tc.linked))
			}
			if tc.prove {
				prove(t, s, sub)
			}
			if got := hookDrain(t, s, sub); len(got) != 1 || got[0] != "hello" {
				t.Fatalf("hook delivered %v, want [hello]", got)
			}
		})
	}
}

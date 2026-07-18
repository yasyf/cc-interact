package daemon

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"strings"
	"testing"
)

// TestReplyWireBytesFrozen proves the daemonkit wire.Framing swap left the response
// frame byte-identical to the pre-daemonkit json.Encoder framing — the wire
// protocol is a compatibility contract with already-deployed clients. It also locks
// that markup is HTML-escaped, never written to the wire raw.
func TestReplyWireBytesFrozen(t *testing.T) {
	r := Reply{
		Proto:         ProtocolVersion,
		OK:            true,
		Error:         "a<b>&c",
		DaemonVersion: "v1.2.3",
		SubjectID:     "s1",
		Status:        "open",
		HTTPPort:      4321,
		Allow:         true,
		Reason:        "x",
		Body:          json.RawMessage(`{"k":1}`),
	}

	var ref bytes.Buffer
	if err := json.NewEncoder(&ref).Encode(r); err != nil {
		t.Fatal(err)
	}

	client, server := net.Pipe()
	go func() {
		(&Server{}).writeReply(server, r)
		_ = server.Close()
	}()
	got, err := io.ReadAll(client)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != ref.String() {
		t.Fatalf("framing swap changed the reply bytes:\n got: %q\nref: %q", got, ref.String())
	}
	if strings.Contains(string(got), "a<b>") {
		t.Fatalf("reply leaked raw HTML markup instead of escaping it: %q", got)
	}
}

package channel

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/yasyf/cc-interact/consume"
	"github.com/yasyf/cc-interact/event"
)

// ErrWindowMoved ends a stream whose subject was its window's boot subject: the
// caller resolves the window's subject again and streams that one.
var ErrWindowMoved = errors.New("the window's session moved to another subject")

// StreamEvents consumes a subject's event log from the daemon and turns each
// frame into a notification: it parses the `type` field out of the JSON payload
// and calls notify(eventType, data). A notify error propagates so the consumer's
// cursor does not advance past an undelivered event (at-least-once). It returns
// when consumption ends — ctx cancelled or a fatal stream error. The
// notification method name and envelope stay with the caller, which typically
// wires notify to push through (*Server).Notify. A window-moved frame ends the
// stream with ErrWindowMoved and is never passed to notify.
func StreamEvents(ctx context.Context, src consume.StreamSource, notify func(eventType, data string) error) error {
	moved := false
	err := consume.ConsumeEvents(ctx, src, func(_ int64, data string) (bool, error) {
		typ := eventType(data)
		if typ == event.TypeWindowMoved {
			moved = true
			return true, nil
		}
		return false, notify(typ, data)
	})
	if moved {
		return ErrWindowMoved
	}
	return err
}

func eventType(data string) string {
	var e struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal([]byte(data), &e)
	return e.Type
}

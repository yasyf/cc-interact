package channel

import (
	"context"
	"encoding/json"

	"github.com/yasyf/cc-interact/consume"
)

// StreamEvents consumes a subject's event log from the daemon and turns each
// frame into a notification: it parses the `type` field out of the JSON payload
// and calls notify(eventType, data). A notify error propagates so the consumer's
// cursor does not advance past an undelivered event (at-least-once). It returns
// when consumption ends — ctx cancelled or a fatal stream error. The
// notification method name and envelope stay with the caller, which typically
// wires notify to push through (*Server).Notify.
func StreamEvents(ctx context.Context, src consume.StreamSource, notify func(eventType, data string) error) error {
	return consume.ConsumeEvents(ctx, src, func(_ int64, data string) (bool, error) {
		return false, notify(eventType(data), data)
	})
}

func eventType(data string) string {
	var e struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal([]byte(data), &e)
	return e.Type
}

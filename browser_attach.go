package rod

import (
	"context"
	"encoding/json"
	"time"

	"github.com/go-rod/rod/lib/proto"
)

func (b *Browser) resumeAttached(params json.RawMessage) {
	var ev struct {
		SessionID          string `json:"sessionId"`
		WaitingForDebugger bool   `json:"waitingForDebugger"`
	}
	if err := json.Unmarshal(params, &ev); err != nil || !ev.WaitingForDebugger || ev.SessionID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(b.ctx, 5*time.Second)
	defer cancel()
	_, _ = b.Call(ctx, ev.SessionID, "Runtime.runIfWaitingForDebugger", map[string]any{})
}

// framePage returns a page bound to the out-of-process iframe whose target
// id is targetID, attaching to it when needed. It returns nil when the frame
// is not a separate target, which is the case for same-site frames.
func (b *Browser) framePage(targetID proto.TargetTargetID) (*Page, error) {
	info, err := b.pageInfo(targetID)
	if err != nil || info.Type != proto.TargetTargetInfoType("iframe") {
		return nil, nil //nolint: nilerr
	}

	return b.pageFromTarget(targetID, false)
}

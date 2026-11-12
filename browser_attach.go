package rod

import (
	"context"
	"encoding/json"
	"sync"
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

	b.targetsLock.Lock()
	defer b.targetsLock.Unlock()

	if page := b.loadCachedPage(targetID); page != nil {
		return page, nil
	}

	session, err := proto.TargetAttachToTarget{TargetID: targetID, Flatten: true}.Call(b)
	if err != nil {
		return nil, err
	}

	sessionCtx, cancel, cancelCause := b.sessionContext()
	page := &Page{
		e:             b.e,
		ctx:           sessionCtx,
		session:       sessionCtx,
		sessionCancel: cancel,
		cancelCause:   cancelCause,
		sleeper:       b.sleeper,
		browser:       b,
		TargetID:      targetID,
		SessionID:     session.SessionID,
		FrameID:       proto.PageFrameID(targetID),
		jsCtxLock:     &sync.Mutex{},
		jsCtxID:       new(proto.RuntimeRemoteObjectID),
		helpersLock:   &sync.Mutex{},
	}
	page.root = page
	page.newKeyboard().newMouse().newTouch()

	b.cachePage(page)
	page.EnableDomain(&proto.PageEnable{})
	return page, nil
}

package rod

import (
	"context"
	"errors"
	"reflect"

	"github.com/go-rod/rod/lib/cdp"
	"github.com/go-rod/rod/lib/proto"
)

// ErrTargetClosed is matched by errors caused by the page's target being
// closed, destroyed, or detached. Such errors also match [context.Canceled].
var ErrTargetClosed = errors.New("rod: target closed")

type targetClosedError struct{ cause error }

func (e *targetClosedError) Error() string {
	if e.cause == nil || errors.Is(e.cause, ErrTargetClosed) {
		return ErrTargetClosed.Error()
	}
	return ErrTargetClosed.Error() + ": " + e.cause.Error()
}

func (e *targetClosedError) Unwrap() []error {
	return []error{ErrTargetClosed, context.Canceled, e.cause}
}

// sessionContext returns the lifetime context of a target session. cancel
// records ErrTargetClosed as the cause; cancelCause lets the browser record
// a connection loss instead.
func (b *Browser) sessionContext() (context.Context, func(), context.CancelCauseFunc) {
	ctx, cancel := context.WithCancelCause(b.ctx)
	ctx = context.WithValue(ctx, sessionCtxKey{}, new(byte))
	return ctx, func() { cancel(ErrTargetClosed) }, cancel
}

// Done is closed when the page's target session ends.
func (p *Page) Done() <-chan struct{} {
	if p.session == nil {
		return p.ctx.Done()
	}
	return p.session.Done()
}

// Err returns nil while the target session is alive. Afterwards it returns
// an error matching [ErrTargetClosed], or [cdp.ErrConnClosed] when the
// browser connection was lost.
func (p *Page) Err() error {
	if p.session == nil {
		return p.ctx.Err()
	}
	if p.session.Err() == nil {
		return nil
	}
	return sessionErr(p.session)
}

func sessionErr(ctx context.Context) error {
	cause := context.Cause(ctx)
	if errors.Is(cause, cdp.ErrConnClosed) {
		return cause
	}
	if errors.Is(cause, ErrTargetClosed) {
		return &targetClosedError{}
	}
	return cause
}

// translate turns a cancellation caused by the session ending into a typed
// error that still matches [context.Canceled].
func (p *Page) translate(err error) error {
	if err == nil || p.session == nil || !errors.Is(err, context.Canceled) || p.session.Err() == nil {
		return err
	}
	if errors.Is(err, ErrTargetClosed) || errors.Is(err, cdp.ErrConnClosed) {
		return err
	}
	return sessionErr(p.session)
}

// Disconnected is closed when the CDP connection ends. It is nil for custom
// clients that cannot report it.
func (b *Browser) Disconnected() <-chan struct{} {
	if c, ok := b.client.(interface{ Done() <-chan struct{} }); ok {
		return c.Done()
	}
	return nil
}

// Err returns why the CDP connection ended, or nil while it is alive.
func (b *Browser) Err() error {
	if c, ok := b.client.(interface{ Err() error }); ok {
		return c.Err()
	}
	return nil
}

// cancelPages ends every cached page session with cause.
func (b *Browser) cancelPages(cause error) {
	b.states.Range(func(k, v interface{}) bool {
		if _, ok := k.(sessionPageKey); !ok {
			return true
		}
		if page, ok := v.(*Page); ok && page.cancelCause != nil {
			page.cancelCause(cause)
		}
		return true
	})
}

type sessionCtxKey struct{}

// lifetime returns a context that ends with p.ctx or with the target session,
// whichever is first, so page subscriptions end when the page goes away even
// on clones bound to an unrelated context. release frees the link early.
func (p *Page) lifetime() (ctx context.Context, release func()) {
	if p.session == nil || p.ctx.Value(sessionCtxKey{}) == p.session.Value(sessionCtxKey{}) {
		return p.ctx, func() {}
	}
	merged, cancel := context.WithCancelCause(p.ctx)
	stop := context.AfterFunc(p.session, func() { cancel(sessionErr(p.session)) })
	return merged, func() {
		stop()
		cancel(nil)
	}
}

// mergeContext returns a context that ends with either ctx or the page session.
func (p *Page) mergeContext(ctx context.Context) (context.Context, func()) {
	merged, cancel := context.WithCancelCause(ctx)
	stop := context.AfterFunc(p.ctx, func() {
		if p.session != nil && p.session.Err() != nil {
			cancel(sessionErr(p.session))
			return
		}
		cancel(context.Cause(p.ctx))
	})
	return merged, func() {
		stop()
		cancel(nil)
	}
}

func waitResult(ctx context.Context) error {
	if ctx.Err() == nil {
		return ErrTargetClosed
	}
	return context.Cause(ctx)
}

// EachEventContext is [Page.EachEvent] bound to ctx and to the page session.
// wait returns nil when a callback returned true, otherwise why it ended.
func (p *Page) EachEventContext(ctx context.Context, callbacks ...interface{}) (wait func() error) {
	merged, cancel := p.mergeContext(ctx)
	w := p.browser.Context(merged).eachEventStop(p.SessionID, callbacks...)
	return func() error {
		defer cancel()
		if w() {
			return nil
		}
		return waitResult(merged)
	}
}

// WaitEventContext is [Page.WaitEvent] bound to ctx and to the page session.
// wait returns nil once e has been loaded, otherwise why it ended.
func (p *Page) WaitEventContext(ctx context.Context, e proto.Event) (wait func() error) {
	return p.EachEventContext(ctx, eventLoader(e))
}

// HandleDialogContext is [Page.HandleDialog] bound to ctx and to the page session.
func (p *Page) HandleDialogContext(ctx context.Context) (
	wait func() (*proto.PageJavascriptDialogOpening, error),
	handle func(*proto.PageHandleJavaScriptDialog) error,
) {
	restore := p.EnableDomain(&proto.PageEnable{})
	var e proto.PageJavascriptDialogOpening
	w := p.WaitEventContext(ctx, &e)
	return func() (*proto.PageJavascriptDialogOpening, error) {
			if err := w(); err != nil {
				restore()
				return nil, err
			}
			return &e, nil
		}, func(h *proto.PageHandleJavaScriptDialog) error {
			defer restore()
			return h.Call(p.Context(ctx))
		}
}

// AutoDialog answers every JavaScript dialog on the page until stop is
// called or the page closes, so alert, confirm, prompt, and beforeunload
// never block the page.
func (p *Page) AutoDialog(accept bool, promptText string) (stop func()) {
	ctx, cancel := context.WithCancel(p.ctx)
	pc := p.Context(ctx)
	restore := p.EnableDomain(&proto.PageEnable{})
	wait := pc.EachEvent(func(e *proto.PageJavascriptDialogOpening) {
		_ = proto.PageHandleJavaScriptDialog{Accept: accept, PromptText: promptText}.Call(pc)
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		wait()
	}()
	return func() {
		cancel()
		<-done
		restore()
	}
}

// eventLoader builds a callback that copies the first event into e and stops.
func eventLoader(e proto.Event) interface{} {
	valE := reflect.ValueOf(e)
	valTrue := reflect.ValueOf(true)

	if valE.Kind() != reflect.Ptr {
		valE = reflect.New(valE.Type())
	}

	fnType := reflect.FuncOf([]reflect.Type{valE.Type()}, []reflect.Type{valTrue.Type()}, false)
	fnVal := reflect.MakeFunc(fnType, func(args []reflect.Value) []reflect.Value {
		valE.Elem().Set(args[0].Elem())
		return []reflect.Value{valTrue}
	})
	return fnVal.Interface()
}

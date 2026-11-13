// Package cdp for application layer communication with browser.
package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"sync"
	"sync/atomic"

	"github.com/go-rod/rod/lib/defaults"
	"github.com/go-rod/rod/lib/utils"
)

// Request to send to browser.
type Request struct {
	ID        int    `json:"id"`
	SessionID string `json:"sessionId,omitempty"`
	Method    string `json:"method"`
	Params    any    `json:"params,omitempty"`
}

// Response from browser.
type Response struct {
	ID     int             `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *Error          `json:"error,omitempty"`
}

// Event from browser.
type Event struct {
	SessionID string          `json:"sessionId,omitempty"`
	Method    string          `json:"method"`
	Params    json.RawMessage `json:"params,omitempty"`
}

// incoming is one decode of a CDP message. Result and Params stay raw
// so the typed decode happens once, on the consumer that needs it.
type incoming struct {
	ID        int             `json:"id"`
	SessionID string          `json:"sessionId,omitempty"`
	Method    string          `json:"method,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *Error          `json:"error,omitempty"`
}

// WebSocketable enables you to choose the websocket lib you want to use.
type WebSocketable interface {
	Send(data []byte) error
	Read() ([]byte, error)
}

const pendingShards = 16

type pendingShard struct {
	mu sync.Mutex
	m  map[int]chan result
}

// Client is a devtools protocol connection instance.
type Client struct {
	count atomic.Uint64

	ws WebSocketable

	pending [pendingShards]pendingShard
	event   chan *Event

	logger utils.Logger

	closeOnce sync.Once
	closeErr  error

	deadOnce sync.Once
	dead     chan struct{}
	cause    error

	// writes is the single frame writer. Call enqueues and can return
	// while the frame is still queued, so a cancelled caller does not
	// block on the socket and the frame is still written whole.
	writes chan writeJob

	// OnDecodeError is called when a frame is not valid JSON.
	// The connection stays up. Nil logs and continues.
	OnDecodeError func(error)
}

// New creates a cdp connection. Event is buffered so a slow consumer does
// not stall command responses until the buffer fills.
func New() *Client {
	cdp := &Client{
		event:  make(chan *Event, 8192),
		logger: defaults.CDP,
		dead:   make(chan struct{}),
	}
	for i := range cdp.pending {
		cdp.pending[i].m = map[int]chan result{}
	}
	return cdp
}

// Logger sets the logger.
func (cdp *Client) Logger(l utils.Logger) *Client {
	cdp.logger = l
	return cdp
}

// Start to browser.
func (cdp *Client) Start(ws WebSocketable) *Client {
	cdp.ws = ws
	cdp.writes = make(chan writeJob, 64)
	go cdp.writer()
	go cdp.consumeMessages()
	return cdp
}

type writeJob struct {
	data []byte
	done chan error
}

// Close the underlying transport. Pending and later calls fail with an error
// matching [ErrConnClosed]. Safe to call more than once.
func (cdp *Client) Close() error {
	cdp.closeOnce.Do(func() {
		cdp.fail(ErrConnClosed)
		if c, ok := cdp.ws.(io.Closer); ok && c != nil {
			cdp.closeErr = c.Close()
		}
	})
	return cdp.closeErr
}

// Done is closed when the connection ends, by Close or by a transport error.
func (cdp *Client) Done() <-chan struct{} {
	return cdp.dead
}

// Err returns why the connection ended, or nil while it is alive.
// The error matches [ErrConnClosed] and wraps the transport error.
func (cdp *Client) Err() error {
	select {
	case <-cdp.dead:
		return cdp.cause
	default:
		return nil
	}
}

type result struct {
	msg json.RawMessage
	err error
}

// Call a method and wait for its response.
func (cdp *Client) Call(ctx context.Context, sessionID, method string, params any) ([]byte, error) {
	req := &Request{
		ID:        int(cdp.count.Add(1)),
		SessionID: sessionID,
		Method:    method,
		Params:    params,
	}

	cdp.logger.Println(req)

	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	done := make(chan result, 1)
	sh := cdp.shard(req.ID)
	sh.mu.Lock()
	sh.m[req.ID] = done
	sh.mu.Unlock()
	defer func() {
		sh.mu.Lock()
		delete(sh.m, req.ID)
		sh.mu.Unlock()
	}()

	err = cdp.Err()
	if err != nil {
		return nil, err
	}

	job, err := cdp.queue(ctx, data)
	if err != nil {
		return nil, err
	}
	finished, msg, err := cdp.awaitWrite(ctx, job.done, done)
	if finished {
		return msg, err
	}
	return cdp.awaitResponse(ctx, done)
}

// Event returns a channel of browser events. The channel is buffered.
// It is closed when the connection ends.
func (cdp *Client) Event() <-chan *Event {
	return cdp.event
}

// deadOutcome is the result of a call whose connection died. A response that
// arrived first still wins over the cause.
func (cdp *Client) deadOutcome(done <-chan result) ([]byte, error) {
	select {
	case res := <-done:
		return res.msg, res.err
	default:
		return nil, cdp.cause
	}
}

// awaitResponse waits for the response to a sent frame.
func (cdp *Client) awaitResponse(ctx context.Context, done <-chan result) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-done:
		return res.msg, res.err
	case <-cdp.dead:
		return cdp.deadOutcome(done)
	}
}

// awaitWrite waits until the writer sends the frame. finished reports that the
// call is over, with msg and err as its result. Otherwise the caller goes on to
// wait for the response.
func (cdp *Client) awaitWrite(
	ctx context.Context, written <-chan error, done <-chan result,
) (finished bool, msg []byte, err error) {
	select {
	case werr := <-written:
		if werr == nil {
			return false, nil, nil
		}
		cause := cdp.Err()
		if cause != nil {
			return true, nil, cause
		}
		return true, nil, werr
	case <-ctx.Done():
		return true, nil, ctx.Err()
	case <-cdp.dead:
		msg, err = cdp.deadOutcome(done)
		return true, msg, err
	}
}

// queue hands the frame to the writer. It runs even when ctx is already done:
// callers rely on cleanup calls such as domain disables reaching the browser.
// The writer, not the caller, blocks on the socket, and only for the
// transport's own write timeout.
func (cdp *Client) queue(ctx context.Context, data []byte) (writeJob, error) {
	job := writeJob{data: data, done: make(chan error, 1)}
	select {
	case <-cdp.dead:
		return job, cdp.cause
	case cdp.writes <- job:
	}
	return job, ctx.Err()
}

func (cdp *Client) shard(id int) *pendingShard {
	return &cdp.pending[uint(id)%pendingShards]
}

func (cdp *Client) writer() {
	for {
		select {
		case <-cdp.dead:
			return
		case job := <-cdp.writes:
			err := cdp.ws.Send(job.data)
			select {
			case job.done <- err:
			default:
			}
		}
	}
}

func (cdp *Client) consumeMessages() {
	defer close(cdp.event)

	for {
		data, err := cdp.ws.Read()
		if err != nil {
			cdp.fail(err)
			return
		}

		msg, err := decodeIncoming(data)
		if err != nil {
			cdp.decodeError(err)
			continue
		}

		if msg.ID == 0 {
			evt := &Event{SessionID: msg.SessionID, Method: msg.Method, Params: msg.Params}
			cdp.logger.Println(evt)
			select {
			case cdp.event <- evt:
			case <-cdp.dead:
				return
			}
			continue
		}

		cdp.logger.Println(&Response{ID: msg.ID, Result: msg.Result, Error: msg.Error})

		sh := cdp.shard(msg.ID)
		sh.mu.Lock()
		done, ok := sh.m[msg.ID]
		sh.mu.Unlock()
		if !ok {
			continue
		}
		res := result{msg: msg.Result}
		if msg.Error != nil {
			res = result{err: msg.Error}
		}
		select {
		case done <- res:
		default:
		}
	}
}

// fail records why the connection ended and wakes every pending call.
func (cdp *Client) fail(err error) {
	cdp.deadOnce.Do(func() {
		if !errors.Is(err, ErrConnClosed) {
			err = &ConnError{Err: err}
		}
		cdp.cause = err
		close(cdp.dead)
	})
}

func (cdp *Client) decodeError(err error) {
	if cdp.OnDecodeError != nil {
		cdp.OnDecodeError(err)
		return
	}
	log.Printf("[cdp] dropped undecodable message: %v", err)
}

// ErrConnClosed is matched by every error returned after the connection ends.
var ErrConnClosed = errors.New("cdp: connection closed")

// ConnError is returned for calls made after the transport failed.
// It matches [ErrConnClosed] and unwraps to the transport error.
type ConnError struct {
	Err error
}

func (e *ConnError) Error() string {
	return fmt.Sprintf("cdp: connection closed: %v", e.Err)
}

// Unwrap returns ErrConnClosed and the transport error.
func (e *ConnError) Unwrap() []error {
	return []error{ErrConnClosed, e.Err}
}

package cdp

import (
	"bufio"
	"errors"
	"os"
	"sync"
	"sync/atomic"
)

// ErrPipeClosed is returned when a pipe transport is used after Close.
var ErrPipeClosed = errors.New("cdp: pipe closed")

// Pipe is a CDP transport over Chrome's --remote-debugging-pipe.
// Messages are NUL-delimited. Read and Send match WebSocketable so a
// Client can sit on top of it. The caller's Send buffer is not retained.
type Pipe struct {
	r *os.File
	w *os.File

	rmu sync.Mutex
	wmu sync.Mutex
	br  *bufio.Reader

	max int

	once   sync.Once
	closed atomic.Bool
}

// NewPipe wraps the read and write ends of a debugging pipe.
// Chrome writes to the parent's read end and reads from the parent's write end.
func NewPipe(r, w *os.File) *Pipe {
	return &Pipe{r: r, w: w, br: bufio.NewReader(r), max: MaxMessageSize}
}

// Send writes one message and a trailing NUL.
func (p *Pipe) Send(msg []byte) error {
	if len(msg) > p.max {
		return ErrMessageTooLarge
	}
	if p.closed.Load() {
		return ErrPipeClosed
	}
	buf := make([]byte, len(msg)+1)
	copy(buf, msg)
	p.wmu.Lock()
	defer p.wmu.Unlock()
	_, err := p.w.Write(buf)
	if err != nil && p.closed.Load() {
		return ErrPipeClosed
	}
	return err
}

// Read returns the next NUL-terminated message, without the NUL.
func (p *Pipe) Read() ([]byte, error) {
	b, err := p.read()
	if err != nil && p.closed.Load() {
		return nil, ErrPipeClosed
	}
	return b, err
}

func (p *Pipe) read() ([]byte, error) {
	p.rmu.Lock()
	defer p.rmu.Unlock()
	if p.closed.Load() {
		return nil, ErrPipeClosed
	}
	// ReadSlice avoids an extra copy for short messages. Long ones fall
	// back to a bounded ReadBytes so a missing NUL cannot grow forever.
	b, err := p.br.ReadSlice(0)
	if err == bufio.ErrBufferFull {
		rest := make([]byte, 0, p.max)
		rest = append(rest, b...)
		for {
			chunk, err2 := p.br.ReadSlice(0)
			if len(rest)+len(chunk) > p.max {
				return nil, ErrMessageTooLarge
			}
			rest = append(rest, chunk...)
			if err2 == nil {
				return trimNUL(rest), nil
			}
			if err2 != bufio.ErrBufferFull {
				return nil, err2
			}
		}
	}
	if err != nil {
		return nil, err
	}
	return trimNUL(b), nil
}

func trimNUL(b []byte) []byte {
	if len(b) > 0 && b[len(b)-1] == 0 {
		b = b[:len(b)-1]
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

// Close closes both ends. Blocked Read and Send calls return ErrPipeClosed.
func (p *Pipe) Close() error {
	var err error
	p.once.Do(func() {
		p.closed.Store(true)
		err = p.r.Close()
		if e := p.w.Close(); err == nil {
			err = e
		}
	})
	return err
}

// PipePair is the two ends passed to Chrome and the two ends kept by the parent.
type PipePair struct {
	ChromeRead  *os.File
	ChromeWrite *os.File
	ParentRead  *os.File
	ParentWrite *os.File
}

// osPipe is a variable so tests can simulate a failing pipe.
var osPipe = os.Pipe

// NewPipePair creates an OS pipe pair for --remote-debugging-pipe.
// The child receives ChromeRead and ChromeWrite as extra files.
func NewPipePair() (*PipePair, error) {
	pr, cw, err := osPipe()
	if err != nil {
		return nil, err
	}
	cr, pw, err := osPipe()
	if err != nil {
		_ = pr.Close()
		_ = cw.Close()
		return nil, err
	}
	return &PipePair{ChromeRead: cr, ChromeWrite: cw, ParentRead: pr, ParentWrite: pw}, nil
}

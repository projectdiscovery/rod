package cdp

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
	"unicode/utf8"
)

var _ WebSocketable = &WebSocket{}

// MaxMessageSize is the largest CDP payload accepted from the peer.
// It is counted across continuation frames. Override on a WebSocket
// before Connect when a caller needs a different bound.
var MaxMessageSize = 64 << 20

const (
	maxControlFrame = 125
	maxFrames       = 1_000_000
	handshakeBody   = 4 << 10
)

// ErrMessageTooLarge is returned when a frame exceeds MaxMessageSize.
var ErrMessageTooLarge = errors.New("cdp: websocket message exceeds size limit")

// WebSocket client for chromium.
// Both Read and Send are safe for concurrent use.
// Read returns only complete text messages. Control frames are handled
// inside the reader: ping is answered, close ends the stream.
type WebSocket struct {
	// Dialer is usually used for proxy.
	Dialer Dialer

	// MaxMessageSize caps a single message. Zero uses the package default.
	MaxMessageSize int

	// WriteTimeout fails and closes the connection when a frame cannot be
	// written in time. Zero uses DefaultWriteTimeout.
	WriteTimeout time.Duration

	// KeepAlive sends a ping at this interval and fails the connection when
	// nothing is read for three intervals. Zero disables it.
	KeepAlive time.Duration

	mu   sync.Mutex
	conn net.Conn
	r    *bufio.Reader
	wmu  sync.Mutex

	closed chan struct{}
	once   sync.Once
}

// Connect to browser.
func (ws *WebSocket) Connect(ctx context.Context, wsURL string, header http.Header) error {
	ws.mu.Lock()
	if ws.conn != nil {
		ws.mu.Unlock()
		return errors.New("duplicated connection: " + wsURL)
	}
	ws.mu.Unlock()

	u, err := url.Parse(wsURL)
	if err != nil {
		return err
	}

	ws.initDialer(u)

	conn, err := ws.Dialer.DialContext(ctx, "tcp", u.Host)
	if err != nil {
		return err
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	ws.mu.Lock()
	ws.conn = conn
	ws.r = bufio.NewReader(conn)
	ws.closed = make(chan struct{})
	ws.mu.Unlock()

	if err := ws.handshake(ctx, u, header); err != nil {
		_ = conn.Close()
		return err
	}
	_ = conn.SetDeadline(time.Time{})
	if ws.KeepAlive > 0 {
		go ws.keepAlive(conn, ws.closed)
	}
	return nil
}

func (ws *WebSocket) keepAlive(conn net.Conn, closed chan struct{}) {
	t := time.NewTicker(ws.KeepAlive)
	defer t.Stop()
	for {
		select {
		case <-closed:
			return
		case <-t.C:
			if err := ws.writeControl(0x89, nil); err != nil {
				_ = ws.Close()
				return
			}
		}
	}
}

// Close the underlying connection. It is safe to call more than once.
func (ws *WebSocket) Close() error {
	ws.mu.Lock()
	conn := ws.conn
	ws.mu.Unlock()
	if conn == nil {
		return nil
	}
	ws.once.Do(func() {
		if ws.closed != nil {
			close(ws.closed)
		}
	})
	return conn.Close()
}

func (ws *WebSocket) initDialer(u *url.URL) {
	if ws.Dialer != nil {
		return
	}
	if u.Scheme == "wss" {
		ws.Dialer = &tlsDialer{}
		if u.Port() == "" {
			u.Host += ":443"
		}
	} else {
		ws.Dialer = &net.Dialer{Timeout: 30 * time.Second}
	}
}

func (ws *WebSocket) limit() int {
	if ws.MaxMessageSize > 0 {
		return ws.MaxMessageSize
	}
	return MaxMessageSize
}

// Send a text message. The caller's buffer is not modified.
func (ws *WebSocket) Send(msg []byte) error {
	err := ws.send(msg)
	if err != nil {
		_ = ws.Close()
	}
	return err
}

func (ws *WebSocket) send(msg []byte) error {
	ws.mu.Lock()
	conn := ws.conn
	ws.mu.Unlock()
	if conn == nil {
		return io.ErrClosedPipe
	}

	headerLen := 6
	if len(msg) > 65535 {
		headerLen = 14
	} else if len(msg) > 125 {
		headerLen = 8
	}
	buf := make([]byte, headerLen+len(msg))
	buf[0] = 0x81 // FIN + text
	switch {
	case len(msg) <= 125:
		buf[1] = 0x80 | byte(len(msg))
	case len(msg) <= 65535:
		buf[1] = 0x80 | 126
		binary.BigEndian.PutUint16(buf[2:], uint16(len(msg)))
	default:
		buf[1] = 0x80 | 127
		binary.BigEndian.PutUint64(buf[2:], uint64(len(msg)))
	}

	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	copy(buf[headerLen-4:], mask[:])
	payload := buf[headerLen:]
	copy(payload, msg)
	maskBytes(payload, mask)

	ws.wmu.Lock()
	defer ws.wmu.Unlock()
	// A partially written frame corrupts the stream, so no caller deadline
	// applies here. WriteTimeout only trips when the peer stopped reading,
	// and then the connection is closed.
	_ = conn.SetWriteDeadline(time.Now().Add(ws.writeTimeout()))
	_, err := conn.Write(buf)
	_ = conn.SetWriteDeadline(time.Time{})
	return err
}

func (ws *WebSocket) writeTimeout() time.Duration {
	if ws.WriteTimeout > 0 {
		return ws.WriteTimeout
	}
	return DefaultWriteTimeout
}

// DefaultWriteTimeout is used when WebSocket.WriteTimeout is zero.
var DefaultWriteTimeout = 60 * time.Second

func maskBytes(b []byte, mask [4]byte) {
	m := binary.LittleEndian.Uint32(mask[:])
	i := 0
	for ; i+8 <= len(b); i += 8 {
		v := binary.LittleEndian.Uint64(b[i:])
		v ^= uint64(m) | uint64(m)<<32
		binary.LittleEndian.PutUint64(b[i:], v)
	}
	for ; i < len(b); i++ {
		b[i] ^= mask[i%4]
	}
}

// Read a text message from the browser.
func (ws *WebSocket) Read() ([]byte, error) {
	b, err := ws.read()
	if err != nil {
		_ = ws.Close()
		return nil, err
	}
	return b, nil
}

func (ws *WebSocket) read() ([]byte, error) {
	ws.mu.Lock()
	r := ws.r
	conn := ws.conn
	ws.mu.Unlock()
	if r == nil || conn == nil {
		return nil, io.ErrClosedPipe
	}

	var assembled []byte
	frames := 0
	expectCont := false

	for {
		if ws.KeepAlive > 0 {
			_ = conn.SetReadDeadline(time.Now().Add(3 * ws.KeepAlive))
		}
		h0, err := r.ReadByte()
		if err != nil {
			return nil, err
		}
		h1, err := r.ReadByte()
		if err != nil {
			return nil, err
		}
		if h0&0x70 != 0 {
			return nil, errors.New("cdp: websocket reserved bits set")
		}
		fin := h0&0x80 != 0
		opcode := h0 & 0x0f
		masked := h1&0x80 != 0
		size, err := ws.readLen(r, h1&0x7f)
		if err != nil {
			return nil, err
		}
		if size < 0 || size > ws.limit() {
			return nil, ErrMessageTooLarge
		}
		var key [4]byte
		if masked {
			if _, err := io.ReadFull(r, key[:]); err != nil {
				return nil, err
			}
		}
		payload := make([]byte, size)
		if _, err := io.ReadFull(r, payload); err != nil {
			return nil, err
		}
		if masked {
			maskBytes(payload, key)
		}

		frames++
		if frames > maxFrames {
			return nil, errors.New("cdp: too many websocket frames")
		}

		switch opcode {
		case 0x9: // ping
			if !fin || size > maxControlFrame {
				return nil, errors.New("cdp: malformed ping")
			}
			if err := ws.writeControl(0x8A, payload); err != nil {
				return nil, err
			}
			continue
		case 0xA: // pong
			continue
		case 0x8: // close
			if !fin || size > maxControlFrame {
				return nil, errors.New("cdp: malformed close")
			}
			if size >= 2 {
				code := binary.BigEndian.Uint16(payload[:2])
				if code < 1000 || code == 1004 || code == 1005 || code == 1006 || (code > 1014 && code < 3000) {
					return nil, errors.New("cdp: invalid close code")
				}
				if size > 2 && !utf8.Valid(payload[2:]) {
					return nil, errors.New("cdp: close reason is not utf-8")
				}
			}
			_ = ws.writeControl(0x88, payload)
			return nil, io.EOF
		case 0x1: // text
			if expectCont {
				return nil, errors.New("cdp: unexpected text frame")
			}
			assembled = append(assembled, payload...)
			if len(assembled) > ws.limit() {
				return nil, ErrMessageTooLarge
			}
			if fin {
				return assembled, nil
			}
			expectCont = true
		case 0x0: // continuation
			if !expectCont {
				return nil, errors.New("cdp: unexpected continuation")
			}
			assembled = append(assembled, payload...)
			if len(assembled) > ws.limit() {
				return nil, ErrMessageTooLarge
			}
			if fin {
				return assembled, nil
			}
		case 0x2:
			return nil, errors.New("cdp: unexpected binary frame")
		default:
			return nil, fmt.Errorf("cdp: unknown opcode %d", opcode)
		}
	}
}

func (ws *WebSocket) readLen(r *bufio.Reader, b byte) (int, error) {
	switch b {
	case 126:
		var buf [2]byte
		if _, err := io.ReadFull(r, buf[:]); err != nil {
			return 0, err
		}
		return int(binary.BigEndian.Uint16(buf[:])), nil
	case 127:
		var buf [8]byte
		if _, err := io.ReadFull(r, buf[:]); err != nil {
			return 0, err
		}
		n := binary.BigEndian.Uint64(buf[:])
		if n > uint64(ws.limit()) || buf[0]&0x80 != 0 {
			return 0, ErrMessageTooLarge
		}
		return int(n), nil
	default:
		return int(b), nil
	}
}

func (ws *WebSocket) writeControl(opcode byte, payload []byte) error {
	ws.mu.Lock()
	conn := ws.conn
	ws.mu.Unlock()
	if conn == nil {
		return io.ErrClosedPipe
	}
	if len(payload) > maxControlFrame {
		payload = payload[:maxControlFrame]
	}
	buf := make([]byte, 6+len(payload))
	buf[0] = opcode
	buf[1] = 0x80 | byte(len(payload))
	var mask [4]byte
	_, _ = rand.Read(mask[:])
	copy(buf[2:], mask[:])
	copy(buf[6:], payload)
	maskBytes(buf[6:], mask)
	ws.wmu.Lock()
	defer ws.wmu.Unlock()
	_ = conn.SetWriteDeadline(time.Now().Add(ws.writeTimeout()))
	_, err := conn.Write(buf)
	_ = conn.SetWriteDeadline(time.Time{})
	return err
}

// BadHandshakeError type.
type BadHandshakeError struct {
	Status string
	Body   string
}

func (e *BadHandshakeError) Error() string {
	return fmt.Sprintf("websocket bad handshake: %s. %s", e.Status, e.Body)
}

func verifyWebSocketAccept(responseHeaders http.Header, websocketKey string) bool {
	expectedKey := websocketKey + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	hash := sha1.New()
	hash.Write([]byte(expectedKey))
	expectedAccept := base64.StdEncoding.EncodeToString(hash.Sum(nil))
	return responseHeaders.Get("Sec-WebSocket-Accept") == expectedAccept
}

func (ws *WebSocket) handshake(ctx context.Context, u *url.URL, header http.Header) error {
	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		return err
	}
	secKey := base64.StdEncoding.EncodeToString(keyBytes)

	req := (&http.Request{Method: http.MethodGet, URL: u, Header: http.Header{
		"Upgrade":               {"websocket"},
		"Connection":            {"Upgrade"},
		"Sec-WebSocket-Key":     {secKey},
		"Sec-WebSocket-Version": {"13"},
	}}).WithContext(ctx)

	if u != nil && u.User != nil && header.Get("Authorization") == "" {
		pass, _ := u.User.Password()
		req.SetBasicAuth(u.User.Username(), pass)
	}

	for k, vs := range header {
		switch {
		case k == "Host" && len(vs) > 0:
			req.Host = vs[0]
		case k == "Sec-WebSocket-Key" && len(vs) > 0:
			secKey = vs[0]
			req.Header[k] = vs
		default:
			req.Header[k] = vs
		}
	}

	ws.mu.Lock()
	conn := ws.conn
	r := ws.r
	ws.mu.Unlock()

	if err := req.Write(conn); err != nil {
		return err
	}

	res, err := http.ReadResponse(r, req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode != http.StatusSwitchingProtocols || !verifyWebSocketAccept(res.Header, secKey) {
		body, _ := io.ReadAll(io.LimitReader(res.Body, handshakeBody))
		return &BadHandshakeError{Status: res.Status, Body: string(body)}
	}
	return nil
}

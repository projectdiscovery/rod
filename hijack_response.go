package rod

import (
	"encoding/base64"
	"io"
	"net/http"
	"regexp"

	"github.com/go-rod/rod/lib/proto"
)

type hijackResponseHandler struct {
	pattern  string
	regexp   *regexp.Regexp
	handler  func(*HijackResponseStage)
	resource proto.NetworkResourceType
}

// HijackResponseStage is a request paused after the browser received the
// response headers. Unlike [Hijack.LoadResponse] the browser itself made the
// request, so cookies, proxy, TLS, and HTTP/2 behave exactly as they would
// without interception.
type HijackResponseStage struct {
	Event *proto.FetchRequestPaused

	client  proto.Client
	maxBody int
	done    bool
}

// URL of the request.
func (s *HijackResponseStage) URL() string { return s.Event.Request.URL }

// StatusCode of the response. Zero when the request failed.
func (s *HijackResponseStage) StatusCode() int {
	if s.Event.ResponseStatusCode == nil {
		return 0
	}
	return *s.Event.ResponseStatusCode
}

// Header returns the response headers.
func (s *HijackResponseStage) Header() http.Header {
	h := http.Header{}
	for _, e := range s.Event.ResponseHeaders {
		h.Add(e.Name, e.Value)
	}
	return h
}

// Body fetches the response body from the browser. It fails when the body
// is larger than the router's MaxBody.
func (s *HijackResponseStage) Body() ([]byte, error) {
	res, err := proto.FetchGetResponseBody{RequestID: s.Event.RequestID}.Call(s.c())
	if err != nil {
		return nil, err
	}
	limit := s.maxBody
	if limit <= 0 {
		limit = DefaultHijackBodyLimit
	}
	if !res.Base64Encoded {
		if len(res.Body) > limit {
			return nil, io.ErrShortBuffer
		}
		return []byte(res.Body), nil
	}
	if base64.StdEncoding.DecodedLen(len(res.Body)) > limit+3 {
		return nil, io.ErrShortBuffer
	}
	return base64.StdEncoding.DecodeString(res.Body)
}

// Continue sends the original response to the page.
func (s *HijackResponseStage) Continue() error {
	s.done = true
	return proto.FetchContinueResponse{RequestID: s.Event.RequestID}.Call(s.c())
}

// Fulfill replaces the response. A nil header keeps the original headers.
func (s *HijackResponseStage) Fulfill(code int, header http.Header, body []byte) error {
	s.done = true
	entries := s.Event.ResponseHeaders
	if header != nil {
		entries = make([]*proto.FetchHeaderEntry, 0, len(header))
		for k, vs := range header {
			for _, v := range vs {
				entries = append(entries, &proto.FetchHeaderEntry{Name: k, Value: v})
			}
		}
	}
	return proto.FetchFulfillRequest{
		RequestID:       s.Event.RequestID,
		ResponseCode:    code,
		ResponseHeaders: entries,
		Body:            body,
	}.Call(s.c())
}

// Fail aborts the request with reason.
func (s *HijackResponseStage) Fail(reason proto.NetworkErrorReason) error {
	s.done = true
	return proto.FetchFailRequest{RequestID: s.Event.RequestID, ErrorReason: reason}.Call(s.c())
}

func (s *HijackResponseStage) c() proto.Client {
	return s.client
}

// AddResponse adds a handler that runs after the browser received the
// response headers for requests matching pattern. The handler should call
// one of Continue, Fulfill, or Fail; otherwise the original response is sent.
func (r *HijackRouter) AddResponse(
	pattern string, resourceType proto.NetworkResourceType, handler func(*HijackResponseStage),
) error {
	reg, err := regexp.Compile(proto.PatternToReg(pattern))
	if err != nil {
		return err
	}

	r.mu.Lock()
	list := make([]*hijackResponseHandler, 0, len(r.response)+1)
	list = append(list, r.response...)
	list = append(list, &hijackResponseHandler{
		pattern:  pattern,
		regexp:   reg,
		handler:  handler,
		resource: resourceType,
	})
	r.response = list
	r.mu.Unlock()

	return r.apply()
}

func (r *HijackRouter) handleResponse(e *proto.FetchRequestPaused) {
	r.mu.RLock()
	handlers := r.response
	r.mu.RUnlock()

	s := &HijackResponseStage{Event: e, client: r.client, maxBody: r.MaxBody}
	for _, h := range handlers {
		if !h.regexp.MatchString(e.Request.URL) {
			continue
		}
		if h.resource != "" && h.resource != e.ResourceType {
			continue
		}
		h.handler(s)
		break
	}
	if !s.done {
		_ = proto.FetchContinueResponse{RequestID: e.RequestID}.Call(r.client)
	}
}

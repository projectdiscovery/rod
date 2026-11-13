package rod

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"

	"github.com/go-rod/rod/lib/proto"
	"github.com/go-rod/rod/lib/utils"
	"github.com/ysmood/gson"
)

// HijackRequests same as Page.HijackRequests, but can intercept requests of the entire browser.
func (b *Browser) HijackRequests() *HijackRouter {
	return newHijackRouter(b, b).initEvents()
}

// HijackRequests creates a new router instance for requests hijacking.
// When use Fetch domain outside the router should be stopped. Enabling hijacking disables page caching,
// but such as 304 Not Modified will still work as expected.
// Requests a service worker makes on its own are not seen by a page router,
// because they belong to the worker target. Use [Browser.HijackRequests] for those.
// The entire process of hijacking one request:
//
//	browser --req-> rod ---> server ---> rod --res-> browser
//
// The --req-> and --res-> are the parts that can be modified.
func (p *Page) HijackRequests() *HijackRouter {
	return newHijackRouter(p.browser, p).initEvents()
}

// DefaultHijackBodyLimit is the maximum response body LoadResponse will buffer.
const DefaultHijackBodyLimit = 64 << 20

// HijackRouter context.
type HijackRouter struct {
	run      func()
	stop     func()
	mu       sync.RWMutex
	handlers []*hijackHandler
	response []*hijackResponseHandler
	enable   *proto.FetchEnable
	client   proto.Client
	browser  *Browser

	// MaxBody caps LoadResponse and HijackResponseStage.Body. Zero means DefaultHijackBodyLimit.
	MaxBody int
}

func newHijackRouter(browser *Browser, client proto.Client) *HijackRouter {
	return &HijackRouter{
		enable:   &proto.FetchEnable{},
		browser:  browser,
		client:   client,
		handlers: []*hijackHandler{},
	}
}

// Add a hijack handler to router, the doc of the pattern is the same as "proto.FetchRequestPattern.URLPattern".
func (r *HijackRouter) Add(pattern string, resourceType proto.NetworkResourceType, handler func(*Hijack)) error {
	reg, err := regexp.Compile(proto.PatternToReg(pattern))
	if err != nil {
		return err
	}

	r.mu.Lock()
	handlers := make([]*hijackHandler, 0, len(r.handlers)+1)
	handlers = append(handlers, r.handlers...)
	handlers = append(handlers, &hijackHandler{
		pattern:  pattern,
		regexp:   reg,
		handler:  handler,
		resource: resourceType,
	})
	r.handlers = handlers
	r.mu.Unlock()

	return r.apply()
}

// Remove handler via the pattern.
func (r *HijackRouter) Remove(pattern string) error {
	r.mu.Lock()
	handlers := []*hijackHandler{}
	for _, h := range r.handlers {
		if h.pattern != pattern {
			handlers = append(handlers, h)
		}
	}
	response := []*hijackResponseHandler{}
	for _, h := range r.response {
		if h.pattern != pattern {
			response = append(response, h)
		}
	}
	r.handlers = handlers
	r.response = response
	r.mu.Unlock()

	return r.apply()
}

// Run the router, after you call it, you shouldn't add new handler to it.
func (r *HijackRouter) Run() {
	r.run()
}

// Stop the router.
func (r *HijackRouter) Stop() error {
	r.stop()
	return proto.FetchDisable{}.Call(r.client)
}

// new context.
func (r *HijackRouter) new(ctx context.Context, e *proto.FetchRequestPaused) *Hijack {
	headers := http.Header{}
	for k, v := range e.Request.Headers {
		headers[k] = []string{v.String()}
	}

	u, _ := url.Parse(e.Request.URL)

	req := &http.Request{
		Method: e.Request.Method,
		URL:    u,
		Body:   io.NopCloser(strings.NewReader(e.Request.PostData)),
		GetBody: func() (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader(e.Request.PostData)), nil
		},
		Header: headers,
	}

	return &Hijack{
		Request: &HijackRequest{
			event: e,
			req:   req.WithContext(ctx),
		},
		Response: &HijackResponse{
			payload: &proto.FetchFulfillRequest{
				ResponseCode: 200,
				RequestID:    e.RequestID,
			},
			fail: &proto.FetchFailRequest{
				RequestID: e.RequestID,
			},
		},
		OnError: func(_ error) {},

		browser: r.browser,
		maxBody: r.MaxBody,
	}
}

// apply enables Fetch with the current patterns, or disables it when there
// are none, so requests are only paused when some handler can want them.
func (r *HijackRouter) apply() error {
	r.mu.Lock()
	patterns := r.patternsLocked()
	r.enable.Patterns = patterns
	enable := proto.FetchEnable{Patterns: patterns, HandleAuthRequests: r.enable.HandleAuthRequests}
	r.mu.Unlock()

	if len(patterns) == 0 {
		return proto.FetchDisable{}.Call(r.client)
	}
	return enable.Call(r.client)
}

// patternsLocked returns the Fetch patterns of every handler. r.mu must be held.
func (r *HijackRouter) patternsLocked() []*proto.FetchRequestPattern {
	patterns := make([]*proto.FetchRequestPattern, 0, len(r.handlers)+len(r.response))
	for _, h := range r.handlers {
		patterns = append(patterns, &proto.FetchRequestPattern{URLPattern: h.pattern, ResourceType: h.resource})
	}
	for _, h := range r.response {
		patterns = append(patterns, &proto.FetchRequestPattern{
			URLPattern:   h.pattern,
			ResourceType: h.resource,
			RequestStage: proto.FetchRequestStageResponse,
		})
	}
	return patterns
}

func (r *HijackRouter) handle(eventCtx context.Context, e *proto.FetchRequestPaused) {
	defer func() {
		if rec := recover(); rec != nil {
			_ = proto.FetchContinueRequest{RequestID: e.RequestID}.Call(r.client)
		}
	}()

	if e.ResponseStatusCode != nil || e.ResponseErrorReason != "" {
		r.handleResponse(e)
		return
	}

	r.mu.RLock()
	handlers := r.handlers
	r.mu.RUnlock()

	ctx := r.new(eventCtx, e)
	for _, h := range handlers {
		if !h.regexp.MatchString(e.Request.URL) {
			continue
		}
		if h.resource != "" && h.resource != e.ResourceType {
			continue
		}

		h.handler(ctx)

		if ctx.continueRequest != nil {
			ctx.continueRequest.RequestID = e.RequestID
			err := ctx.continueRequest.Call(r.client)
			if err != nil {
				ctx.OnError(err)
			}
			return
		}

		if ctx.Skip {
			continue
		}

		if ctx.Response.fail.ErrorReason != "" {
			err := ctx.Response.fail.Call(r.client)
			if err != nil {
				ctx.OnError(err)
			}
			return
		}

		err := ctx.Response.payload.Call(r.client)
		if err != nil {
			ctx.OnError(err)
		}
		return
	}
	_ = proto.FetchContinueRequest{RequestID: e.RequestID}.Call(r.client)
}

func (r *HijackRouter) initEvents() *HijackRouter {
	ctx := r.browser.ctx
	if cta, ok := r.client.(proto.Contextable); ok {
		ctx = cta.GetContext()
	}

	var sessionID proto.TargetSessionID
	if tsa, ok := r.client.(proto.Sessionable); ok {
		sessionID = tsa.GetSessionID()
	}

	eventCtx, cancel := context.WithCancel(ctx)
	r.stop = cancel

	onPaused := func(e *proto.FetchRequestPaused, from proto.TargetSessionID) bool {
		// A browser-level router must leave requests paused by page routers alone.
		if from != sessionID {
			return false
		}
		go r.handle(eventCtx, e)
		return false
	}
	r.run = r.browser.Context(eventCtx).eachEvent(sessionID, onPaused)
	return r
}

// hijackHandler to handle each request that match the regexp.
type hijackHandler struct {
	pattern  string
	regexp   *regexp.Regexp
	handler  func(*Hijack)
	resource proto.NetworkResourceType
}

// Hijack context.
type Hijack struct {
	Request  *HijackRequest
	Response *HijackResponse
	OnError  func(error)

	// Skip to next handler
	Skip bool

	continueRequest *proto.FetchContinueRequest

	// CustomState is used to store things for this context
	CustomState any

	browser *Browser
	maxBody int
}

// ContinueRequest without hijacking. The RequestID will be set by the router, you don't have to set it.
func (h *Hijack) ContinueRequest(cq *proto.FetchContinueRequest) {
	h.continueRequest = cq
}

// LoadResponse will send request to the real destination and load the response as default response to override.
func (h *Hijack) LoadResponse(client *http.Client, loadBody bool) error {
	res, err := client.Do(h.Request.req)
	if err != nil {
		return err
	}

	defer func() { _ = res.Body.Close() }()

	h.Response.payload.ResponseCode = res.StatusCode
	h.Response.RawResponse = res

	for k, vs := range res.Header {
		for _, v := range vs {
			h.Response.SetHeader(k, v)
		}
	}

	if loadBody {
		limit := int64(h.maxBody)
		if limit <= 0 {
			limit = DefaultHijackBodyLimit
		}
		b, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
		if err != nil {
			return err
		}
		if int64(len(b)) > limit {
			return io.ErrShortBuffer
		}
		h.Response.payload.Body = b
	}

	return nil
}

// HijackRequest context.
type HijackRequest struct {
	event *proto.FetchRequestPaused
	req   *http.Request
}

// Event is the paused Fetch.requestPaused event that created this request. It
// carries the request id, the frame and the resource type exactly as Chrome
// reported them, and fields such as ResponseStatusCode when the request is
// paused at the response stage. Treat it as read-only.
func (ctx *HijackRequest) Event() *proto.FetchRequestPaused {
	return ctx.event
}

// Type of the resource.
func (ctx *HijackRequest) Type() proto.NetworkResourceType {
	return ctx.event.ResourceType
}

// Method of the request.
func (ctx *HijackRequest) Method() string {
	return ctx.event.Request.Method
}

// URL of the request.
func (ctx *HijackRequest) URL() *url.URL {
	u, _ := url.Parse(ctx.event.Request.URL)
	return u
}

// Header via a key.
func (ctx *HijackRequest) Header(key string) string {
	return ctx.event.Request.Headers[key].String()
}

// Headers of request.
func (ctx *HijackRequest) Headers() proto.NetworkHeaders {
	return ctx.event.Request.Headers
}

// Body of the request, devtools API doesn't support binary data yet, only string can be captured.
func (ctx *HijackRequest) Body() string {
	return ctx.event.Request.PostData
}

// JSONBody of the request.
func (ctx *HijackRequest) JSONBody() gson.JSON {
	return gson.NewFrom(ctx.Body())
}

// Req returns the underlying http.Request instance that will be used to send the request.
func (ctx *HijackRequest) Req() *http.Request {
	return ctx.req
}

// SetContext of the underlying http.Request instance.
func (ctx *HijackRequest) SetContext(c context.Context) *HijackRequest {
	ctx.req = ctx.req.WithContext(c)
	return ctx
}

// SetBody of the request, if obj is []byte or string, raw body will be used, else it will be encoded as json.
func (ctx *HijackRequest) SetBody(obj any) *HijackRequest {
	var b []byte

	switch body := obj.(type) {
	case []byte:
		b = body
	case string:
		b = []byte(body)
	default:
		b = utils.MustToJSONBytes(body)
	}

	ctx.req.Body = io.NopCloser(bytes.NewBuffer(b))

	return ctx
}

// IsNavigation determines whether the request is a navigation request.
func (ctx *HijackRequest) IsNavigation() bool {
	return ctx.Type() == proto.NetworkResourceTypeDocument
}

// HijackResponse context.
type HijackResponse struct {
	payload     *proto.FetchFulfillRequest
	RawResponse *http.Response
	fail        *proto.FetchFailRequest
}

// Payload to respond the request from the browser.
func (ctx *HijackResponse) Payload() *proto.FetchFulfillRequest {
	return ctx.payload
}

// Body of the payload.
func (ctx *HijackResponse) Body() string {
	return string(ctx.payload.Body)
}

// Headers returns the clone of response headers.
// If you want to modify the response headers use HijackResponse.SetHeader .
func (ctx *HijackResponse) Headers() http.Header {
	header := http.Header{}

	for _, h := range ctx.payload.ResponseHeaders {
		header.Add(h.Name, h.Value)
	}

	return header
}

// SetHeader of the payload via key-value pairs.
func (ctx *HijackResponse) SetHeader(pairs ...string) *HijackResponse {
	headerIndex := make(map[string]int, len(ctx.payload.ResponseHeaders))
	for i, header := range ctx.payload.ResponseHeaders {
		headerIndex[header.Name] = i
	}

	for i := 0; i < len(pairs); i += 2 {
		name := pairs[i]
		value := pairs[i+1]

		if idx, exists := headerIndex[name]; exists {
			ctx.payload.ResponseHeaders[idx].Value = value
		} else {
			ctx.payload.ResponseHeaders = append(ctx.payload.ResponseHeaders, &proto.FetchHeaderEntry{
				Name:  name,
				Value: value,
			})
			headerIndex[name] = len(ctx.payload.ResponseHeaders) - 1
		}
	}
	return ctx
}

// AddHeader appends key-value pairs to the end of the response headers.
// Duplicate keys will be preserved.
func (ctx *HijackResponse) AddHeader(pairs ...string) *HijackResponse {
	for i := 0; i < len(pairs); i += 2 {
		ctx.payload.ResponseHeaders = append(ctx.payload.ResponseHeaders, &proto.FetchHeaderEntry{
			Name:  pairs[i],
			Value: pairs[i+1],
		})
	}
	return ctx
}

// SetBody of the payload, if obj is []byte or string, raw body will be used, else it will be encoded as json.
func (ctx *HijackResponse) SetBody(obj any) *HijackResponse {
	switch body := obj.(type) {
	case []byte:
		ctx.payload.Body = body
	case string:
		ctx.payload.Body = []byte(body)
	default:
		ctx.payload.Body = utils.MustToJSONBytes(body)
	}
	return ctx
}

// Fail request.
func (ctx *HijackResponse) Fail(reason proto.NetworkErrorReason) *HijackResponse {
	ctx.fail.ErrorReason = reason
	return ctx
}

// HandleAuth answers the next HTTP authentication challenge with the given
// credentials. Interception starts when HandleAuth is called; the returned
// function blocks until one challenge is answered or the browser context ends.
// Only browser-level Fetch events are handled, so page-level [HijackRouter]
// instances keep their requests. When a browser-level Fetch interception is
// already active its owner keeps continuing paused requests.
// Ref: https://developer.mozilla.org/en-US/docs/Web/HTTP/Authentication
func (b *Browser) HandleAuth(username, password string) func() error {
	ctx, cancel := context.WithCancel(b.ctx)
	messages := b.listen(ctx, "", map[string]struct{}{
		(proto.FetchRequestPaused{}).ProtoEvent(): {},
		(proto.FetchAuthRequired{}).ProtoEvent():  {},
	})

	prev := &proto.FetchEnable{}
	owned := !b.LoadState("", prev)
	enableErr := proto.FetchEnable{
		Patterns:           prev.Patterns,
		HandleAuthRequests: true,
	}.Call(b)

	restore := func() {
		cancel()
		c := b.Context(context.Background())
		if owned {
			_ = proto.FetchDisable{}.Call(c)
		} else {
			_ = prev.Call(c)
		}
	}

	return func() error {
		defer restore()
		if enableErr != nil {
			return enableErr
		}

		for msg := range messages {
			if msg.SessionID != "" {
				continue
			}
			paused := proto.FetchRequestPaused{}
			auth := proto.FetchAuthRequired{}
			switch {
			case msg.Load(&paused):
				if owned {
					err := proto.FetchContinueRequest{RequestID: paused.RequestID}.Call(b.Context(ctx))
					if err != nil {
						return err
					}
				}
			case msg.Load(&auth):
				return proto.FetchContinueWithAuth{
					RequestID: auth.RequestID,
					AuthChallengeResponse: &proto.FetchAuthChallengeResponse{
						Response: proto.FetchAuthChallengeResponseResponseProvideCredentials,
						Username: username,
						Password: password,
					},
				}.Call(b.Context(ctx))
			}
		}
		return ctx.Err()
	}
}

package rod

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/go-rod/rod/lib/proto"
)

// DefaultEventQueue is the per-subscription queue length used when
// Browser.EventQueue is not set. Non-critical events beyond it are dropped.
const DefaultEventQueue = 4096

// router delivers browser events to subscriptions indexed by session and by
// method, so publishing an event touches only the subscriptions that can
// want it. Publishing never blocks: each subscription has its own queue and
// a goroutine that hands messages to the consumer.
type router struct {
	mu sync.RWMutex

	// all holds subscriptions with no session and no method filter.
	all map[*subscription]struct{}
	// byMethod holds subscriptions with no session filter, keyed by method.
	byMethod map[string]map[*subscription]struct{}
	// bySession holds session subscriptions, with or without a method filter.
	bySession map[proto.TargetSessionID]map[*subscription]struct{}

	queue   int
	dropped atomic.Uint64
}

type subscription struct {
	r       *router
	ctx     context.Context
	session proto.TargetSessionID
	methods map[string]struct{}
	out     chan *Message

	mu     sync.Mutex
	queue  []*Message
	notify chan struct{}
}

func newRouter(queue int) *router {
	return &router{
		all:       map[*subscription]struct{}{},
		byMethod:  map[string]map[*subscription]struct{}{},
		bySession: map[proto.TargetSessionID]map[*subscription]struct{}{},
		queue:     queue,
	}
}

// subscribe returns a channel of events. An empty session means every
// session. A nil methods set means every method. The channel is closed after
// ctx ends.
func (r *router) subscribe(ctx context.Context, session proto.TargetSessionID, methods map[string]struct{}) <-chan *Message {
	s := &subscription{
		r:       r,
		ctx:     ctx,
		session: session,
		methods: methods,
		out:     make(chan *Message),
		notify:  make(chan struct{}, 1),
	}

	r.mu.Lock()
	switch {
	case session != "":
		set := r.bySession[session]
		if set == nil {
			set = map[*subscription]struct{}{}
			r.bySession[session] = set
		}
		set[s] = struct{}{}
	case methods == nil:
		r.all[s] = struct{}{}
	default:
		for m := range methods {
			set := r.byMethod[m]
			if set == nil {
				set = map[*subscription]struct{}{}
				r.byMethod[m] = set
			}
			set[s] = struct{}{}
		}
	}
	r.mu.Unlock()

	go s.pump()
	return s.out
}

func (r *router) unsubscribe(s *subscription) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case s.session != "":
		if set := r.bySession[s.session]; set != nil {
			delete(set, s)
			if len(set) == 0 {
				delete(r.bySession, s.session)
			}
		}
	case s.methods == nil:
		delete(r.all, s)
	default:
		for m := range s.methods {
			if set := r.byMethod[m]; set != nil {
				delete(set, s)
				if len(set) == 0 {
					delete(r.byMethod, m)
				}
			}
		}
	}
}

func (r *router) publish(m *Message) {
	r.mu.RLock()
	targets := make([]*subscription, 0, len(r.all)+4)
	for s := range r.all {
		targets = append(targets, s)
	}
	for s := range r.byMethod[m.Method] {
		targets = append(targets, s)
	}
	if m.SessionID != "" {
		for s := range r.bySession[m.SessionID] {
			if s.methods == nil {
				targets = append(targets, s)
			} else if _, ok := s.methods[m.Method]; ok {
				targets = append(targets, s)
			}
		}
	}
	r.mu.RUnlock()

	critical := criticalEvent(m.Method)
	for _, s := range targets {
		s.push(m, critical)
	}
}

func (s *subscription) push(m *Message, critical bool) {
	s.mu.Lock()
	if !critical && s.r.queue > 0 && len(s.queue) >= s.r.queue {
		s.mu.Unlock()
		s.r.dropped.Add(1)
		return
	}
	s.queue = append(s.queue, m)
	s.mu.Unlock()
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

func (s *subscription) pump() {
	defer close(s.out)
	defer s.r.unsubscribe(s)
	for {
		s.mu.Lock()
		var m *Message
		if len(s.queue) > 0 {
			m = s.queue[0]
			s.queue[0] = nil
			s.queue = s.queue[1:]
			if len(s.queue) == 0 {
				s.queue = nil
			}
		}
		s.mu.Unlock()

		if m == nil {
			select {
			case <-s.ctx.Done():
				return
			case <-s.notify:
				continue
			}
		}

		select {
		case <-s.ctx.Done():
			return
		case s.out <- m:
		}
	}
}

// criticalEvent reports events that are never dropped. Losing one would
// leave a request paused, a dialog open, or a target untracked.
func criticalEvent(method string) bool {
	if strings.HasPrefix(method, "Target.") || strings.HasPrefix(method, "Fetch.") {
		return true
	}
	switch method {
	case "Page.javascriptDialogOpening", "Page.javascriptDialogClosed",
		"Inspector.detached", "Inspector.targetCrashed":
		return true
	}
	return false
}

// DroppedEvents reports how many non-critical events were discarded because a
// subscription's queue was full.
func (b *Browser) DroppedEvents() uint64 {
	if b.router == nil {
		return 0
	}
	return b.router.dropped.Load()
}

// EventQueue sets the per-subscription queue length for events delivered
// after the next Connect. Zero uses DefaultEventQueue. A negative value keeps
// every event, like upstream rod.
func (b *Browser) EventQueue(n int) *Browser {
	b.eventQueue = n
	return b
}

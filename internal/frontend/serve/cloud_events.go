package serve

import (
	"encoding/json"
	"errors"
	"sync"
	"time"

	"reasonix/internal/platform/remotecloud"
)

// cloudEventsKeepalive matches the SSE stream's watermark cadence; a var so
// tests can shorten it.
var cloudEventsKeepalive = sseKeepaliveInterval

// CloudEvents is the push side of the relay's event lane: one runtime's stream
// with the same attach/resume semantics the SSE endpoint serves.
func (h *Hub) CloudEvents(runtimeID string, after int64, attach bool) (remotecloud.EventStream, error) {
	runtime := h.eventRuntime(runtimeID)
	if runtime == nil {
		return nil, errors.New("runtime has no local event stream")
	}
	var frames <-chan Frame
	var unsubscribe func()
	if attach {
		frames, unsubscribe = runtime.Server.subscribeEvents(0, true, true)
	} else {
		frames, unsubscribe = runtime.Server.subscribeEvents(after, false, true)
	}
	stream := &cloudEventStream{
		frames: make(chan remotecloud.EventFrame, 64),
		done:   make(chan struct{}), unsubscribe: unsubscribe,
	}
	go stream.run(frames, runtime.Events)
	return stream, nil
}

// eventRuntime resolves a subscription's target: a named local runtime, or the
// first local one when the controller spells the unprefixed /events path.
func (h *Hub) eventRuntime(id string) *Runtime {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if id != "" {
		runtime := h.runtimes[id]
		if runtime == nil || runtime.remote != nil || runtime.Server == nil {
			return nil
		}
		return runtime
	}
	for _, key := range h.order {
		runtime := h.runtimes[key]
		if runtime != nil && runtime.remote == nil && runtime.Server != nil {
			return runtime
		}
	}
	return nil
}

type cloudEventStream struct {
	frames      chan remotecloud.EventFrame
	done        chan struct{}
	unsubscribe func()
	once        sync.Once
}

func (s *cloudEventStream) Frames() <-chan remotecloud.EventFrame { return s.frames }

func (s *cloudEventStream) Stop() {
	s.once.Do(func() {
		close(s.done)
		s.unsubscribe()
	})
}

func (s *cloudEventStream) run(frames <-chan Frame, events *Broadcaster) {
	defer close(s.frames)
	keepalive := time.NewTicker(cloudEventsKeepalive)
	defer keepalive.Stop()
	for {
		select {
		case <-s.done:
			return
		case frame, ok := <-frames:
			if !ok {
				return
			}
			select {
			case s.frames <- remotecloud.EventFrame{Seq: frame.Seq, Data: frame.Data}:
			case <-s.done:
				return
			}
		case <-keepalive.C:
			data, err := json.Marshal(map[string]any{"kind": "stream_watermark", "seq": events.Watermark()})
			if err != nil {
				continue
			}
			select {
			case s.frames <- remotecloud.EventFrame{Data: data}:
			case <-s.done:
				return
			}
		}
	}
}

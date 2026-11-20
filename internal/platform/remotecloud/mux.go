package remotecloud

import (
	"encoding/json"
	"log/slog"
	"sync"
	"time"
	"unicode/utf8"

	"reasonix/internal/platform/account"
)

const (
	// muxFrameChunk keeps one pushed part well under the relay's 64KiB frame
	// limit once the sealed envelope is added; a large event arrives as parts.
	muxFrameChunk    = 16 << 10
	muxCreditCap     = 1 << 20
	muxCreditDefault = 256 << 10
)

// muxCreditWait is how long a push waits for the controller to replenish its
// byte budget before the subscription is dropped. A var so tests can shorten it.
var muxCreditWait = 30 * time.Second

// EventFrame is one frame of a runtime's event stream; Seq 0 is unnumbered.
type EventFrame struct {
	Seq  int64
	Data []byte
}

// EventStream is a live subscription. Frames closes when the source ends; Stop
// releases it and is safe to call more than once.
type EventStream interface {
	Frames() <-chan EventFrame
	Stop()
}

// EventService is the push side a frontend may implement. The platform layer
// cannot import a frontend, so the host injects it like DesktopService.
type EventService interface {
	CloudEvents(runtimeID string, after int64, attach bool) (EventStream, error)
}

type muxPush struct {
	enabled bool
	subsMu  sync.Mutex
	subs    map[string]*muxSub
}

type muxSub struct {
	id      string
	session *controllerSession
	writer  *relayWriter
	stream  EventStream

	mu     sync.Mutex
	credit int64
	wake   chan struct{}
	done   chan struct{}
	once   sync.Once
}

func (sub *muxSub) cancel() {
	sub.once.Do(func() {
		close(sub.done)
		sub.stream.Stop()
	})
}

func (sub *muxSub) grant(n int64) int64 {
	sub.mu.Lock()
	sub.credit = min(sub.credit+n, muxCreditCap)
	total := sub.credit
	sub.mu.Unlock()
	select {
	case sub.wake <- struct{}{}:
	default:
	}
	return total
}

// take waits for n bytes of budget. Credit is consumed per chunk, so a frame
// larger than the whole budget still streams as the controller replenishes.
func (sub *muxSub) take(n int64) bool {
	timer := time.NewTimer(muxCreditWait)
	defer timer.Stop()
	for {
		sub.mu.Lock()
		if sub.credit >= n {
			sub.credit -= n
			sub.mu.Unlock()
			return true
		}
		sub.mu.Unlock()
		select {
		case <-sub.wake:
		case <-sub.done:
			return false
		case <-timer.C:
			return false
		}
	}
}

func helloMux(payload string) bool {
	var greeting hello
	if json.Unmarshal([]byte(payload), &greeting) != nil {
		return false
	}
	return greeting.Capabilities["mux"] >= 1
}

func (h *Host) muxCommand(s *controllerSession, cmd sessionCommand) {
	command := cmd.command
	fail := func(message string) {
		reply, err := s.cipher.seal(map[string]any{"v": protocolVersion, "type": "error", "id": command.ID, "error": message})
		if err == nil {
			_ = writeDirected(cmd.writer, s.connectionID, reply)
		}
	}
	if s.push == nil || !s.push.enabled || h.events == nil || !hasScope(cmd.scopes, account.RemoteDesktop) {
		fail("event push is unavailable")
		return
	}
	switch command.Type {
	case "mux.open":
		// Subscribe before the duplicate check: the insert below must hold the
		// same lock cancelPush sweeps, so a session that ends mid-open cannot
		// strand a pump on a dead stream. A refused open releases the stream.
		stream, err := h.events.CloudEvents(command.Runtime, command.After, command.Attach)
		if err != nil {
			fail(err.Error())
			return
		}
		credit := command.Credit
		if credit <= 0 {
			credit = muxCreditDefault
		}
		s.push.subsMu.Lock()
		held := s.push.subs[command.ID] != nil
		if !held {
			select {
			case <-s.done:
				held = true
			default:
			}
		}
		if held {
			s.push.subsMu.Unlock()
			stream.Stop()
			fail("stream is already open")
			return
		}
		sub := &muxSub{
			id: command.ID, session: s, writer: cmd.writer, stream: stream,
			credit: min(credit, muxCreditCap), wake: make(chan struct{}, 1), done: make(chan struct{}),
		}
		s.push.subs[command.ID] = sub
		s.push.subsMu.Unlock()
		// Acknowledge before the pump starts: pump and ack contend for the relay
		// writer, and a frame that overtakes the ack is dropped by the
		// controller, which does not yet know the stream is open.
		reply, err := s.cipher.seal(map[string]any{"v": protocolVersion, "type": "mux.opened", "id": command.ID, "stream": command.ID})
		if err == nil {
			err = writeDirected(cmd.writer, s.connectionID, reply)
		}
		if err != nil {
			sub.cancel()
			s.push.subsMu.Lock()
			if s.push.subs[command.ID] == sub {
				delete(s.push.subs, command.ID)
			}
			s.push.subsMu.Unlock()
			return
		}
		go h.pumpMux(s, sub)
	case "mux.close":
		s.push.subsMu.Lock()
		sub := s.push.subs[command.ID]
		delete(s.push.subs, command.ID)
		s.push.subsMu.Unlock()
		if sub != nil {
			sub.cancel()
		}
		reply, err := s.cipher.seal(map[string]any{
			"v": protocolVersion, "type": "mux.closed", "id": command.ID, "stream": command.ID, "reason": "closed by controller",
		})
		if err == nil {
			_ = writeDirected(cmd.writer, s.connectionID, reply)
		}
	case "mux.credit":
		// Reached only if a credit slipped past the read-loop grant; the
		// session goroutine may be parked on a slow desktop request, so the
		// grant still happens off-thread rather than never.
		go func() {
			s.push.subsMu.Lock()
			sub := s.push.subs[command.ID]
			s.push.subsMu.Unlock()
			if sub == nil {
				fail("unknown stream")
				return
			}
			total := sub.grant(command.Credit)
			reply, err := s.cipher.seal(map[string]any{"v": protocolVersion, "type": "mux.credit", "id": command.ID, "stream": command.ID, "credit": total})
			if err == nil {
				_ = writeDirected(cmd.writer, s.connectionID, reply)
			}
		}()
	}
}

// muxCredit grants byte budget in the read loop, before the command queue: a
// credit that queues behind a slow desktop request would let the credit
// timeout starve this session's own streams. Granting is a mutex and a
// channel send; the ack write is bounded by the relay write deadline.
func (h *Host) muxCredit(s *controllerSession, writer *relayWriter, scopes []account.RemoteCapability, command controllerCommand) {
	if s.push == nil || !s.push.enabled || !hasScope(scopes, account.RemoteDesktop) {
		reply, err := s.cipher.seal(map[string]any{"v": protocolVersion, "type": "error", "id": command.ID, "error": "event push is unavailable"})
		if err == nil {
			_ = writeDirected(writer, s.connectionID, reply)
		}
		return
	}
	s.push.subsMu.Lock()
	sub := s.push.subs[command.ID]
	s.push.subsMu.Unlock()
	if sub == nil {
		reply, err := s.cipher.seal(map[string]any{"v": protocolVersion, "type": "error", "id": command.ID, "error": "unknown stream"})
		if err == nil {
			_ = writeDirected(writer, s.connectionID, reply)
		}
		return
	}
	total := sub.grant(command.Credit)
	reply, err := s.cipher.seal(map[string]any{"v": protocolVersion, "type": "mux.credit", "id": command.ID, "stream": command.ID, "credit": total})
	if err == nil {
		_ = writeDirected(writer, s.connectionID, reply)
	}
}

func (h *Host) pumpMux(s *controllerSession, sub *muxSub) {
	defer func() {
		sub.cancel()
		s.push.subsMu.Lock()
		if s.push.subs[sub.id] == sub {
			delete(s.push.subs, sub.id)
		}
		s.push.subsMu.Unlock()
	}()
	for {
		select {
		case <-sub.done:
			return
		case frame, ok := <-sub.stream.Frames():
			if !ok {
				// The source ended without a controller request: say so, or the
				// controller's stream stalls open until the link itself ends.
				h.pushMuxClosed(s, sub, "source_ended")
				return
			}
			if !h.pushFrame(s, sub, frame) {
				return
			}
		}
	}
}

// pushFrame writes one event as budgeted chunks; false means the subscription
// is finished — closed, starved, or the connection is gone.
func (h *Host) pushFrame(s *controllerSession, sub *muxSub, frame EventFrame) bool {
	for start, index := 0, 0; ; {
		end := min(start+muxFrameChunk, len(frame.Data))
		// Chunk on rune boundaries: the sealed payload is JSON, and json.Marshal
		// replaces a torn UTF-8 sequence with U+FFFD, which would corrupt the
		// reassembled event silently. Event data is valid UTF-8, so the scan
		// always lands on a rune start and never reaches start.
		for end < len(frame.Data) && !utf8.RuneStart(frame.Data[end]) {
			end--
		}
		chunk := frame.Data[start:end]
		if !sub.take(int64(len(chunk))) {
			h.pushMuxClosed(s, sub, "credit_timeout")
			return false
		}
		reply, err := s.cipher.seal(map[string]any{
			"v": protocolVersion, "type": "mux.frame", "stream": sub.id,
			"seq": frame.Seq, "index": index, "done": end == len(frame.Data),
			"data": string(chunk),
		})
		if err != nil {
			return false
		}
		if err := writeDirected(sub.writer, s.connectionID, reply); err != nil {
			return false
		}
		if h.muxTrace {
			slog.Debug("remote mux frame", "stream", sub.id, "seq", frame.Seq, "bytes", len(chunk))
		}
		if end == len(frame.Data) {
			return true
		}
		start = end
		index++
	}
}

func (h *Host) pushMuxClosed(s *controllerSession, sub *muxSub, reason string) {
	reply, err := s.cipher.seal(map[string]any{"v": protocolVersion, "type": "mux.closed", "stream": sub.id, "reason": reason})
	if err == nil {
		_ = writeDirected(sub.writer, s.connectionID, reply)
	}
}

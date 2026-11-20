package remotecloud

import (
	"context"
	"time"

	"reasonix/internal/platform/account"
)

// controllerSession is one encrypted controller. The read loop owns admission
// (cipher.open, admit); a dedicated goroutine runs commands in arrival order,
// so a slow desktop request stalls neither the read loop nor another session.
type controllerSession struct {
	cipher       *sessionCipher
	ordinal      int
	nonce        string
	connectionID string
	deviceID     string

	lastSeq int64
	uploads uploads

	commands chan sessionCommand
	done     chan struct{}
	push     *muxPush
}

type sessionCommand struct {
	writer  *relayWriter
	scopes  []account.RemoteCapability
	command controllerCommand
}

// stop ends the session: its command goroutine exits and every push
// subscription is released. Only the Run goroutine calls it.
func (s *controllerSession) stop() {
	if s.done != nil {
		close(s.done)
	}
	s.cancelPush()
}

// cancelPush releases subscriptions but keeps the session resumable: a device
// reconnect within the grace period reuses the cipher, and the controller
// reopens its streams from its own cursor.
func (s *controllerSession) cancelPush() {
	if s.push == nil {
		return
	}
	s.push.subsMu.Lock()
	for id, sub := range s.push.subs {
		sub.cancel()
		delete(s.push.subs, id)
	}
	s.push.subsMu.Unlock()
}

func (h *Host) runSession(ctx context.Context, s *controllerSession) {
	for {
		select {
		case <-s.done:
			return
		case cmd := <-s.commands:
			h.runCommand(ctx, s, cmd)
		}
	}
}

func (h *Host) runCommand(ctx context.Context, s *controllerSession, cmd sessionCommand) {
	select {
	case <-s.done:
		return
	default:
	}
	s.uploads.expire(time.Now())
	var response map[string]any
	switch cmd.command.Type {
	case "ping":
		response = map[string]any{"v": protocolVersion, "type": "pong", "id": cmd.command.ID, "at": time.Now().UTC().Format(time.RFC3339Nano)}
	case "tasks.list", "tasks.get", "tasks.send":
		response = h.taskCommand(ctx, s.deviceID, s.ordinal, cmd.scopes, cmd.command)
	case "desktop.request":
		_ = h.desktopCommand(ctx, cmd.writer, s.cipher, s.deviceID, s.ordinal, s.connectionID, cmd.scopes, cmd.command)
		return
	case "desktop.request.part":
		_ = h.uploadPart(ctx, cmd.writer, s, s.deviceID, s.connectionID, cmd.scopes, cmd.command)
		return
	case "mux.open", "mux.close":
		h.muxCommand(s, cmd)
		return
	default:
		return
	}
	reply, err := s.cipher.seal(response)
	if err != nil {
		return
	}
	_ = writeDirected(cmd.writer, s.connectionID, reply)
}

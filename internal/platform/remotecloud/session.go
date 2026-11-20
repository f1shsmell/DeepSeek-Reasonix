package remotecloud

import (
	"context"
	"crypto/ecdh"
	"runtime"
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
	bulk    bool

	commands chan sessionCommand
	done     chan struct{}
	push     *muxPush
}

type sessionCommand struct {
	writer  *relayWriter
	scopes  []account.RemoteCapability
	command controllerCommand
	// bulk carries one binary part; command is then the zero value.
	bulk []byte
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

// admitController performs the sealed handshake for a new controller and
// answers with the ready greeting, advertising only the lanes both sides
// declared.
func (h *Host) admitController(ctx context.Context, writer *relayWriter, saved *identity, private *ecdh.PrivateKey, message gatewayMessage) (*controllerSession, error) {
	created, err := newSessionCipher(private, saved.DeviceID, message.Payload)
	if err != nil {
		return nil, err
	}
	ordinal := 0
	if h.presence != nil {
		ordinal = h.presence.CloudControllerConnected(message.ConnectionID, "Web Studio")
	}
	nonce, err := randomToken()
	if err != nil {
		return nil, err
	}
	push := helloMux(message.Payload)
	bulk := helloBulk(message.Payload)
	session := &controllerSession{
		cipher: created, ordinal: ordinal, nonce: nonce,
		connectionID: message.ConnectionID, deviceID: "cloud:" + message.ConnectionID,
		commands: make(chan sessionCommand, 256), done: make(chan struct{}),
		push: &muxPush{enabled: push, subs: make(map[string]*muxSub)},
		bulk: bulk,
	}
	go h.runSession(ctx, session)
	ready := map[string]any{
		"v": protocolVersion, "type": "ready", "deviceId": saved.DeviceID,
		"name": h.name, "platform": platformName(runtime.GOOS), "version": h.version,
		"features": []string{"replay", "upload"}, "session": nonce, "instance": h.instance(),
	}
	capabilities := map[string]int{}
	if h.events != nil && push {
		capabilities["mux"] = 1
	}
	if bulk {
		capabilities["bulk"] = 1
	}
	if len(capabilities) > 0 {
		ready["capabilities"] = capabilities
	}
	reply, err := created.seal(ready)
	if err != nil {
		session.stop()
		return nil, err
	}
	if err := writeDirected(writer, message.ConnectionID, reply); err != nil {
		session.stop()
		return nil, err
	}
	return session, nil
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
	if cmd.bulk != nil {
		h.bulkPart(ctx, s, cmd)
		return
	}
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
	case "bulk.open":
		h.bulkOpen(s, cmd)
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

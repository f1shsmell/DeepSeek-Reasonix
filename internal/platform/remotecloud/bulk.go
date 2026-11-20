package remotecloud

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	"reasonix/internal/platform/account"
)

// The bulk lane carries an attachment's bytes as binary sealed parts: only
// the envelope inflates them. The kernel rebuilds the JSON body the desktop's
// /attachments endpoint expects, and the endpoint itself is unchanged.

func helloBulk(payload string) bool {
	var greeting hello
	if json.Unmarshal([]byte(payload), &greeting) != nil {
		return false
	}
	return greeting.Capabilities["bulk"] >= 1
}

// queueBulk admits one binary part onto the session's command queue, keeping
// it in arrival order beside the JSON commands. A full queue drops the part;
// the next part's index then refuses the upload, and the controller resumes.
func (h *Host) queueBulk(s *controllerSession, writer *relayWriter, scopes []account.RemoteCapability, part []byte) error {
	if !s.bulk {
		return errors.New("remote cloud: bulk lane was not negotiated")
	}
	select {
	case s.commands <- sessionCommand{writer: writer, scopes: scopes, bulk: part}:
	case <-s.done:
	default:
	}
	return nil
}

func (h *Host) bulkOpen(s *controllerSession, cmd sessionCommand) {
	command := cmd.command
	fail := func(message string) {
		reply, err := s.cipher.seal(map[string]any{"v": protocolVersion, "type": "error", "id": command.ID, "error": message})
		if err == nil {
			_ = writeDirected(cmd.writer, s.connectionID, reply)
		}
	}
	if h.desktop == nil || !hasScope(cmd.scopes, account.RemoteDesktop) {
		s.uploads.drop(command.ID)
		fail("desktop access is unavailable")
		return
	}
	if !s.bulk {
		fail("bulk lane was not negotiated")
		return
	}
	next, err := s.uploads.begin(command, time.Now())
	if err != nil {
		fail(err.Error())
		return
	}
	reply, err := s.cipher.seal(map[string]any{"v": protocolVersion, "type": "bulk.ready", "id": command.ID, "next": next})
	if err == nil {
		_ = writeDirected(cmd.writer, s.connectionID, reply)
	}
}

func (h *Host) bulkPart(ctx context.Context, s *controllerSession, cmd sessionCommand) {
	cur := s.uploads.current
	if h.desktop == nil || !hasScope(cmd.scopes, account.RemoteDesktop) {
		if cur != nil {
			s.uploads.drop(cur.id)
			// Say so: a silent drop leaves the controller's upload waiting out
			// its sixty-second watchdog for parts that will never land.
			_ = h.writeDesktopError(cmd.writer, s.cipher, s.connectionID, cur.id, "desktop access is unavailable")
		}
		return
	}
	request, done, err := s.uploads.addBinary(cmd.bulk, time.Now())
	if err != nil {
		if cur != nil {
			_ = h.writeDesktopError(cmd.writer, s.cipher, s.connectionID, cur.id, err.Error())
		}
		return
	}
	if !done {
		return
	}
	body, err := attachmentBody(cur.name, cur.mime, request.Body)
	if err != nil {
		return
	}
	request.Body = body
	_ = h.runDesktop(ctx, cmd.writer, s.cipher, s.deviceID, s.ordinal, s.connectionID, cur.id, request)
}

// attachmentBody rebuilds the JSON the /attachments endpoint reads. The
// encoding matches what a browser's btoa produces, which is what the endpoint
// has always decoded.
func attachmentBody(name, mime string, data []byte) ([]byte, error) {
	return json.Marshal(map[string]string{
		"name": name,
		"mime": mime,
		"data": base64.StdEncoding.EncodeToString(data),
	})
}

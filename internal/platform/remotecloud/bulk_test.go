package remotecloud

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"reasonix/internal/platform/account"
)

func (r *muxRig) binary(t *testing.T, channel *sessionCipher, connectionID string, index int, part []byte) {
	t.Helper()
	r.binaryWithScopes(t, channel, connectionID, []account.RemoteCapability{account.RemoteDesktop}, index, part)
}

func (r *muxRig) binaryWithScopes(t *testing.T, channel *sessionCipher, connectionID string, scopes []account.RemoteCapability, index int, part []byte) {
	t.Helper()
	framed := make([]byte, 4+len(part))
	binary.BigEndian.PutUint32(framed[:4], uint32(index))
	copy(framed[4:], part)
	payload, err := channel.sealBytes(framed)
	if err != nil {
		t.Fatal(err)
	}
	r.dispatch(t, gatewayMessage{
		Type: "controller_message", ConnectionID: connectionID,
		Scopes: scopes, Payload: payload,
	})
}

func bulkOpenCommand(id string, total int) controllerCommand {
	return controllerCommand{
		Version: 1, Type: "bulk.open", ID: id,
		Path: "/rt/r1/attachments", Name: "photo.png", Mime: "image/png", Total: total,
	}
}

func TestBulkUploadRebuildsTheAttachmentBody(t *testing.T) {
	desktop := &recordingDesktop{}
	rig := newMuxRig(t, &Host{desktop: desktop})
	connectionID := strings.Repeat("c", 32)
	channel := rig.greet(t, connectionID, map[string]int{"bulk": 1})
	ready := readDirected(t, rig.controller, channel)
	caps, _ := ready["capabilities"].(map[string]any)
	if caps["bulk"] != float64(1) {
		t.Fatalf("ready did not advertise bulk: %+v", ready)
	}

	rig.command(t, channel, connectionID, bulkOpenCommand("up1", 2))
	if answer := readDirected(t, rig.controller, channel); answer["type"] != "bulk.ready" || answer["next"] != float64(0) {
		t.Fatalf("bulk.ready = %+v", answer)
	}
	first := []byte("first half of the file")
	second := []byte("second half")
	rig.binary(t, channel, connectionID, 0, first)
	rig.binary(t, channel, connectionID, 1, second)
	if answer := readDirected(t, rig.controller, channel); answer["type"] != "desktop.response" || answer["id"] != "up1" {
		t.Fatalf("final answer = %+v", answer)
	}

	seen := desktop.seen()
	if len(seen) != 1 {
		t.Fatalf("desktop ran %d requests, want 1", len(seen))
	}
	request := seen[0]
	if request.Method != http.MethodPost || request.Path != "/rt/r1/attachments" {
		t.Fatalf("request = %+v", request)
	}
	var body struct{ Name, Mime, Data string }
	if err := json.Unmarshal(request.Body, &body); err != nil {
		t.Fatal(err)
	}
	whole, err := base64.StdEncoding.DecodeString(body.Data)
	if err != nil {
		t.Fatal(err)
	}
	if body.Name != "photo.png" || body.Mime != "image/png" || string(whole) != string(first)+string(second) {
		t.Fatalf("rebuilt body = %+v", body)
	}
}

func TestBulkResumeSkipsHeldParts(t *testing.T) {
	desktop := &recordingDesktop{}
	rig := newMuxRig(t, &Host{desktop: desktop})
	connectionID := strings.Repeat("c", 32)
	channel := rig.greet(t, connectionID, map[string]int{"bulk": 1})
	readDirected(t, rig.controller, channel)

	rig.command(t, channel, connectionID, bulkOpenCommand("up1", 3))
	readDirected(t, rig.controller, channel)
	rig.binary(t, channel, connectionID, 0, []byte("part zero"))

	// The link dropped and came back; the same open answers with what is held.
	rig.command(t, channel, connectionID, bulkOpenCommand("up1", 3))
	if answer := readDirected(t, rig.controller, channel); answer["type"] != "bulk.ready" || answer["next"] != float64(1) {
		t.Fatalf("resume = %+v", answer)
	}
	rig.binary(t, channel, connectionID, 1, []byte("part one"))
	rig.binary(t, channel, connectionID, 2, []byte("part two"))
	readDirected(t, rig.controller, channel)

	seen := desktop.seen()
	if len(seen) != 1 {
		t.Fatalf("desktop ran %d requests, want 1", len(seen))
	}
	var body struct{ Data string }
	if err := json.Unmarshal(seen[0].Body, &body); err != nil {
		t.Fatal(err)
	}
	whole, _ := base64.StdEncoding.DecodeString(body.Data)
	if string(whole) != "part zeropart onepart two" {
		t.Fatalf("reassembled = %q", whole)
	}
}

func TestBulkRefusedWithoutTheCapability(t *testing.T) {
	desktop := &recordingDesktop{}
	rig := newMuxRig(t, &Host{desktop: desktop})
	connectionID := strings.Repeat("c", 32)
	channel := rig.greet(t, connectionID, nil)
	ready := readDirected(t, rig.controller, channel)
	if caps, ok := ready["capabilities"].(map[string]any); ok && caps["bulk"] != nil {
		t.Fatalf("bulk advertised without negotiation: %+v", ready)
	}

	rig.command(t, channel, connectionID, bulkOpenCommand("up1", 1))
	if answer := readDirected(t, rig.controller, channel); answer["type"] != "error" {
		t.Fatalf("bulk.open without capability = %+v", answer)
	}
	payload, err := channel.sealBytes([]byte("bytes"))
	if err != nil {
		t.Fatal(err)
	}
	err = rig.host.handle(t.Context(), rig.writer, rig.saved, rig.private, rig.sessions, mustJSON(t, gatewayMessage{
		Type: "controller_message", ConnectionID: connectionID,
		Scopes: []account.RemoteCapability{account.RemoteDesktop}, Payload: payload,
	}))
	if err == nil {
		t.Fatal("a binary part was admitted without the capability")
	}
	if len(desktop.seen()) != 0 {
		t.Fatal("desktop ran a request from an unnegotiated lane")
	}
}

func TestBulkNeedsAScopeThatCoversThePath(t *testing.T) {
	desktop := &recordingDesktop{}
	rig := newMuxRig(t, &Host{desktop: desktop})
	connectionID := strings.Repeat("c", 32)
	channel := rig.greet(t, connectionID, map[string]int{"bulk": 1})
	readDirected(t, rig.controller, channel)

	payload, err := channel.seal(bulkOpenCommand("up1", 1))
	if err != nil {
		t.Fatal(err)
	}
	rig.dispatch(t, gatewayMessage{
		Type: "controller_message", ConnectionID: connectionID,
		Scopes: []account.RemoteCapability{account.RemoteTasks}, Payload: payload,
	})
	if answer := readDirected(t, rig.controller, channel); answer["type"] != "error" {
		t.Fatalf("bulk.open without a covering scope = %+v", answer)
	}
	if len(desktop.seen()) != 0 {
		t.Fatal("desktop ran a request the scope refused")
	}
}

func TestBulkPartWithoutAnOpenIsDropped(t *testing.T) {
	desktop := &recordingDesktop{}
	rig := newMuxRig(t, &Host{desktop: desktop})
	connectionID := strings.Repeat("c", 32)
	channel := rig.greet(t, connectionID, map[string]int{"bulk": 1})
	readDirected(t, rig.controller, channel)

	rig.binary(t, channel, connectionID, 0, []byte("orphan"))
	// A ping behind the part proves it was processed; nothing may answer for it.
	rig.command(t, channel, connectionID, controllerCommand{Version: 1, Type: "ping", ID: "probe"})
	if answer := readDirected(t, rig.controller, channel); answer["type"] != "pong" {
		t.Fatalf("orphan part answered: %+v", answer)
	}
	if len(desktop.seen()) != 0 {
		t.Fatal("an orphan part reached the desktop")
	}
}

func TestBulkOversizedPartRefusesTheUpload(t *testing.T) {
	desktop := &recordingDesktop{}
	rig := newMuxRig(t, &Host{desktop: desktop})
	connectionID := strings.Repeat("c", 32)
	channel := rig.greet(t, connectionID, map[string]int{"bulk": 1})
	readDirected(t, rig.controller, channel)

	rig.command(t, channel, connectionID, bulkOpenCommand("up1", 2))
	readDirected(t, rig.controller, channel)
	rig.binary(t, channel, connectionID, 0, make([]byte, uploadPartMax+1))
	if answer := readDirected(t, rig.controller, channel); answer["type"] != "error" || answer["id"] != "up1" {
		t.Fatalf("oversized part = %+v", answer)
	}
	// The upload is gone: a following part finds nothing open.
	rig.binary(t, channel, connectionID, 1, []byte("late"))
	rig.command(t, channel, connectionID, controllerCommand{Version: 1, Type: "ping", ID: "probe"})
	if answer := readDirected(t, rig.controller, channel); answer["type"] != "pong" {
		t.Fatalf("part after refusal answered: %+v", answer)
	}
	if len(desktop.seen()) != 0 {
		t.Fatal("a refused upload reached the desktop")
	}
}

func TestBulkOpenRefusesWhileAnotherUploadIsActive(t *testing.T) {
	desktop := &recordingDesktop{}
	rig := newMuxRig(t, &Host{desktop: desktop})
	connectionID := strings.Repeat("c", 32)
	channel := rig.greet(t, connectionID, map[string]int{"bulk": 1})
	readDirected(t, rig.controller, channel)

	rig.command(t, channel, connectionID, bulkOpenCommand("up1", 3))
	readDirected(t, rig.controller, channel)
	rig.binary(t, channel, connectionID, 0, []byte("held"))

	// Replacing a live upload strands it: its next part then fails the
	// innocent replacement with an order error named for the wrong id. A
	// second open is refused instead, and the first upload keeps its parts.
	rig.command(t, channel, connectionID, bulkOpenCommand("up2", 2))
	if answer := readDirected(t, rig.controller, channel); answer["type"] != "error" || answer["id"] != "up2" {
		t.Fatalf("second bulk.open = %+v, want a busy refusal", answer)
	}
	rig.binary(t, channel, connectionID, 1, []byte("part one"))
	rig.binary(t, channel, connectionID, 2, []byte("part two"))
	if answer := readDirected(t, rig.controller, channel); answer["type"] != "desktop.response" || answer["id"] != "up1" {
		t.Fatalf("first upload = %+v, want it completed untouched", answer)
	}
}

func TestBulkPartScopeFailureTellsTheController(t *testing.T) {
	desktop := &recordingDesktop{}
	rig := newMuxRig(t, &Host{desktop: desktop})
	connectionID := strings.Repeat("c", 32)
	channel := rig.greet(t, connectionID, map[string]int{"bulk": 1})
	readDirected(t, rig.controller, channel)

	rig.command(t, channel, connectionID, bulkOpenCommand("up1", 2))
	readDirected(t, rig.controller, channel)

	// A part whose scopes cannot reach the desktop used to drop the upload
	// silently, and the controller waited out its watchdog for parts that
	// would never land. The upload fails with a typed error now.
	framed := make([]byte, 4+3)
	binary.BigEndian.PutUint32(framed[:4], 0)
	copy(framed[4:], []byte("abc"))
	payload, err := channel.sealBytes(framed)
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.host.handle(t.Context(), rig.writer, rig.saved, rig.private, rig.sessions, mustJSON(t, gatewayMessage{
		Type: "controller_message", ConnectionID: connectionID,
		Scopes: []account.RemoteCapability{account.RemoteFiles}, Payload: payload,
	})); err != nil {
		t.Fatal(err)
	}
	if answer := readDirected(t, rig.controller, channel); answer["type"] != "error" || answer["id"] != "up1" {
		t.Fatalf("scoped-out part = %+v, want a typed error for up1", answer)
	}
	if len(desktop.seen()) != 0 {
		t.Fatal("a scoped-out part reached the desktop")
	}
}

func TestBulkGapRefusesRatherThanCorrupts(t *testing.T) {
	desktop := &recordingDesktop{}
	rig := newMuxRig(t, &Host{desktop: desktop})
	connectionID := strings.Repeat("c", 32)
	channel := rig.greet(t, connectionID, map[string]int{"bulk": 1})
	readDirected(t, rig.controller, channel)

	rig.command(t, channel, connectionID, bulkOpenCommand("up1", 3))
	readDirected(t, rig.controller, channel)
	rig.binary(t, channel, connectionID, 0, []byte("held"))
	// Part 1 never arrived (a full queue dropped it); part 2 must not take
	// its place.
	rig.binary(t, channel, connectionID, 2, []byte("skipped ahead"))
	if answer := readDirected(t, rig.controller, channel); answer["type"] != "error" || answer["id"] != "up1" {
		t.Fatalf("gap = %+v", answer)
	}
	if len(desktop.seen()) != 0 {
		t.Fatal("a gapped upload reached the desktop")
	}
}

func TestBinarySealStaysOnItsOwnLane(t *testing.T) {
	rig := newMuxRig(t, &Host{})
	connectionID := strings.Repeat("c", 32)
	channel := rig.greet(t, connectionID, map[string]int{"bulk": 1})
	readDirected(t, rig.controller, channel)

	part := []byte{0, 1, 2, 255, 254}
	wire, err := channel.sealBytes(part)
	if err != nil {
		t.Fatal(err)
	}
	// The device side opens it as bytes; the controller mirror does the same.
	mirror := rig.sessions[connectionID].cipher
	opened, err := mirror.openBytes(wire)
	if err != nil || string(opened) != string(part) {
		t.Fatalf("openBytes = %v, %v", opened, err)
	}
	if _, err := mirror.openBytes(wire); err == nil {
		t.Fatal("a replayed binary part was admitted")
	}
	fresh, err := channel.sealBytes(part)
	if err != nil {
		t.Fatal(err)
	}
	var command controllerCommand
	if err := mirror.open(fresh, &command); err == nil {
		t.Fatal("a binary message opened as a command")
	}
	jsonWire, err := channel.seal(map[string]any{"v": 1, "type": "ping", "id": "p"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mirror.openBytes(jsonWire); err == nil {
		t.Fatal("a command opened as a binary part")
	}
}

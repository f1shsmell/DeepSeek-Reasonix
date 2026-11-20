package remotecloud

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"reasonix/internal/platform/account"
)

type pushStub struct {
	mu      sync.Mutex
	frames  chan EventFrame
	stopped atomic.Int64
	runtime string
	after   int64
	attach  bool
}

func newPushStub() *pushStub { return &pushStub{frames: make(chan EventFrame, 64)} }

func (s *pushStub) CloudEvents(runtimeID string, after int64, attach bool) (EventStream, error) {
	s.mu.Lock()
	s.runtime, s.after, s.attach = runtimeID, after, attach
	s.mu.Unlock()
	return &pushStream{stub: s}, nil
}

func (s *pushStub) subscription() (string, int64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runtime, s.after, s.attach
}

type pushStream struct{ stub *pushStub }

func (s *pushStream) Frames() <-chan EventFrame { return s.stub.frames }
func (s *pushStream) Stop()                     { s.stub.stopped.Add(1) }

type blockingDesktop struct{ release chan struct{} }

func (d *blockingDesktop) CloudDesktop(_ context.Context, _ DesktopRequest, _ string) (DesktopResponse, error) {
	<-d.release
	return DesktopResponse{Status: http.StatusOK, ContentType: "text/plain", Body: []byte("ok")}, nil
}

// muxRig drives host.handle the way the connect loop does, with a real
// WebSocket pair so pushed frames are read off the wire, not a buffer.
type muxRig struct {
	host       *Host
	writer     *relayWriter
	controller *websocket.Conn
	sessions   map[string]*controllerSession
	saved      *identity
	private    *ecdh.PrivateKey
}

func newMuxRig(t *testing.T, host *Host) *muxRig {
	t.Helper()
	connections := make(chan *websocket.Conn, 1)
	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		connections <- conn
		<-done
	}))
	controller, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	deviceWire := <-connections
	if err := controller.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	private, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rig := &muxRig{
		host: host, writer: &relayWriter{conn: deviceWire}, controller: controller,
		sessions: make(map[string]*controllerSession),
		saved:    &identity{DeviceID: strings.Repeat("d", 64)}, private: private,
	}
	t.Cleanup(func() {
		for _, session := range rig.sessions {
			session.stop()
		}
		controller.Close()
		deviceWire.Close()
		close(done)
		server.Close()
	})
	return rig
}

func (r *muxRig) dispatch(t *testing.T, message gatewayMessage) {
	t.Helper()
	if err := r.host.handle(t.Context(), r.writer, r.saved, r.private, r.sessions, mustJSON(t, message)); err != nil {
		t.Fatal(err)
	}
}

func (r *muxRig) greet(t *testing.T, connectionID string, capabilities map[string]int) *sessionCipher {
	t.Helper()
	controller, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		t.Fatal(err)
	}
	greeting := mustJSON(t, hello{
		Version: protocolVersion, Type: "hello", Capabilities: capabilities,
		PublicKey: base64.RawURLEncoding.EncodeToString(controller.PublicKey().Bytes()),
		Salt:      base64.RawURLEncoding.EncodeToString(salt),
	})
	r.dispatch(t, gatewayMessage{Type: "controller_message", ConnectionID: connectionID, Payload: string(greeting)})
	mirror := mustJSON(t, hello{
		Version: protocolVersion, Type: "hello",
		PublicKey: base64.RawURLEncoding.EncodeToString(r.private.PublicKey().Bytes()),
		Salt:      base64.RawURLEncoding.EncodeToString(salt),
	})
	channel, err := newSessionCipher(controller, r.saved.DeviceID, string(mirror))
	if err != nil {
		t.Fatal(err)
	}
	return channel
}

func (r *muxRig) command(t *testing.T, channel *sessionCipher, connectionID string, command controllerCommand) {
	t.Helper()
	payload, err := channel.seal(command)
	if err != nil {
		t.Fatal(err)
	}
	r.dispatch(t, gatewayMessage{
		Type: "controller_message", ConnectionID: connectionID,
		Scopes: []account.RemoteCapability{account.RemoteDesktop}, Payload: payload,
	})
}

func awaitPushStop(t *testing.T, stub *pushStub) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for stub.stopped.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the subscription outlived its controller")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestMuxPushStreamsFramesWithCursor(t *testing.T) {
	stub := newPushStub()
	rig := newMuxRig(t, &Host{events: stub})
	connectionID := strings.Repeat("c", 32)
	channel := rig.greet(t, connectionID, map[string]int{"mux": 1})
	ready := readDirected(t, rig.controller, channel)
	capabilities, _ := ready["capabilities"].(map[string]any)
	if capabilities["mux"] != float64(1) {
		t.Fatalf("ready capabilities = %+v", ready["capabilities"])
	}

	rig.command(t, channel, connectionID, controllerCommand{
		Version: 1, Type: "mux.open", ID: "s1", Runtime: "r1", Attach: true, Credit: 64 << 10,
	})
	if ack := readDirected(t, rig.controller, channel); ack["type"] != "mux.opened" || ack["stream"] != "s1" {
		t.Fatalf("open ack = %+v", ack)
	}
	if runtime, after, attach := stub.subscription(); runtime != "r1" || after != 0 || !attach {
		t.Fatalf("subscription = %q after=%d attach=%v", runtime, after, attach)
	}

	data := `{"kind":"turn_done","seq":7}`
	stub.frames <- EventFrame{Seq: 7, Data: []byte(data)}
	frame := readDirected(t, rig.controller, channel)
	if frame["type"] != "mux.frame" || frame["stream"] != "s1" || frame["seq"] != float64(7) ||
		frame["done"] != true || frame["data"] != data {
		t.Fatalf("pushed frame = %+v", frame)
	}

	rig.command(t, channel, connectionID, controllerCommand{Version: 1, Type: "mux.close", ID: "s1"})
	if ack := readDirected(t, rig.controller, channel); ack["type"] != "mux.closed" || ack["id"] != "s1" {
		t.Fatalf("close ack = %+v", ack)
	}
	awaitPushStop(t, stub)
}

func TestMuxResumeCarriesTheCursor(t *testing.T) {
	stub := newPushStub()
	rig := newMuxRig(t, &Host{events: stub})
	connectionID := strings.Repeat("c", 32)
	channel := rig.greet(t, connectionID, map[string]int{"mux": 1})
	readDirected(t, rig.controller, channel)
	rig.command(t, channel, connectionID, controllerCommand{
		Version: 1, Type: "mux.open", ID: "s1", Runtime: "r1", After: 42, Credit: 64 << 10,
	})
	readDirected(t, rig.controller, channel)
	if _, after, attach := stub.subscription(); after != 42 || attach {
		t.Fatalf("resume subscription after=%d attach=%v, want 42/false", after, attach)
	}
}

func TestMuxOpenFallsBackWithoutTheCapability(t *testing.T) {
	for _, tc := range []struct {
		name         string
		events       EventService
		capabilities map[string]int
	}{
		{"old controller", newPushStub(), nil},
		{"host without an event source", nil, map[string]int{"mux": 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newMuxRig(t, &Host{events: tc.events})
			connectionID := strings.Repeat("c", 32)
			channel := rig.greet(t, connectionID, tc.capabilities)
			ready := readDirected(t, rig.controller, channel)
			if _, advertised := ready["capabilities"]; advertised {
				t.Fatalf("ready advertises push it cannot serve: %+v", ready)
			}
			rig.command(t, channel, connectionID, controllerCommand{
				Version: 1, Type: "mux.open", ID: "s1", Runtime: "r1", Attach: true, Credit: 64 << 10,
			})
			if answer := readDirected(t, rig.controller, channel); answer["type"] != "error" || answer["id"] != "s1" {
				t.Fatalf("mux.open without push = %+v, want a typed error the phone can fall back on", answer)
			}
		})
	}
}

func TestMuxChunksLargeFrames(t *testing.T) {
	stub := newPushStub()
	rig := newMuxRig(t, &Host{events: stub})
	connectionID := strings.Repeat("c", 32)
	channel := rig.greet(t, connectionID, map[string]int{"mux": 1})
	readDirected(t, rig.controller, channel)
	rig.command(t, channel, connectionID, controllerCommand{
		Version: 1, Type: "mux.open", ID: "s1", Runtime: "r1", Attach: true, Credit: 1 << 20,
	})
	readDirected(t, rig.controller, channel)

	data := bytes.Repeat([]byte("x"), 2*muxFrameChunk+17)
	stub.frames <- EventFrame{Seq: 9, Data: data}
	var joined []byte
	for index := range 3 {
		frame := readDirected(t, rig.controller, channel)
		if frame["type"] != "mux.frame" || frame["index"] != float64(index) || frame["seq"] != float64(9) {
			t.Fatalf("chunk %d = %+v", index, frame)
		}
		if frame["done"] != (index == 2) {
			t.Fatalf("chunk %d done = %v", index, frame["done"])
		}
		joined = append(joined, frame["data"].(string)...)
	}
	if !bytes.Equal(joined, data) {
		t.Fatal("chunks did not reassemble into the original frame")
	}
}

func TestMuxCreditTimeoutClosesOnlyTheStarvedStream(t *testing.T) {
	previous := muxCreditWait
	muxCreditWait = 150 * time.Millisecond
	t.Cleanup(func() { muxCreditWait = previous })

	stub := newPushStub()
	rig := newMuxRig(t, &Host{events: stub})
	starvedID := strings.Repeat("a", 32)
	liveID := strings.Repeat("b", 32)
	starved := rig.greet(t, starvedID, map[string]int{"mux": 1})
	readDirected(t, rig.controller, starved)
	live := rig.greet(t, liveID, map[string]int{"mux": 1})
	readDirected(t, rig.controller, live)

	rig.command(t, starved, starvedID, controllerCommand{
		Version: 1, Type: "mux.open", ID: "s1", Runtime: "r1", Attach: true, Credit: 8,
	})
	readDirected(t, rig.controller, starved)
	stub.frames <- EventFrame{Seq: 1, Data: []byte(`{"kind":"notice","seq":1}`)}

	rig.command(t, live, liveID, controllerCommand{Version: 1, Type: "ping", ID: "probe"})
	if pong := readDirected(t, rig.controller, live); pong["type"] != "pong" {
		t.Fatalf("a starved push stream blocked another controller: %+v", pong)
	}
	closed := readDirected(t, rig.controller, starved)
	if closed["type"] != "mux.closed" || closed["stream"] != "s1" || closed["reason"] != "credit_timeout" {
		t.Fatalf("starved stream = %+v, want credit_timeout", closed)
	}
	awaitPushStop(t, stub)
}

func TestMuxCreditGrantReplenishesTheStream(t *testing.T) {
	stub := newPushStub()
	rig := newMuxRig(t, &Host{events: stub})
	connectionID := strings.Repeat("c", 32)
	channel := rig.greet(t, connectionID, map[string]int{"mux": 1})
	readDirected(t, rig.controller, channel)
	data := []byte(`{"kind":"notice","seq":1}`)
	rig.command(t, channel, connectionID, controllerCommand{
		Version: 1, Type: "mux.open", ID: "s1", Runtime: "r1", Attach: true, Credit: int64(len(data)),
	})
	readDirected(t, rig.controller, channel)

	stub.frames <- EventFrame{Seq: 1, Data: data}
	if frame := readDirected(t, rig.controller, channel); frame["seq"] != float64(1) {
		t.Fatalf("first frame = %+v", frame)
	}
	stub.frames <- EventFrame{Seq: 2, Data: data}
	rig.command(t, channel, connectionID, controllerCommand{Version: 1, Type: "mux.credit", ID: "s1", Credit: 1024})
	ack := readDirected(t, rig.controller, channel)
	if ack["type"] != "mux.credit" || ack["credit"] != float64(1024) {
		t.Fatalf("credit ack = %+v", ack)
	}
	if frame := readDirected(t, rig.controller, channel); frame["seq"] != float64(2) {
		t.Fatalf("frame after replenishment = %+v", frame)
	}

	rig.command(t, channel, connectionID, controllerCommand{Version: 1, Type: "mux.credit", ID: "gone", Credit: 1})
	if answer := readDirected(t, rig.controller, channel); answer["type"] != "error" {
		t.Fatalf("credit for an unknown stream = %+v", answer)
	}
}

func TestSlowDesktopCommandDoesNotBlockAnotherSession(t *testing.T) {
	release := make(chan struct{})
	rig := newMuxRig(t, &Host{desktop: &blockingDesktop{release: release}})
	slowID := strings.Repeat("a", 32)
	fastID := strings.Repeat("b", 32)
	slow := rig.greet(t, slowID, nil)
	readDirected(t, rig.controller, slow)
	fast := rig.greet(t, fastID, nil)
	readDirected(t, rig.controller, fast)

	rig.command(t, slow, slowID, controllerCommand{Version: 1, Type: "desktop.request", ID: "d1", Method: http.MethodGet, Path: "/history"})
	rig.command(t, fast, fastID, controllerCommand{Version: 1, Type: "ping", ID: "probe"})
	if pong := readDirected(t, rig.controller, fast); pong["type"] != "pong" {
		t.Fatalf("a slow desktop request blocked another controller: %+v", pong)
	}
	close(release)
	if answer := readDirected(t, rig.controller, slow); answer["type"] != "desktop.response" || answer["id"] != "d1" {
		t.Fatalf("slow request answer = %+v", answer)
	}
}

func TestMuxSubscriptionsDieWithTheirController(t *testing.T) {
	stub := newPushStub()
	presence := &presenceStub{}
	rig := newMuxRig(t, &Host{events: stub, presence: presence})
	connectionID := strings.Repeat("c", 32)
	channel := rig.greet(t, connectionID, map[string]int{"mux": 1})
	readDirected(t, rig.controller, channel)
	rig.command(t, channel, connectionID, controllerCommand{
		Version: 1, Type: "mux.open", ID: "s1", Runtime: "r1", Attach: true, Credit: 64 << 10,
	})
	readDirected(t, rig.controller, channel)

	rig.dispatch(t, gatewayMessage{Type: "controller_disconnected", ConnectionID: connectionID})
	awaitPushStop(t, stub)
	if presence.disconnected != connectionID {
		t.Fatalf("disconnected = %q", presence.disconnected)
	}
	if _, held := rig.sessions[connectionID]; held {
		t.Fatal("the disconnected session was kept")
	}
}

func TestMuxChunksRespectRuneBoundaries(t *testing.T) {
	stub := newPushStub()
	rig := newMuxRig(t, &Host{events: stub})
	connectionID := strings.Repeat("c", 32)
	channel := rig.greet(t, connectionID, map[string]int{"mux": 1})
	readDirected(t, rig.controller, channel)
	rig.command(t, channel, connectionID, controllerCommand{
		Version: 1, Type: "mux.open", ID: "s1", Runtime: "r1", Attach: true, Credit: 1 << 20,
	})
	readDirected(t, rig.controller, channel)

	// A three-byte rune straddles the 16 KiB boundary the byte-based splitter
	// would cut at; json.Marshal would turn the torn sequence into U+FFFD.
	data := append(bytes.Repeat([]byte("x"), muxFrameChunk-1), []byte("中剩余的中文内容")...)
	data = append(data, bytes.Repeat([]byte("y"), 100)...)
	stub.frames <- EventFrame{Seq: 9, Data: data}
	var joined []byte
	for index := 0; ; index++ {
		frame := readDirected(t, rig.controller, channel)
		if frame["type"] != "mux.frame" || frame["stream"] != "s1" || frame["seq"] != float64(9) {
			t.Fatalf("chunk %d = %+v", index, frame)
		}
		if frame["index"] != float64(index) {
			t.Fatalf("chunk %d carries index %v", index, frame["index"])
		}
		joined = append(joined, frame["data"].(string)...)
		if frame["done"] == true {
			break
		}
	}
	if !bytes.Equal(joined, data) {
		t.Fatal("chunks did not reassemble into the original frame")
	}
	if bytes.ContainsRune(joined, '\uFFFD') {
		t.Fatal("a torn UTF-8 sequence was replaced with U+FFFD")
	}
}

func TestMuxOpenedAckPrecedesFirstFrame(t *testing.T) {
	stub := newPushStub()
	rig := newMuxRig(t, &Host{events: stub})
	connectionID := strings.Repeat("c", 32)
	channel := rig.greet(t, connectionID, map[string]int{"mux": 1})
	readDirected(t, rig.controller, channel)

	// The frame is queued before the open, so a pump that starts before the
	// ack is written wins the writer and the controller sees an orphan frame.
	stub.frames <- EventFrame{Seq: 1, Data: []byte(`{"kind":"notice"}`)}
	rig.command(t, channel, connectionID, controllerCommand{
		Version: 1, Type: "mux.open", ID: "s1", Runtime: "r1", Attach: true, Credit: 64 << 10,
	})
	if ack := readDirected(t, rig.controller, channel); ack["type"] != "mux.opened" {
		t.Fatalf("first reply after mux.open = %+v, want the opened ack before any frame", ack)
	}
	if frame := readDirected(t, rig.controller, channel); frame["type"] != "mux.frame" {
		t.Fatalf("second reply after mux.open = %+v, want the pushed frame", frame)
	}
}

func TestMuxSourceEndClosesTheStream(t *testing.T) {
	stub := newPushStub()
	rig := newMuxRig(t, &Host{events: stub})
	connectionID := strings.Repeat("c", 32)
	channel := rig.greet(t, connectionID, map[string]int{"mux": 1})
	readDirected(t, rig.controller, channel)
	rig.command(t, channel, connectionID, controllerCommand{
		Version: 1, Type: "mux.open", ID: "s1", Runtime: "r1", Attach: true, Credit: 64 << 10,
	})
	readDirected(t, rig.controller, channel)

	close(stub.frames)
	closed := readDirected(t, rig.controller, channel)
	if closed["type"] != "mux.closed" || closed["stream"] != "s1" || closed["reason"] != "source_ended" {
		t.Fatalf("source end = %+v, want a source_ended close", closed)
	}
	awaitPushStop(t, stub)
}

func TestMuxOpenRefusesAfterSessionStop(t *testing.T) {
	stub := newPushStub()
	rig := newMuxRig(t, &Host{events: stub})
	connectionID := strings.Repeat("c", 32)
	channel := rig.greet(t, connectionID, map[string]int{"mux": 1})
	readDirected(t, rig.controller, channel)
	session := rig.sessions[connectionID]
	rig.dispatch(t, gatewayMessage{Type: "controller_disconnected", ConnectionID: connectionID})

	// Drive muxCommand directly: the session's command goroutine has exited,
	// so a queued open would never be processed. An open that arrives after
	// the stop must be refused and its subscription released, not parked on a
	// dead session for the credit timeout to reap.
	command := controllerCommand{Version: 1, Type: "mux.open", ID: "s1", Runtime: "r1", Attach: true, Credit: 64 << 10}
	queued := sessionCommand{writer: rig.writer, scopes: []account.RemoteCapability{account.RemoteDesktop}, command: command}
	rig.host.muxCommand(session, queued)
	if answer := readDirected(t, rig.controller, channel); answer["type"] != "error" || answer["id"] != "s1" {
		t.Fatalf("mux.open on a stopped session = %+v, want a typed refusal", answer)
	}
	awaitPushStop(t, stub)
}

func TestMuxCreditSurvivesABlockedDesktopRequest(t *testing.T) {
	previous := muxCreditWait
	muxCreditWait = 300 * time.Millisecond
	t.Cleanup(func() { muxCreditWait = previous })

	release := make(chan struct{})
	stub := newPushStub()
	rig := newMuxRig(t, &Host{events: stub, desktop: &blockingDesktop{release: release}})
	t.Cleanup(func() { close(release) })
	connectionID := strings.Repeat("c", 32)
	channel := rig.greet(t, connectionID, map[string]int{"mux": 1})
	readDirected(t, rig.controller, channel)
	rig.command(t, channel, connectionID, controllerCommand{
		Version: 1, Type: "mux.open", ID: "s1", Runtime: "r1", Attach: true, Credit: 8,
	})
	readDirected(t, rig.controller, channel)
	data := bytes.Repeat([]byte("z"), 100)
	stub.frames <- EventFrame{Seq: 1, Data: data}

	// Park the session goroutine on a desktop request, then grant credit.
	// A credit that waits behind it lets the starved stream time out.
	rig.command(t, channel, connectionID, controllerCommand{Version: 1, Type: "desktop.request", ID: "d1", Method: http.MethodGet, Path: "/history"})
	time.Sleep(100 * time.Millisecond)
	rig.command(t, channel, connectionID, controllerCommand{Version: 1, Type: "mux.credit", ID: "s1", Credit: 200})
	if ack := readDirected(t, rig.controller, channel); ack["type"] != "mux.credit" || ack["credit"] != float64(208) {
		t.Fatalf("credit behind a blocked desktop request = %+v, want the grant ack", ack)
	}
	if frame := readDirected(t, rig.controller, channel); frame["type"] != "mux.frame" {
		t.Fatalf("after the grant = %+v, want the pushed frame", frame)
	}
}

func TestReplayCacheSurvivesConcurrentSessions(t *testing.T) {
	var cache replayCache
	var wg sync.WaitGroup
	for worker := range 8 {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for round := range 200 {
				key := fmt.Sprintf("%d-%d", worker, round%16)
				now := time.Now()
				cache.put(key, []map[string]any{{"id": key}}, 4, now)
				cache.get(key, now)
				if round%50 == 0 {
					cache.reset()
				}
			}
		}(worker)
	}
	wg.Wait()
}

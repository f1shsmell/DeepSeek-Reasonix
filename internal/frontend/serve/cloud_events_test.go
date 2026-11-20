package serve

import (
	"strings"
	"testing"
	"time"

	"reasonix/internal/base/testenv"
	"reasonix/internal/contract/event"
	"reasonix/internal/platform/remotecloud"
)

func nextPushFrame(t *testing.T, stream remotecloud.EventStream) remotecloud.EventFrame {
	t.Helper()
	select {
	case frame, ok := <-stream.Frames():
		if !ok {
			t.Fatal("the push stream closed before the frame arrived")
		}
		return frame
	case <-time.After(5 * time.Second):
		t.Fatal("no push frame arrived")
		return remotecloud.EventFrame{}
	}
}

func TestCloudEventsAttachesWithWatermarkAndPushesLiveFrames(t *testing.T) {
	t.Setenv("REASONIX_HOME", testenv.TempDir(t))
	hub := NewHub(HubOptions{})
	runtime := hubRuntime(t, hub, testenv.TempDir(t))
	stream, err := hub.CloudEvents(runtime.ID, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Stop()

	first := nextPushFrame(t, stream)
	if kindOf(t, first.Data) != "stream_watermark" || first.Seq != 0 {
		t.Fatalf("attach frame = %+v, want an unnumbered watermark", first)
	}
	runtime.Events.Emit(event.Event{Kind: event.TurnDone})
	live := nextPushFrame(t, stream)
	if kindOf(t, live.Data) != "turn_done" || live.Seq == 0 {
		t.Fatalf("live frame = %+v, want a numbered turn_done", live)
	}
}

func TestCloudEventsKeepsTheWatermarkMoving(t *testing.T) {
	previous := cloudEventsKeepalive
	cloudEventsKeepalive = 20 * time.Millisecond
	t.Cleanup(func() { cloudEventsKeepalive = previous })

	t.Setenv("REASONIX_HOME", testenv.TempDir(t))
	hub := NewHub(HubOptions{})
	runtime := hubRuntime(t, hub, testenv.TempDir(t))
	stream, err := hub.CloudEvents(runtime.ID, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Stop()
	nextPushFrame(t, stream)
	runtime.Events.Emit(event.Event{Kind: event.TurnDone})
	nextPushFrame(t, stream)

	beat := nextPushFrame(t, stream)
	if kindOf(t, beat.Data) != "stream_watermark" || beat.Seq != 0 {
		t.Fatalf("keepalive = %+v, want an unnumbered watermark", beat)
	}
}

func TestCloudEventsResumesFromTheCursor(t *testing.T) {
	t.Setenv("REASONIX_HOME", testenv.TempDir(t))
	hub := NewHub(HubOptions{})
	runtime := hubRuntime(t, hub, testenv.TempDir(t))
	for _, id := range []string{"t1", "t2", "t3"} {
		runtime.Events.Emit(event.Event{Kind: event.ToolResult, Tool: event.Tool{ID: id, Name: "bash"}})
	}
	stream, err := hub.CloudEvents(runtime.ID, 1, false)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Stop()
	for _, want := range []string{"t2", "t3"} {
		frame := nextPushFrame(t, stream)
		if frame.Seq == 0 || !strings.Contains(string(frame.Data), `"id":"`+want+`"`) {
			t.Fatalf("replayed frame = %+v, want %s", frame, want)
		}
	}
	// A resume still ends its replay with the attach watermark: the cursor the
	// client adopts before live frames start flowing.
	if mark := nextPushFrame(t, stream); kindOf(t, mark.Data) != "stream_watermark" || mark.Seq != 0 {
		t.Fatalf("resume watermark = %+v", mark)
	}
	runtime.Events.Emit(event.Event{Kind: event.TurnDone})
	if live := nextPushFrame(t, stream); kindOf(t, live.Data) != "turn_done" {
		t.Fatalf("live frame after resume = %+v", live)
	}
}

func TestCloudEventsDefaultRuntimeIsTheFirstLocal(t *testing.T) {
	t.Setenv("REASONIX_HOME", testenv.TempDir(t))
	hub := NewHub(HubOptions{})
	runtime := hubRuntime(t, hub, testenv.TempDir(t))
	stream, err := hub.CloudEvents("", 0, true)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Stop()
	nextPushFrame(t, stream)
	runtime.Events.Emit(event.Event{Kind: event.TurnDone})
	if live := nextPushFrame(t, stream); kindOf(t, live.Data) != "turn_done" {
		t.Fatalf("default runtime frame = %+v", live)
	}
}

func TestCloudEventsRefusesWhatItCannotServe(t *testing.T) {
	hub := NewHub(HubOptions{})
	if _, err := hub.CloudEvents("missing", 0, true); err == nil {
		t.Fatal("an unknown runtime was subscribed")
	}
	if _, err := hub.CloudEvents("", 0, true); err == nil {
		t.Fatal("a hub with no local runtime was subscribed")
	}
}

func TestCloudEventsStopClosesTheFrames(t *testing.T) {
	t.Setenv("REASONIX_HOME", testenv.TempDir(t))
	hub := NewHub(HubOptions{})
	runtime := hubRuntime(t, hub, testenv.TempDir(t))
	stream, err := hub.CloudEvents(runtime.ID, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	nextPushFrame(t, stream)
	stream.Stop()
	stream.Stop()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-stream.Frames():
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("Stop left the frames channel open")
		}
	}
}

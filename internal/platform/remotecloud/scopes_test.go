package remotecloud

import (
	"net/http"
	"strings"
	"testing"

	"reasonix/internal/platform/account"
)

func TestTunnelScopeTable(t *testing.T) {
	for _, tc := range []struct {
		path string
		want account.RemoteCapability
	}{
		{"/workspace/file?path=a.txt", account.RemoteFiles},
		{"/rt/r1/workspace/files", account.RemoteFiles},
		{"/rt/r1/attachments", account.RemoteFiles},
		{"/attachments/upload", account.RemoteFiles},
		{"/workspace/preview/content", account.RemoteFiles},
		{"/terminals", account.RemoteTerminal},
		{"/rt/r1/terminals/t1/input", account.RemoteTerminal},
		{"/submit", account.RemoteDesktop},
		{"/rt/r1/submit", account.RemoteDesktop},
		{"/workspace/file/../submit", account.RemoteDesktop},
		{"/rt/r1/workspace/file/../../submit", account.RemoteDesktop},
		{"/rt/r1", account.RemoteDesktop},
		{"/rt/", account.RemoteDesktop},
		{"", account.RemoteDesktop},
		{"/workspace/file/", account.RemoteFiles},
	} {
		if got := tunnelScope(tc.path); got != tc.want {
			t.Errorf("tunnelScope(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestTunnelFilesGrantReachesOnlyMappedPaths(t *testing.T) {
	desktop := &recordingDesktop{}
	rig := newMuxRig(t, &Host{desktop: desktop})
	connectionID := strings.Repeat("c", 32)
	channel := rig.greet(t, connectionID, nil)
	readDirected(t, rig.controller, channel)
	files := []account.RemoteCapability{account.RemoteFiles}

	for _, path := range []string{"/workspace/file?path=a.txt", "/rt/r1/workspace/files", "/rt/r1/attachments"} {
		rig.commandWithScopes(t, channel, connectionID, files, controllerCommand{
			Version: 1, Type: "desktop.request", ID: path, Method: http.MethodGet, Path: path,
		})
		if answer := readDirected(t, rig.controller, channel); answer["type"] != "desktop.response" || answer["id"] != path {
			t.Fatalf("files grant on %q = %+v", path, answer)
		}
	}
	for _, path := range []string{"/submit", "/rt/r1/submit", "/workspace/file/../submit", "/terminals"} {
		rig.commandWithScopes(t, channel, connectionID, files, controllerCommand{
			Version: 1, Type: "desktop.request", ID: path, Method: http.MethodGet, Path: path,
		})
		if answer := readDirected(t, rig.controller, channel); answer["type"] != "error" || answer["id"] != path {
			t.Fatalf("files grant on unmapped %q = %+v, want a refusal", path, answer)
		}
	}
	if seen := desktop.seen(); len(seen) != 3 {
		t.Fatalf("desktop ran %d requests, want only the 3 mapped ones", len(seen))
	}
}

func TestTunnelDesktopGrantKeepsEveryPath(t *testing.T) {
	desktop := &recordingDesktop{}
	rig := newMuxRig(t, &Host{desktop: desktop})
	connectionID := strings.Repeat("c", 32)
	channel := rig.greet(t, connectionID, nil)
	readDirected(t, rig.controller, channel)
	for _, path := range []string{"/submit", "/workspace/file", "/terminals", "/rt/r1/status"} {
		rig.command(t, channel, connectionID, controllerCommand{
			Version: 1, Type: "desktop.request", ID: path, Method: http.MethodGet, Path: path,
		})
		if answer := readDirected(t, rig.controller, channel); answer["type"] != "desktop.response" {
			t.Fatalf("desktop grant on %q = %+v", path, answer)
		}
	}
}

func TestTunnelTerminalGrantReachesOnlyTerminals(t *testing.T) {
	desktop := &recordingDesktop{}
	rig := newMuxRig(t, &Host{desktop: desktop})
	connectionID := strings.Repeat("c", 32)
	channel := rig.greet(t, connectionID, nil)
	readDirected(t, rig.controller, channel)
	terminal := []account.RemoteCapability{account.RemoteTerminal}

	rig.commandWithScopes(t, channel, connectionID, terminal, controllerCommand{
		Version: 1, Type: "desktop.request", ID: "t1", Method: http.MethodGet, Path: "/rt/r1/terminals",
	})
	if answer := readDirected(t, rig.controller, channel); answer["type"] != "desktop.response" {
		t.Fatalf("terminal grant on /terminals = %+v", answer)
	}
	rig.commandWithScopes(t, channel, connectionID, terminal, controllerCommand{
		Version: 1, Type: "desktop.request", ID: "f1", Method: http.MethodGet, Path: "/workspace/file",
	})
	if answer := readDirected(t, rig.controller, channel); answer["type"] != "error" {
		t.Fatalf("terminal grant on /workspace/file = %+v, want a refusal", answer)
	}
}

func TestTunnelSessionCommandsStillNeedTheirOwnScope(t *testing.T) {
	rig := newMuxRig(t, &Host{tasks: &taskStub{}})
	connectionID := strings.Repeat("c", 32)
	channel := rig.greet(t, connectionID, nil)
	readDirected(t, rig.controller, channel)
	rig.commandWithScopes(t, channel, connectionID, []account.RemoteCapability{account.RemoteFiles}, controllerCommand{
		Version: 1, Type: "tasks.list", ID: "list",
	})
	if answer := readDirected(t, rig.controller, channel); answer["type"] != "error" {
		t.Fatalf("files grant on tasks.list = %+v, want the tasks scope refusal", answer)
	}
}

func TestTunnelBulkOpenFollowsTheTable(t *testing.T) {
	desktop := &recordingDesktop{}
	rig := newMuxRig(t, &Host{desktop: desktop})
	connectionID := strings.Repeat("c", 32)
	channel := rig.greet(t, connectionID, map[string]int{"bulk": 1})
	readDirected(t, rig.controller, channel)
	files := []account.RemoteCapability{account.RemoteFiles}

	rig.commandWithScopes(t, channel, connectionID, files, bulkOpenCommand("up1", 1))
	if answer := readDirected(t, rig.controller, channel); answer["type"] != "bulk.ready" || answer["next"] != float64(0) {
		t.Fatalf("files grant bulk.open on /attachments = %+v", answer)
	}
	command := bulkOpenCommand("up2", 1)
	command.Path = "/rt/r1/submit"
	rig.commandWithScopes(t, channel, connectionID, files, command)
	if answer := readDirected(t, rig.controller, channel); answer["type"] != "error" || answer["id"] != "up2" {
		t.Fatalf("files grant bulk.open on /submit = %+v, want a refusal", answer)
	}
}

func TestTunnelBulkPartsFollowTheHeldPath(t *testing.T) {
	desktop := &recordingDesktop{}
	rig := newMuxRig(t, &Host{desktop: desktop})
	connectionID := strings.Repeat("c", 32)
	channel := rig.greet(t, connectionID, map[string]int{"bulk": 1})
	readDirected(t, rig.controller, channel)
	files := []account.RemoteCapability{account.RemoteFiles}

	rig.commandWithScopes(t, channel, connectionID, files, bulkOpenCommand("up1", 1))
	readDirected(t, rig.controller, channel)
	rig.binaryWithScopes(t, channel, connectionID, files, 0, []byte("bytes"))
	if answer := readDirected(t, rig.controller, channel); answer["type"] != "desktop.response" || answer["id"] != "up1" {
		t.Fatalf("files grant bulk part = %+v", answer)
	}
	if seen := desktop.seen(); len(seen) != 1 || seen[0].Path != "/rt/r1/attachments" {
		t.Fatalf("desktop saw %+v", seen)
	}
}

func TestTunnelUploadPartsFollowTheHeldPath(t *testing.T) {
	desktop := &recordingDesktop{}
	rig := newMuxRig(t, &Host{desktop: desktop})
	connectionID := strings.Repeat("c", 32)
	channel := rig.greet(t, connectionID, nil)
	readDirected(t, rig.controller, channel)
	files := []account.RemoteCapability{account.RemoteFiles}

	// The first part names the path; the second omits it, as the page's do.
	rig.commandWithScopes(t, channel, connectionID, files, part("up1", 0, 2, []byte("head"), http.MethodPost, "/rt/r1/attachments"))
	rig.commandWithScopes(t, channel, connectionID, files, part("up1", 1, 2, []byte("tail"), http.MethodPost, "/rt/r1/attachments"))
	if answer := readDirected(t, rig.controller, channel); answer["type"] != "desktop.response" || answer["id"] != "up1" {
		t.Fatalf("files grant upload = %+v", answer)
	}

	rig.commandWithScopes(t, channel, connectionID, files, part("up2", 0, 2, []byte("head"), http.MethodPost, "/rt/r1/submit"))
	if answer := readDirected(t, rig.controller, channel); answer["type"] != "error" || answer["id"] != "up2" {
		t.Fatalf("files grant upload on /submit = %+v, want a refusal", answer)
	}
	if seen := desktop.seen(); len(seen) != 1 {
		t.Fatalf("desktop ran %d requests, want only the attachments upload", len(seen))
	}
}

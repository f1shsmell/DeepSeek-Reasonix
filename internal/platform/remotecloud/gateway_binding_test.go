package remotecloud

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"reasonix/internal/platform/account"
)

func hostAnswering(t *testing.T, devices []account.RemoteDevice) *Host {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/me/devices" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"devices": devices})
	}))
	t.Cleanup(srv.Close)
	return New(account.New(srv.URL, "test", srv.Client()), nil, "", "test")
}

// A saved identity belongs to one gateway. Pointing the machine at another (or
// back at the hosted one) has to register the device again, or the account
// service would keep answering for it at the old gateway.
func TestASavedIdentityIsCurrentOnlyForTheGatewayItWasRegisteredAt(t *testing.T) {
	saved := &identity{DeviceID: "dev"}
	device := func(gateway string) []account.RemoteDevice {
		return []account.RemoteDevice{{ID: "dev", Capabilities: []account.RemoteCapability{account.RemoteDesktop}, GatewayID: gateway}}
	}
	cases := []struct {
		name    string
		bound   string
		devices []account.RemoteDevice
		want    bool
	}{
		{"hosted device, hosted host", "", device(""), true},
		{"self-hosted device, same gateway", "g1", device("g1"), true},
		{"self-hosted device, host moved to the hosted gateway", "", device("g1"), false},
		{"hosted device, host moved to a self-hosted gateway", "g1", device(""), false},
		{"self-hosted device, host moved to another gateway", "g2", device("g1"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := hostAnswering(t, tc.devices)
			h.SetGateway(tc.bound)
			if got := h.identityCurrent(context.Background(), "token", saved); got != tc.want {
				t.Fatalf("identityCurrent = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestARelayThatIsNotWSSIsRefusedBeforeAnyCredentialIsSent(t *testing.T) {
	for _, url := range []string{"ws://relay.example.com", "http://relay.example.com", "relay.example.com"} {
		h := New(account.New("", "test", nil), nil, url, "test")
		err := h.connect(context.Background(), "token", &identity{DeviceID: "dev", DeviceCredential: "secret"}, nil)
		if err == nil || unavailableReasonFor(err) != unavailableRelayRefused {
			t.Fatalf("connect(%q) = %v (reason %q), want a typed relay refusal", url, err, unavailableReasonFor(err))
		}
	}
}

func TestRelaySecureAdmitsWSSAndOnlyLoopbackWS(t *testing.T) {
	for url, want := range map[string]bool{
		"wss://relay.example.com": true, "ws://127.0.0.1:8080": true, "ws://localhost:9": true, "ws://[::1]:1": true,
		"ws://relay.example.com": false, "ws://10.0.0.5": false, "https://relay.example.com": false, "wss://": false, "": false,
	} {
		if got := relaySecure(url); got != want {
			t.Errorf("relaySecure(%q) = %v, want %v", url, got, want)
		}
	}
}

// An https origin is how every other surface spells a relay, so the desktop
// reads it the same way the phone client does instead of demanding a second
// spelling of one address in one deployment.
func TestAnHTTPSRelayOriginIsDialledOverWSS(t *testing.T) {
	for raw, want := range map[string]string{
		"https://relay.example.com":      "wss://relay.example.com",
		"https://relay.example.com/":     "wss://relay.example.com",
		"  https://relay.example.com  ":  "wss://relay.example.com",
		"https://relay.example.com:8443": "wss://relay.example.com:8443",
		// The scheme is matched case-insensitively, because a person typing an
		// address into an env var does not think of it as a token.
		"HTTPS://relay.example.com": "wss://relay.example.com",
		// Anything the rewrite does not understand is left for relaySecure to
		// refuse, rather than silently turned into something dialable.
		"wss://relay.example.com":       "wss://relay.example.com",
		"http://relay.example.com":      "http://relay.example.com",
		"https://relay.example.com/path": "https://relay.example.com/path",
		// Credentials, a query and a fragment are not part of an origin. Dropping
		// them silently would dial a host the operator did not name, so the
		// string is handed back untouched and refused downstream.
		"https://user:pw@relay.example.com": "https://user:pw@relay.example.com",
		"https://relay.example.com?x=1":     "https://relay.example.com?x=1",
		"https://relay.example.com#f":       "https://relay.example.com#f",
		"":                                  "",
	} {
		if got := normalizeRelayURL(raw); got != want {
			t.Errorf("normalizeRelayURL(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestSetGatewayTrimsTheID(t *testing.T) {
	h := New(account.New("", "test", nil), nil, "", "test")
	h.SetGateway("  g1 \n")
	if h.gatewayID != "g1" {
		t.Fatalf("gatewayID = %q", h.gatewayID)
	}
}

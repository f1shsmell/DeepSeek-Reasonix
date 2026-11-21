package serve

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"reasonix/internal/base/testenv"
)

// gateOn is a gate standing where a door stands, with whatever public name is
// in front of it. The handler is the kernel stand-in: what this file watches
// is the gate's own verdicts.
func gateOn(t *testing.T, origin, public string) *httptest.Server {
	t.Helper()
	t.Setenv("REASONIX_HOME", testenv.TempDir(t))
	gate := NewDeviceGate(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), DeviceGateOptions{
		Registry: NewDeviceRegistry(), Origin: origin, PublicOrigin: public,
		Page: fstest.MapFS{"index.html": {Data: []byte("x")}}, Machine: "desk",
	})
	srv := httptest.NewServer(gate)
	t.Cleanup(srv.Close)
	return srv
}

// named sends one request as a client would: to the proxy's address, but
// saying the Host and Origin the path it took would.
func named(t *testing.T, srv *httptest.Server, method, path, host, origin, body string, cookie *http.Cookie) (*http.Response, *http.Cookie) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	var set *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == DeviceCookie {
			set = c
		}
	}
	return resp, set
}

const (
	lanDoor   = "http://192.168.1.20:41234"
	proxyName = "https://studio.example.com"
)

// The phone through the tunnel arrives named by the proxy, and the proxy's
// origin is the state-changing origin the gate admits beside the door's own.
func TestAPublicOriginAnswersForItsProxy(t *testing.T) {
	reg := NewDeviceRegistry()
	gate := NewDeviceGate(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), DeviceGateOptions{
		Registry: reg, Origin: "http://127.0.0.1:41234", PublicOrigin: proxyName,
		Page: fstest.MapFS{"index.html": {Data: []byte("x")}}, Machine: "desk",
	})
	srv := httptest.NewServer(gate)
	t.Cleanup(srv.Close)

	code, _ := reg.Offer("http://127.0.0.1:41234")
	resp, set := named(t, srv, http.MethodPost, PairPath, "studio.example.com", proxyName, `{"code":"`+code+`"}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pair through the proxy = %d", resp.StatusCode)
	}
	if set == nil {
		t.Fatal("pairing set no device cookie")
	}
	// The browser that holds this cookie sits on https://studio.example.com;
	// a cookie it would not keep is a pairing it loses on the way in.
	if !set.Secure {
		t.Fatal("pairing through an https name set a cookie without Secure")
	}

	resp, _ = named(t, srv, http.MethodPost, "/anything", "studio.example.com", proxyName, "{}", &http.Cookie{Name: DeviceCookie, Value: set.Value})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a paired phone through the proxy = %d", resp.StatusCode)
	}
}

// A proxy that rewrites the Origin's authority along with the Host leaves the
// page's scheme behind: the door the Host matched, tightened to TLS.
func TestAProxyThatRewritesTheOriginIsStillTheProxy(t *testing.T) {
	reg := NewDeviceRegistry()
	gate := NewDeviceGate(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), DeviceGateOptions{
		Registry: reg, Origin: "http://127.0.0.1:41234", PublicOrigin: proxyName,
		Page: fstest.MapFS{"index.html": {Data: []byte("x")}}, Machine: "desk",
	})
	srv := httptest.NewServer(gate)
	t.Cleanup(srv.Close)

	code, _ := reg.Offer("http://127.0.0.1:41234")
	resp, _ := named(t, srv, http.MethodPost, PairPath, "127.0.0.1:41234", "https://127.0.0.1:41234", `{"code":"`+code+`"}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pair through a rewriting proxy = %d", resp.StatusCode)
	}
}

// With no proxy named there is nothing to tighten against: a page claiming the
// door's own address over TLS is a stranger's spelling, and it is refused.
func TestATightenedOriginWithoutAProxyIsRefused(t *testing.T) {
	reg := NewDeviceRegistry()
	gate := NewDeviceGate(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), DeviceGateOptions{Registry: reg, Origin: "http://127.0.0.1:41234", Page: fstest.MapFS{}, Machine: "desk"})
	srv := httptest.NewServer(gate)
	t.Cleanup(srv.Close)

	code, _ := reg.Offer("http://127.0.0.1:41234")
	resp, _ := named(t, srv, http.MethodPost, PairPath, "127.0.0.1:41234", "https://127.0.0.1:41234", `{"code":"`+code+`"}`, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("tightened origin with no proxy = %d, want 403", resp.StatusCode)
	}
	if _, _, err := reg.Redeem("http://127.0.0.1:41234", code, ""); err != nil {
		t.Fatalf("a refused page spent the code: %v", err)
	}
}

// The public name answers for the door the proxy dials, and for no other: a
// LAN door carries no public name, so a request wearing one is a stranger's.
func TestAPublicOriginAnswersForNoOtherDoor(t *testing.T) {
	srv := gateOn(t, lanDoor, "")

	resp, _ := named(t, srv, http.MethodPost, "/anything", "studio.example.com", proxyName, "{}", nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a public name at a door it does not stand in front of = %d, want 403", resp.StatusCode)
	}
}

// A page on another host over TLS naming the door is a stranger's spelling:
// tightening is a proxy's rewriting, not a page's costume.
func TestATightenedOriginNamingAnotherHostIsRefused(t *testing.T) {
	reg := NewDeviceRegistry()
	gate := NewDeviceGate(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), DeviceGateOptions{
		Registry: reg, Origin: "http://127.0.0.1:41234", PublicOrigin: proxyName,
		Page: fstest.MapFS{"index.html": {Data: []byte("x")}}, Machine: "desk",
	})
	srv := httptest.NewServer(gate)
	t.Cleanup(srv.Close)

	code, _ := reg.Offer("http://127.0.0.1:41234")
	resp, _ := named(t, srv, http.MethodPost, PairPath, "127.0.0.1:41234", "https://stranger.example.com:41234", `{"code":"`+code+`"}`, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a stranger's tightened origin = %d, want 403", resp.StatusCode)
	}
}

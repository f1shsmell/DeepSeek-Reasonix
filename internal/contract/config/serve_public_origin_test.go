package config

import (
	"strings"
	"testing"
)

func TestSharePublicOriginAcceptsAnHTTPSNaming(t *testing.T) {
	for _, raw := range []string{"https://studio.example.com", "https://studio.example.com/", "https://studio.example.com:8443"} {
		if err := ValidateSharePublicOrigin(raw); err != nil {
			t.Fatalf("ValidateSharePublicOrigin(%q) = %v, want accepted", raw, err)
		}
	}
}

func TestSharePublicOriginRefusesWhatCannotBeAPublicName(t *testing.T) {
	for _, raw := range []string{
		"",
		"http://studio.example.com",
		"studio.example.com",
		"https://",
		"https://studio.example.com/remote",
		"https://studio.example.com/?device=x",
		"https://user@studio.example.com",
		"https://127.0.0.1:41234",
		"https://localhost:41234",
		"not a url at all",
	} {
		if err := ValidateSharePublicOrigin(raw); err == nil {
			t.Fatalf("ValidateSharePublicOrigin(%q) = nil, want refused", raw)
		}
	}
}

func TestSharePublicOriginReadsWhatItValidates(t *testing.T) {
	cfg := &Config{Serve: ServeConfig{SharePublicOrigin: "https://studio.example.com"}}
	if got := cfg.SharePublicOrigin(); got != "https://studio.example.com" {
		t.Fatalf("SharePublicOrigin() = %q, want the setting", got)
	}
	cfg.Serve.SharePublicOrigin = "http://studio.example.com"
	if got := cfg.SharePublicOrigin(); got != "" {
		t.Fatalf("SharePublicOrigin() = %q, want nothing for a shape it does not answer for", got)
	}
}

func TestSetSharePublicOriginRoundTripsAndClears(t *testing.T) {
	cfg := &Config{}
	if err := cfg.SetSharePublicOrigin("https://studio.example.com"); err != nil {
		t.Fatal(err)
	}
	if cfg.Serve.SharePublicOrigin != "https://studio.example.com" {
		t.Fatalf("stored %q", cfg.Serve.SharePublicOrigin)
	}
	if err := cfg.SetSharePublicOrigin("http://no.tls"); err == nil {
		t.Fatal("a plain-http name was stored")
	}
	if cfg.Serve.SharePublicOrigin != "https://studio.example.com" {
		t.Fatalf("a refused save cleared the stored name: %q", cfg.Serve.SharePublicOrigin)
	}
	if err := cfg.SetSharePublicOrigin("  "); err != nil {
		t.Fatal(err)
	}
	if cfg.Serve.SharePublicOrigin != "" {
		t.Fatalf("clearing left %q", cfg.Serve.SharePublicOrigin)
	}
}

func TestRenderWritesThePublicOriginBesideThePort(t *testing.T) {
	cfg := &Config{Serve: ServeConfig{SharePort: 41234, SharePublicOrigin: "https://studio.example.com"}}
	var b strings.Builder
	renderServeSection(&b, cfg)
	out := b.String()
	if !strings.Contains(out, `share_port = 41234`) || !strings.Contains(out, `share_public_origin = "https://studio.example.com"`) {
		t.Fatalf("render =\n%s\nwant the port and the public origin", out)
	}
}

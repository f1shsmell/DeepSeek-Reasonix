package config

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// ErrSharePublicOriginInvalid marks a public origin the gate cannot answer for.
var ErrSharePublicOriginInvalid = errors.New("share public origin is not valid")

// ValidateSharePublicOrigin accepts an https origin and nothing else: the
// public name is where phones arrive from the open internet, and it must be
// where TLS is in force. A loopback name is a misconfiguration, not a name
// phones dial — the proxy's own door is the machine's to open, and it is
// opened on its own.
func ValidateSharePublicOrigin(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.User != nil {
		return fmt.Errorf("%w: use an https origin, like https://studio.example.com", ErrSharePublicOriginInvalid)
	}
	host := strings.ToLower(u.Hostname())
	if host == "127.0.0.1" || host == "localhost" || host == "::1" {
		return fmt.Errorf("%w: a loopback name is not a public one", ErrSharePublicOriginInvalid)
	}
	return nil
}

// SharePublicOrigin is the reverse proxy's name for this Studio — the address
// phones dial from anywhere — or "" when unset or hand-written outside the
// accepted shape.
func (c *Config) SharePublicOrigin() string {
	if c == nil || ValidateSharePublicOrigin(c.Serve.SharePublicOrigin) != nil {
		return ""
	}
	return c.Serve.SharePublicOrigin
}

// SetSharePublicOrigin validates and stores [serve] share_public_origin;
// empty clears it.
func (c *Config) SetSharePublicOrigin(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw != "" {
		if err := ValidateSharePublicOrigin(raw); err != nil {
			return err
		}
	}
	c.Serve.SharePublicOrigin = raw
	return nil
}

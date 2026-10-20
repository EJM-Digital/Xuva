package main

import (
	"testing"

	"github.com/EJM-Digital/Xuva/server/internal/config"
)

// TestPortFromHTTPAddr covers the three shapes config.HTTPAddr can take:
// pure ":N" (Go's default unspecified-bind notation), explicit IPv4
// host:port, and an empty/garbage string. The firewall hookup skips when
// this returns 0, so the test mainly proves we don't accidentally short
// the firewall code path for the common shapes.
func TestPortFromHTTPAddr(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{":8097", 8097},
		{"0.0.0.0:8097", 8097},
		{"192.168.1.5:8097", 8097},
		{"[::]:8097", 8097},
		{"", 0},
		{"not-a-host-port", 0},
		{":notnumeric", 0},
	}
	for _, tc := range cases {
		if got := portFromHTTPAddr(tc.in); got != tc.want {
			t.Errorf("portFromHTTPAddr(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestHostIsLoopback(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"127.0.0.1:8097", true},
		{"localhost:8097", true},
		{"[::1]:8097", true},
		{":8097", false},      // all interfaces
		{"0.0.0.0:8097", false},
		{"[::]:8097", false},
		{"192.168.1.5:8097", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := hostIsLoopback(tc.in); got != tc.want {
			t.Errorf("hostIsLoopback(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestGuardAuthDisabledBind(t *testing.T) {
	// Auth enabled: never blocked regardless of bind.
	if err := guardAuthDisabledBind(config.Config{AuthDisabled: false, HTTPAddr: "0.0.0.0:8097"}); err != nil {
		t.Errorf("auth-enabled non-loopback should be allowed, got %v", err)
	}
	// Auth disabled on loopback: allowed.
	if err := guardAuthDisabledBind(config.Config{AuthDisabled: true, HTTPAddr: "127.0.0.1:8097"}); err != nil {
		t.Errorf("auth-disabled loopback should be allowed, got %v", err)
	}
	// Auth disabled on a non-loopback bind: blocked.
	if err := guardAuthDisabledBind(config.Config{AuthDisabled: true, HTTPAddr: "0.0.0.0:8097"}); err == nil {
		t.Error("auth-disabled non-loopback bind should be refused")
	}
	// Override env lets it through.
	t.Setenv("XUVA_AUTH_DISABLED_ALLOW_NONLOOPBACK", "true")
	if err := guardAuthDisabledBind(config.Config{AuthDisabled: true, HTTPAddr: "0.0.0.0:8097"}); err != nil {
		t.Errorf("override should permit auth-disabled non-loopback, got %v", err)
	}
}

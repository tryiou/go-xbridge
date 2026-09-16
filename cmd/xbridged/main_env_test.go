package main

import (
	"testing"
)

func TestResolveCredentialFlag(t *testing.T) {
	t.Setenv("XBRIDGED_TEST_USER", "envuser")
	t.Setenv("XBRIDGED_TEST_EMPTY", "")

	tests := []struct {
		name    string
		flagVal string
		envKey  string
		want    string
	}{
		{"flag wins over env", "flaguser", "XBRIDGED_TEST_USER", "flaguser"},
		{"empty flag falls back to env", "", "XBRIDGED_TEST_USER", "envuser"},
		{"unset env stays empty", "", "XBRIDGED_TEST_UNSET_7f3a", ""},
		{"set-but-empty env stays empty", "", "XBRIDGED_TEST_EMPTY", ""},
		{"flag wins over empty env", "flaguser", "XBRIDGED_TEST_EMPTY", "flaguser"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveCredentialFlag(tt.flagVal, tt.envKey); got != tt.want {
				t.Errorf("resolveCredentialFlag(%q, %q) = %q, want %q",
					tt.flagVal, tt.envKey, got, tt.want)
			}
		})
	}
}

func TestRpcCredentialsHalfConfigured(t *testing.T) {
	tests := []struct {
		name string
		user string
		pass string
		want bool
	}{
		{"both set", "u", "p", false},
		{"neither set", "", "", false},
		{"user only", "u", "", true},
		{"password only", "", "p", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rpcCredentialsHalfConfigured(tt.user, tt.pass); got != tt.want {
				t.Errorf("rpcCredentialsHalfConfigured(%q, %q) = %v, want %v",
					tt.user, tt.pass, got, tt.want)
			}
		})
	}
}
func TestResolveCredentialFlagRealKeys(t *testing.T) {
	t.Setenv("XBRIDGED_RPCUSER", "u")
	t.Setenv("XBRIDGED_RPCPASSWORD", "p")
	if got := resolveCredentialFlag("", "XBRIDGED_RPCUSER"); got != "u" {
		t.Errorf("RPCUSER fallback = %q, want %q", got, "u")
	}
	if got := resolveCredentialFlag("", "XBRIDGED_RPCPASSWORD"); got != "p" {
		t.Errorf("RPCPASSWORD fallback = %q, want %q", got, "p")
	}
}

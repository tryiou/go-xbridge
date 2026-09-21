package log

import (
	"log/slog"
	"testing"
)

// TestLevelFacade pins the runtime level gate: SetLevel/Level round-trip,
// ParseLevel maps names (case-insensitive, with aliases and whitespace
// tolerance) and rejects unknown names, and L() never returns nil.
// Previously the whole facade sat at 0% (only handlers/file/txlog/dedup
// were covered). The incoming level is restored so later files keep INFO.
func TestLevelFacade(t *testing.T) {
	prev := Level()
	defer SetLevel(prev)

	SetLevel(slog.LevelDebug)
	if got := Level(); got != slog.LevelDebug {
		t.Fatalf("Level = %v, want Debug", got)
	}
	SetLevel(slog.LevelError)
	if got := Level(); got != slog.LevelError {
		t.Fatalf("Level = %v, want Error", got)
	}
	if L() == nil {
		t.Fatal("L() = nil")
	}
	// Emitters must not panic at any level, with or without fields.
	for _, emit := range []func(string, ...any){Debug, Info, Warn, Error} {
		emit("facade probe")
		emit("facade probe with fields", "k", "v", "n", 1)
	}
}

// TestParseLevel pins the name table, including aliases, case-insensitivity,
// and the empty→Info default.
func TestParseLevel(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want slog.Level
	}{
		{"debug", slog.LevelDebug}, {"DEBUG", slog.LevelDebug}, {"  Debug ", slog.LevelDebug},
		{"info", slog.LevelInfo}, {"", slog.LevelInfo}, {"INFO", slog.LevelInfo},
		{"warn", slog.LevelWarn}, {"warning", slog.LevelWarn}, {"WARN", slog.LevelWarn},
		{"error", slog.LevelError}, {"err", slog.LevelError}, {"Error", slog.LevelError},
	} {
		got, err := ParseLevel(tc.in)
		if err != nil {
			t.Errorf("ParseLevel(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseLevel(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
	for _, bad := range []string{"verbose", "trace", "debugg", "0"} {
		if _, err := ParseLevel(bad); err == nil {
			t.Errorf("ParseLevel(%q): want error, got nil", bad)
		}
	}
}

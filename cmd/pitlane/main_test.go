package main

import (
	"log/slog"
	"testing"
)

func TestCheckNoAuthListen(t *testing.T) {
	for _, tc := range []struct {
		listen  string
		any, ok bool
	}{
		{"127.0.0.1:8080", false, true},
		{"127.0.0.2:8080", false, true},
		{"[::1]:8080", false, true},
		{"localhost:8080", false, true},
		{"LocalHost:8080", false, true},
		{":8080", false, false},
		{"0.0.0.0:8080", false, false},
		{"[::]:8080", false, false},
		{"192.168.1.10:8080", false, false},
		{"pitlane.lan:8080", false, false},
		{"garbage", false, false},
		{":8080", true, true},
		{"192.168.1.10:8080", true, true},
	} {
		if err := checkNoAuthListen(tc.listen, tc.any); (err == nil) != tc.ok {
			t.Errorf("%q any=%v: err=%v, want ok=%v", tc.listen, tc.any, err, tc.ok)
		}
	}
}

func TestParseLogLevel(t *testing.T) {
	for in, want := range map[string]slog.Level{"debug": slog.LevelDebug, "INFO": slog.LevelInfo, " warn ": slog.LevelWarn, "error": slog.LevelError, "info+2": slog.LevelInfo + 2} {
		if got, err := parseLogLevel(in); err != nil || got != want {
			t.Errorf("%q: got %v %v, want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "verbose", "warning", "3"} {
		if _, err := parseLogLevel(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

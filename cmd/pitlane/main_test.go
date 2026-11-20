package main

import (
	"bytes"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/TuroYT/pitlane/internal/server"
)

func TestLogSetupToken(t *testing.T) {
	const secret = "0123456789abcdef01234567"
	for _, tc := range []struct {
		name         string
		tok          server.SetupToken
		wantToken    bool
		wantFile     bool
		wantNoOutput bool
	}{
		{"saved", server.SetupToken{Value: secret, File: "/data/setup-token", Saved: true}, false, true, false},
		{"not saved", server.SetupToken{Value: secret, File: "/data/setup-token"}, true, false, false},
		{"setup complete", server.SetupToken{}, false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logSetupToken(slog.New(slog.NewTextHandler(&buf, nil)), tc.tok)
			out := buf.String()
			if got := strings.Contains(out, secret); got != tc.wantToken {
				t.Errorf("token in the log = %v, want %v: %s", got, tc.wantToken, out)
			}
			if got := strings.Contains(out, "/data/setup-token"); got != tc.wantFile {
				t.Errorf("file in the log = %v, want %v: %s", got, tc.wantFile, out)
			}
			if tc.wantNoOutput && out != "" {
				t.Errorf("unexpected log: %s", out)
			}
		})
	}
}

func TestNewHTTPServerTimeouts(t *testing.T) {
	srv := newHTTPServer(":0", http.NotFoundHandler())
	if srv.ReadHeaderTimeout != readHeaderTimeout || srv.IdleTimeout != idleTimeout {
		t.Errorf("ReadHeaderTimeout=%v IdleTimeout=%v, want %v and %v", srv.ReadHeaderTimeout, srv.IdleTimeout, readHeaderTimeout, idleTimeout)
	}
	// Event streams and cache transfers outlive any fixed deadline.
	if srv.ReadTimeout != 0 || srv.WriteTimeout != 0 {
		t.Errorf("ReadTimeout=%v WriteTimeout=%v, want none", srv.ReadTimeout, srv.WriteTimeout)
	}
}

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

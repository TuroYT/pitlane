package main

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// parseLogLevel parses --log-level (debug, info, warn or error, any case;
// slog's "info+2" offsets are accepted too).
func parseLogLevel(s string) (slog.Level, error) {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(strings.TrimSpace(s))); err != nil {
		return 0, fmt.Errorf("invalid --log-level / PITLANE_LOG_LEVEL %q: want debug, info, warn or error", s)
	}
	return lvl, nil
}

// checkNoAuthListen refuses to serve without authentication on anything but
// a loopback address, unless explicitly allowed: whoever reaches the UI can
// register runners and read job logs.
func checkNoAuthListen(listen string, anyAddress bool) error {
	if anyAddress || isLoopback(listen) {
		return nil
	}
	return fmt.Errorf("--insecure-no-auth needs a loopback listen address (127.0.0.1, ::1 or localhost), not %q; "+
		"to serve it on another address behind your own authentication, also set --insecure-no-auth-any-address "+
		"(PITLANE_INSECURE_NO_AUTH_ANY_ADDRESS=true)", listen)
}

// isLoopback reports whether a listen address only accepts local
// connections (":8080" listens on every interface).
func isLoopback(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

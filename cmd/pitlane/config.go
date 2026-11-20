package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strings"

	"github.com/TuroYT/pitlane/internal/server"
)

// Defaults of the flags, used when neither the flag nor its environment
// variable is set.
const (
	defaultListen      = ":8080"
	defaultCacheListen = ":8091"
	defaultDataDir     = "/var/lib/pitlane"
	defaultLogLevel    = "info"
)

// config is the daemon's command line and environment. A flag wins over
// its environment variable, which wins over the default.
type config struct {
	Listen      string
	CacheListen string // "" or "off" disables the actions cache
	DataDir     string
	BaseURL     string
	// TrustedProxies is the raw list, parsed by check.
	TrustedProxies string
	LogLevel       slog.Level
	InsecureNoAuth bool
	// InsecureNoAuthAnyAddress allows InsecureNoAuth off loopback.
	InsecureNoAuthAnyAddress bool
	// PAT is PITLANE_GITHUB_TOKEN: environment only, a flag would show it
	// in the process list.
	PAT string
}

// parseConfig parses the daemon's flags (args excludes the program name)
// over the environment. Like the flag package, it prints usage errors on
// out itself; the error it returns only tells the caller to stop (see
// usageExitCode).
func parseConfig(name string, args []string, getenv func(string) string, out io.Writer) (config, error) {
	var c config
	var level string
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(out)
	defineFlags(fs, &c, &level, getenv)
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	lvl, err := parseLogLevel(level)
	if err != nil {
		fmt.Fprintf(out, "pitlane: %v\n", err)
		return config{}, err
	}
	c.LogLevel = lvl
	c.PAT = strings.TrimSpace(getenv("PITLANE_GITHUB_TOKEN"))
	return c, nil
}

// defineFlags declares the daemon's flags on fs, defaulting to the
// environment so --help shows the effective values.
func defineFlags(fs *flag.FlagSet, c *config, level *string, getenv func(string) string) {
	fs.StringVar(&c.Listen, "listen", envOr(getenv, "PITLANE_LISTEN", defaultListen), "HTTP listen address (PITLANE_LISTEN)")
	fs.StringVar(&c.CacheListen, "cache-listen", envOr(getenv, "PITLANE_CACHE_LISTEN", defaultCacheListen),
		"listen address of the actions cache that runners reach on the slot networks' gateway, \"off\" to disable (PITLANE_CACHE_LISTEN)")
	fs.StringVar(&c.DataDir, "data", envOr(getenv, "PITLANE_DATA", defaultDataDir), "data directory (PITLANE_DATA)")
	fs.StringVar(&c.BaseURL, "base-url", envOr(getenv, "PITLANE_BASE_URL", ""), "public URL of the UI, e.g. http://pitlane.lan:8080 (PITLANE_BASE_URL)")
	fs.StringVar(&c.TrustedProxies, "trusted-proxies", envOr(getenv, "PITLANE_TRUSTED_PROXIES", ""),
		"comma-separated IPs or CIDRs of reverse proxies whose Host and X-Forwarded-Proto headers are trusted when no base URL is set; loopback always is (PITLANE_TRUSTED_PROXIES)")
	fs.StringVar(level, "log-level", envOr(getenv, "PITLANE_LOG_LEVEL", defaultLogLevel), "debug, info, warn or error (PITLANE_LOG_LEVEL)")
	fs.BoolVar(&c.InsecureNoAuth, "insecure-no-auth", getenv("PITLANE_INSECURE_NO_AUTH") == "true",
		"disable login (development only, loopback listen address only) (PITLANE_INSECURE_NO_AUTH)")
	fs.BoolVar(&c.InsecureNoAuthAnyAddress, "insecure-no-auth-any-address", getenv("PITLANE_INSECURE_NO_AUTH_ANY_ADDRESS") == "true",
		"allow --insecure-no-auth on a non-loopback listen address, e.g. behind an authenticating proxy (PITLANE_INSECURE_NO_AUTH_ANY_ADDRESS)")
}

// usageExitCode is the exit code after a parseConfig error: 0 for --help,
// 2 for any usage error, as flag.ExitOnError does.
func usageExitCode(err error) int {
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	return 2
}

// check validates the settings whose errors are fatal (logged, exit 1)
// rather than usage errors, and warns when login is disabled. It returns
// the parsed trusted proxies.
func (c config) check(log *slog.Logger) ([]netip.Prefix, error) {
	if c.InsecureNoAuth {
		if err := checkNoAuthListen(c.Listen, c.InsecureNoAuthAnyAddress); err != nil {
			return nil, err
		}
		log.Warn("login is disabled (--insecure-no-auth): anyone reaching this address controls Pitlane", "addr", c.Listen)
	}
	return server.ParseTrustedProxies(c.TrustedProxies)
}

// serverConfig is the configuration of the web server.
func (c config) serverConfig(proxies []netip.Prefix) server.Config {
	return server.Config{Version: version, DataDir: c.DataDir, BaseURL: c.BaseURL, InsecureNoAuth: c.InsecureNoAuth,
		Listen: c.Listen, TrustedProxies: proxies, PAT: c.PAT}
}

// envOr returns the environment variable key, or def when it is unset or
// empty.
func envOr(getenv func(string) string, key, def string) string {
	if v := getenv(key); v != "" {
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

package main

import (
	"bytes"
	"errors"
	"flag"
	"log/slog"
	"net/netip"
	"reflect"
	"strings"
	"testing"
)

func TestParseConfig(t *testing.T) {
	t.Parallel()
	fullEnv := map[string]string{
		"PITLANE_LISTEN":                       "127.0.0.1:9000",
		"PITLANE_CACHE_LISTEN":                 "off",
		"PITLANE_DATA":                         "/srv/pitlane",
		"PITLANE_BASE_URL":                     "https://ci.example.org",
		"PITLANE_TRUSTED_PROXIES":              "10.0.0.0/8",
		"PITLANE_LOG_LEVEL":                    "debug",
		"PITLANE_INSECURE_NO_AUTH":             "true",
		"PITLANE_INSECURE_NO_AUTH_ANY_ADDRESS": "true",
		"PITLANE_GITHUB_TOKEN":                 "  ghp_secret\n",
	}
	for _, tc := range []struct {
		name string
		env  map[string]string
		args []string
		want config
	}{
		{"defaults", nil, nil, config{Listen: ":8080", CacheListen: ":8091", DataDir: "/var/lib/pitlane", LogLevel: slog.LevelInfo}},
		{"environment", fullEnv, nil, config{Listen: "127.0.0.1:9000", CacheListen: "off", DataDir: "/srv/pitlane",
			BaseURL: "https://ci.example.org", TrustedProxies: "10.0.0.0/8", LogLevel: slog.LevelDebug,
			InsecureNoAuth: true, InsecureNoAuthAnyAddress: true, PAT: "ghp_secret"}},
		{"flags win over the environment", fullEnv, []string{
			"--listen", "[::1]:7000", "--cache-listen", ":9191", "--data", "/data", "--base-url", "http://x",
			"--trusted-proxies", "192.168.0.1", "--log-level", "ERROR",
			"--insecure-no-auth=false", "--insecure-no-auth-any-address=false",
		}, config{Listen: "[::1]:7000", CacheListen: ":9191", DataDir: "/data", BaseURL: "http://x",
			TrustedProxies: "192.168.0.1", LogLevel: slog.LevelError, PAT: "ghp_secret"}},
		{"bare boolean flags", nil, []string{"-insecure-no-auth", "-insecure-no-auth-any-address"},
			config{Listen: ":8080", CacheListen: ":8091", DataDir: "/var/lib/pitlane", LogLevel: slog.LevelInfo,
				InsecureNoAuth: true, InsecureNoAuthAnyAddress: true}},
		{"only \"true\" enables a boolean variable", map[string]string{"PITLANE_INSECURE_NO_AUTH": "1", "PITLANE_INSECURE_NO_AUTH_ANY_ADDRESS": "yes"}, nil,
			config{Listen: ":8080", CacheListen: ":8091", DataDir: "/var/lib/pitlane", LogLevel: slog.LevelInfo}},
		{"empty variables fall back to the defaults", map[string]string{"PITLANE_LISTEN": "", "PITLANE_LOG_LEVEL": ""}, nil,
			config{Listen: ":8080", CacheListen: ":8091", DataDir: "/var/lib/pitlane", LogLevel: slog.LevelInfo}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			got, err := parseConfig("pitlane", tc.args, func(k string) string { return tc.env[k] }, &out)
			if err != nil {
				t.Fatalf("parseConfig: %v (%s)", err, out.String())
			}
			if got != tc.want {
				t.Errorf("got  %+v\nwant %+v", got, tc.want)
			}
			if out.Len() != 0 {
				t.Errorf("unexpected output: %s", out.String())
			}
		})
	}
}

func TestParseConfigErrors(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		env      map[string]string
		args     []string
		wantCode int
		wantOut  string
	}{
		{"help", nil, []string{"-h"}, 0, "Usage of pitlane:"},
		{"unknown flag", nil, []string{"--bogus"}, 2, "flag provided but not defined: -bogus"},
		{"missing value", nil, []string{"--listen"}, 2, "flag needs an argument: -listen"},
		{"bad boolean", nil, []string{"--insecure-no-auth=maybe"}, 2, "invalid boolean value"},
		{"bad level flag", nil, []string{"--log-level", "verbose"}, 2, `pitlane: invalid --log-level / PITLANE_LOG_LEVEL "verbose"`},
		{"bad level variable", map[string]string{"PITLANE_LOG_LEVEL": "loud"}, nil, 2, `"loud"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			_, err := parseConfig("pitlane", tc.args, func(k string) string { return tc.env[k] }, &out)
			if err == nil {
				t.Fatal("no error")
			}
			if got := usageExitCode(err); got != tc.wantCode {
				t.Errorf("exit code %d, want %d (%v)", got, tc.wantCode, err)
			}
			if !strings.Contains(out.String(), tc.wantOut) {
				t.Errorf("output %q does not contain %q", out.String(), tc.wantOut)
			}
		})
	}
}

// --help shows each flag with its environment variable and effective
// default, which comes from the environment when it is set.
func TestParseConfigHelpShowsEnvironmentDefaults(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	_, err := parseConfig("pitlane", []string{"--help"}, func(k string) string {
		return map[string]string{"PITLANE_DATA": "/srv/pitlane"}[k]
	}, &out)
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("err = %v, want flag.ErrHelp", err)
	}
	for _, want := range []string{
		`-data string`, `(default "/srv/pitlane")`, `(default ":8080")`, `(default ":8091")`, `(default "info")`,
		"PITLANE_LISTEN", "PITLANE_CACHE_LISTEN", "PITLANE_BASE_URL", "PITLANE_TRUSTED_PROXIES",
		"PITLANE_LOG_LEVEL", "PITLANE_INSECURE_NO_AUTH)", "PITLANE_INSECURE_NO_AUTH_ANY_ADDRESS",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("--help lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "GITHUB_TOKEN") {
		t.Error("the token must not be a flag")
	}
}

func TestConfigCheck(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		cfg         config
		wantErr     string
		wantWarn    bool
		wantProxies []netip.Prefix
	}{
		{name: "auth on any address", cfg: config{Listen: ":8080"}},
		{name: "no auth on loopback", cfg: config{Listen: "127.0.0.1:8080", InsecureNoAuth: true}, wantWarn: true},
		{name: "no auth on every interface", cfg: config{Listen: ":8080", InsecureNoAuth: true}, wantErr: "needs a loopback listen address"},
		{name: "no auth on any address, allowed", cfg: config{Listen: ":8080", InsecureNoAuth: true, InsecureNoAuthAnyAddress: true}, wantWarn: true},
		{name: "trusted proxies", cfg: config{Listen: ":8080", TrustedProxies: "10.0.0.0/8, 192.168.1.2"},
			wantProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("192.168.1.2/32")}},
		{name: "bad trusted proxy", cfg: config{Listen: ":8080", TrustedProxies: "not-an-ip"}, wantErr: "not-an-ip"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			log, buf := testLogger()
			proxies, err := tc.cfg.check(log)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(proxies, tc.wantProxies) {
				t.Errorf("proxies = %v, want %v", proxies, tc.wantProxies)
			}
			warned := strings.Contains(buf.String(), "login is disabled (--insecure-no-auth)")
			if warned != tc.wantWarn {
				t.Errorf("warning logged = %v, want %v: %s", warned, tc.wantWarn, buf.String())
			}
		})
	}
}

func TestServerConfig(t *testing.T) {
	t.Parallel()
	proxies := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	c := config{Listen: ":1", CacheListen: ":2", DataDir: "/d", BaseURL: "http://b", InsecureNoAuth: true, PAT: "tok"}
	got := c.serverConfig(proxies)
	if got.Version != version || got.DataDir != "/d" || got.BaseURL != "http://b" || !got.InsecureNoAuth ||
		got.Listen != ":1" || got.PAT != "tok" || !reflect.DeepEqual(got.TrustedProxies, proxies) {
		t.Errorf("serverConfig = %+v", got)
	}
}

func TestCheckNoAuthListen(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

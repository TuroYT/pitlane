// Command pitlane runs fast, ephemeral, autoscaled self-hosted GitHub Actions
// runners on a single Docker host, with a web UI.
package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	// Installed as the runner's externals/node*/bin/node, the binary runs as
	// the Node shim from this package's init, before anything else.
	_ "github.com/TuroYT/pitlane/internal/nodeshim"
	"github.com/TuroYT/pitlane/internal/pool"
)

var version = "dev"

func main() {
	os.Exit(realSystem().main(context.Background(), os.Args))
}

// system is everything the command takes from the process and the host:
// environment, standard streams, signals, listeners, Docker and exec. main
// uses the real one; tests replace what they cannot run (Docker, exec,
// signals) and keep the rest.
type system struct {
	getenv  func(string) string
	environ func() []string
	stdout  io.Writer
	stderr  io.Writer
	// signals derives the context that ends on SIGINT or SIGTERM.
	signals func(context.Context) (context.Context, context.CancelFunc)
	listen  func(network, address string) (net.Listener, error)
	// ping checks that the Docker daemon answers.
	ping       func(context.Context, *pool.Pool) error
	executable func() (string, error)
	exec       func(argv0 string, argv, envv []string) error
	// setDefaultLogger installs the logger of packages that have none of
	// their own (store).
	setDefaultLogger func(*slog.Logger)
}

// realSystem is the system of the running process.
func realSystem() system {
	return system{
		getenv:  os.Getenv,
		environ: os.Environ,
		stdout:  os.Stdout,
		stderr:  os.Stderr,
		signals: func(ctx context.Context) (context.Context, context.CancelFunc) {
			return signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
		},
		listen:           net.Listen,
		ping:             func(ctx context.Context, p *pool.Pool) error { return p.Ping(ctx) },
		executable:       os.Executable,
		exec:             syscall.Exec,
		setDefaultLogger: slog.SetDefault,
	}
}

// main runs the command line args (args[0] is the program) and returns the
// exit code.
func (s system) main(ctx context.Context, args []string) int {
	if len(args) > 1 {
		switch args[1] {
		case "version", "--version", "-v":
			return s.printVersion()
		case "cache-forward":
			return s.cacheForward(ctx, args[2:])
		case "setup-token":
			return s.setupToken(args[2:])
		}
	}
	return s.daemon(ctx, args)
}

// daemon runs Pitlane itself until a signal or a restart request, then
// re-executes the updated binary when an update asked for it.
func (s system) daemon(ctx context.Context, args []string) int {
	cfg, err := parseConfig(programName(args), args[min(1, len(args)):], s.getenv, s.stderr)
	if err != nil {
		return usageExitCode(err)
	}
	log := slog.New(slog.NewTextHandler(s.stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	proxies, err := cfg.check(log)
	if err != nil {
		log.Error("fatal", "error", err)
		return 1
	}
	s.setDefaultLogger(log)
	err = run(ctx, log, cfg.serverConfig(proxies), cfg.CacheListen, s)
	if errors.Is(err, errRestart) {
		return s.reexec(log, args)
	}
	if err != nil {
		log.Error("fatal", "error", err)
		return 1
	}
	return 0
}

// programName is the name the usage message shows, as the flag package
// shows os.Args[0].
func programName(args []string) string {
	if len(args) == 0 {
		return "pitlane"
	}
	return args[0]
}

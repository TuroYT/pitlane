package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"
	"testing"

	"github.com/TuroYT/pitlane/internal/pool"
)

// syncBuffer is a bytes.Buffer safe for the server goroutines that log.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// testLogger logs everything, debug included, into a buffer.
func testLogger() (*slog.Logger, *syncBuffer) {
	var buf syncBuffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

// loopbackListen listens on a free loopback port, whatever address it is
// asked for: the actions cache needs a fixed port in its address, which a
// test cannot reserve.
func loopbackListen(t *testing.T) func(network, address string) (net.Listener, error) {
	t.Helper()
	return func(network, _ string) (net.Listener, error) {
		ln, err := net.Listen(network, "127.0.0.1:0")
		if err == nil {
			t.Cleanup(func() { _ = ln.Close() })
		}
		return ln, err
	}
}

// testSystem is a system without signals, Docker or exec: getenv reads env,
// the signal context is the caller's, ping succeeds, exec fails.
func testSystem(t *testing.T, env map[string]string) (system, *syncBuffer, *syncBuffer) {
	t.Helper()
	var stdout, stderr syncBuffer
	return system{
		getenv:  func(k string) string { return env[k] },
		environ: func() []string { return []string{"A=1"} },
		stdout:  &stdout,
		stderr:  &stderr,
		signals: func(ctx context.Context) (context.Context, context.CancelFunc) {
			return context.WithCancel(ctx)
		},
		listen:           loopbackListen(t),
		ping:             func(context.Context, *pool.Pool) error { return nil },
		executable:       func() (string, error) { return "", errors.New("no executable in tests") },
		exec:             func(string, []string, []string) error { return errors.New("no exec in tests") },
		setDefaultLogger: func(*slog.Logger) {},
	}, &stdout, &stderr
}

// refused reports whether nothing listens on addr any more.
func refused(addr net.Addr) bool {
	c, err := net.Dial(addr.Network(), addr.String())
	if err != nil {
		return true
	}
	_ = c.Close()
	return false
}

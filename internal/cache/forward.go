package cache

import (
	"context"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"
)

// Forward relays TCP connections from listen to target until ctx ends. When
// Pitlane runs in a container, the pool runs it in a small unprivileged
// container joined to the slot networks (`pitlane cache-forward`), so jobs
// reach the cache port and nothing else of Pitlane. target is resolved on
// each connection: it follows Pitlane's container across restarts.
func Forward(ctx context.Context, listen, target string, log *slog.Logger) error {
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	log.Info("forwarding the actions cache", "listen", listen, "target", target)
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			log.Warn("accept", "error", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go relay(c, target, log)
	}
}

func relay(c net.Conn, target string, log *slog.Logger) {
	defer func() { _ = c.Close() }()
	up, err := net.DialTimeout("tcp", target, 10*time.Second)
	if err != nil {
		log.Warn("actions cache unreachable", "target", target, "error", err)
		return
	}
	defer func() { _ = up.Close() }()
	var wg sync.WaitGroup
	pipe := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		if tc, ok := dst.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}
	wg.Add(2)
	go pipe(up, c)
	go pipe(c, up)
	wg.Wait()
}

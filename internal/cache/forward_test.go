package cache

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestForward(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_, _ = w.Write([]byte("echo:" + string(b)))
	}))
	defer up.Close()
	// A free port for the forwarder.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Forward(ctx, addr, strings.TrimPrefix(up.URL, "http://"), slog.New(slog.DiscardHandler))
	}()
	var res *http.Response
	for range 50 {
		if res, err = http.Post("http://"+addr+"/x", "text/plain", strings.NewReader("hello")); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if string(b) != "echo:hello" {
		t.Fatalf("got %q", b)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

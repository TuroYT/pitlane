package pool

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
)

func TestCapWriterDrainsBeyondCap(t *testing.T) {
	w := &capWriter{max: 5}
	for _, s := range []string{"abc", "defgh", "ijk"} {
		if n, err := w.Write([]byte(s)); n != len(s) || err != nil {
			t.Fatalf("write must accept everything: %d %v", n, err)
		}
	}
	if w.String() != "abcde" {
		t.Fatalf("kept %q", w.String())
	}
}

func TestWaitExecPollsUntilFinished(t *testing.T) {
	calls := 0
	code, err := waitExec(context.Background(), func(context.Context) (container.ExecInspect, error) {
		calls++
		if calls < 3 {
			return container.ExecInspect{Running: true, ExitCode: 0}, nil
		}
		return container.ExecInspect{Running: false, ExitCode: 7}, nil
	})
	if err != nil || code != 7 || calls != 3 {
		t.Fatalf("code=%d err=%v calls=%d", code, err, calls)
	}
}

func TestWaitExecStillRunningIsAnError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	code, err := waitExec(ctx, func(context.Context) (container.ExecInspect, error) {
		return container.ExecInspect{Running: true}, nil
	})
	if err == nil || code != -1 || !strings.Contains(err.Error(), "still running") {
		t.Fatalf("code=%d err=%v", code, err)
	}
	_, err = waitExec(context.Background(), func(context.Context) (container.ExecInspect, error) {
		return container.ExecInspect{}, errors.New("boom")
	})
	if err == nil {
		t.Fatal("inspect error must surface")
	}
}

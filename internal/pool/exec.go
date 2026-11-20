package pool

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
)

// execOutputMax bounds what exec keeps of a script's output: some of these
// containers are controlled by jobs.
const execOutputMax = 4 << 20

// exec runs a shell script inside a container and returns its output.
func (p *Pool) exec(ctx context.Context, name, script string) (string, int, error) {
	ex, err := p.cli.ContainerExecCreate(ctx, name, container.ExecOptions{Cmd: []string{"sh", "-c", script}, AttachStdout: true, AttachStderr: true})
	if err != nil {
		return "", -1, err
	}
	att, err := p.cli.ContainerExecAttach(ctx, ex.ID, container.ExecAttachOptions{})
	if err != nil {
		return "", -1, err
	}
	defer att.Close()
	// Read to the end (output beyond the cap is discarded): stopping early
	// would leave the process running and its exit code unknown.
	out := &capWriter{max: execOutputMax}
	if _, err := stdcopy.StdCopy(out, out, att.Reader); err != nil {
		return out.String(), -1, fmt.Errorf("read exec output: %w", err)
	}
	code, err := waitExec(ctx, func(ctx context.Context) (container.ExecInspect, error) {
		return p.cli.ContainerExecInspect(ctx, ex.ID)
	})
	return out.String(), code, err
}

// execExitWait bounds how long exec waits for a process whose output has
// ended to be reported as exited; it is polled with a delay doubling from
// execPollMin up to execPollMax.
const (
	execExitWait = 30 * time.Second
	execPollMin  = 10 * time.Millisecond
	execPollMax  = 500 * time.Millisecond
)

// waitExec polls an exec until Docker reports it finished and returns its
// exit code. Its output stream ending is not enough: the exit code is only
// meaningful once Running is false.
func waitExec(ctx context.Context, inspect func(context.Context) (container.ExecInspect, error)) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, execExitWait)
	defer cancel()
	delay := execPollMin
	for {
		ins, err := inspect(ctx)
		if err != nil {
			return -1, err
		}
		if !ins.Running {
			return ins.ExitCode, nil
		}
		select {
		case <-ctx.Done():
			return -1, fmt.Errorf("exec still running: %w", ctx.Err())
		case <-time.After(delay):
		}
		delay = min(delay*2, execPollMax)
	}
}

// capWriter keeps the first max bytes written to it and discards the rest.
type capWriter struct {
	buf bytes.Buffer
	max int
}

func (w *capWriter) Write(b []byte) (int, error) {
	if room := w.max - w.buf.Len(); room > 0 {
		w.buf.Write(b[:min(room, len(b))])
	}
	return len(b), nil
}

func (w *capWriter) String() string { return w.buf.String() }

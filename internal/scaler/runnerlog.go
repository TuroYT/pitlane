package scaler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// LogFile returns the path of a finished runner's log.
func (m *Manager) LogFile(name string) string {
	return filepath.Join(m.logDir, filepath.Base(name)+".log")
}

// maxArchivedLog bounds the archived log of one runner (its output is
// controlled by the job).
const maxArchivedLog = 64 << 20

var errLogFull = errors.New("log size limit reached")

// capWriter passes through at most n bytes, then fails with errLogFull.
type capWriter struct {
	w io.Writer
	n int64
}

func (c *capWriter) Write(b []byte) (int, error) {
	if c.n <= 0 {
		return 0, errLogFull
	}
	full := int64(len(b)) > c.n
	if full {
		b = b[:c.n]
	}
	n, err := c.w.Write(b)
	c.n -= int64(n)
	if err == nil && full {
		err = errLogFull
	}
	return n, err
}

// archiveLog copies a log through copyTo into w, keeping at most max bytes
// followed by a truncation notice.
func archiveLog(w io.Writer, max int64, copyTo func(io.Writer) error) error {
	err := copyTo(&capWriter{w: w, n: max})
	if errors.Is(err, errLogFull) {
		_, err = fmt.Fprintf(w, "\n[pitlane] log truncated at %d MB\n", max>>20)
	}
	return err
}

// archiveRunnerLog saves the output of a runner's container to its log
// file. A failure only loses the log.
func (m *Manager) archiveRunnerLog(ctx context.Context, r *runner) {
	f, err := os.OpenFile(m.LogFile(r.name), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		m.log.Warn("archive runner log", "runner", r.name, "error", err)
		return
	}
	limit := m.logLimit
	if limit <= 0 {
		limit = maxArchivedLog
	}
	if err := archiveLog(f, limit, func(w io.Writer) error { return m.pool.Logs(ctx, r.containerID, false, w) }); err != nil {
		m.log.Warn("archive runner log", "runner", r.name, "error", err)
	}
	if err := f.Close(); err != nil {
		m.log.Warn("archive runner log", "runner", r.name, "error", err)
	}
}

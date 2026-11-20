package server

import (
	"context"
	"io"

	"github.com/TuroYT/pitlane/internal/pool"
)

// runnerPool is what the server uses of the slot pool (*pool.Pool). Most of
// it talks to Docker: handlers depend on this interface so tests can serve
// them without a Docker daemon.
type runnerPool interface {
	Config() pool.Config
	// Configure applies a new configuration (slots are rebuilt as needed).
	Configure(ctx context.Context, cfg pool.Config) error
	StrictSlots() int
	Memory() pool.MemoryState
	Slots() []pool.SlotState
	SlotWipes() map[string]int64
	Ping(ctx context.Context) error
	DiskSpace() (total, free int64, err error)
	// Logs copies the logs of a runner container to w.
	Logs(ctx context.Context, id string, follow bool, w io.Writer) error
	// TailJobOutput streams the step output of the job a runner container runs.
	TailJobOutput(ctx context.Context, id string, w io.Writer) error
}

var _ runnerPool = (*pool.Pool)(nil)

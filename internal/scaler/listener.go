package scaler

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/TuroYT/pitlane/internal/store"
)

// Listener restarts: a failed listener is retried after a delay that doubles
// up to maxListenBackoff, and starts over once a listener stayed up for
// listenHealthy.
const (
	minListenBackoff = 5 * time.Second
	maxListenBackoff = 2 * time.Minute
	listenHealthy    = 5 * time.Minute
)

// run keeps the target's listener running until the target or parent is
// canceled.
func (t *target) run(parent context.Context) {
	defer context.AfterFunc(parent, t.cancel)()
	backoff := minListenBackoff
	for t.ctx.Err() == nil {
		start := time.Now()
		err := t.listen()
		if t.ctx.Err() != nil {
			break
		}
		if time.Since(start) > listenHealthy {
			backoff = minListenBackoff
		}
		msg := "listener stopped"
		if err != nil {
			msg = err.Error()
		}
		t.log.Error("listener failed, retrying", "error", msg, "in", backoff)
		t.setStatus("error", msg)
		select {
		case <-t.ctx.Done():
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxListenBackoff)
	}
}

// listen connects to the target's scale set and delivers its job messages
// until the target is canceled (nil) or the session fails.
func (t *target) listen() error {
	cfg := t.cfg()
	client, err := t.m.connect(t.m.creds(), cfg, t.m.Version)
	if err != nil {
		return err
	}
	id, err := t.resolveScaleSet(client, cfg)
	if err != nil {
		return err
	}
	host, _ := os.Hostname()
	session, err := client.openSession(t.ctx, id, fmt.Sprintf("pitlane-%s-%d", host, cfg.ID))
	if err != nil {
		return err
	}
	defer func() {
		if err := session.close(context.Background()); err != nil {
			t.log.Warn("close message session", "error", err)
		}
	}()
	t.mu.Lock()
	t.client, t.session, t.scaleSetID = client, session, id
	t.t.ScaleSetID = id
	t.mu.Unlock()
	t.setStatus("listening", "")
	t.log.Info("listening for jobs", "scaleSet", id, "label", cfg.ScaleSetName)
	go t.cleanupOrphans()
	err = session.run(t.ctx, id, cfg.MaxRunners, t.log.WithGroup("listener"), &scaler{t: t, session: session})
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// resolveScaleSet returns the ID of the target's scale set, creating the
// scale set when missing, and records the ID.
func (t *target) resolveScaleSet(client scaleSetAPI, cfg store.Target) (int, error) {
	id, created, err := client.ensureScaleSet(t.ctx, cfg.ScaleSetName)
	if err != nil {
		return 0, err
	}
	if created {
		t.log.Info("scale set created", "id", id, "label", cfg.ScaleSetName)
	}
	if cfg.ScaleSetID != id {
		// Only that column: cfg may be stale (e.g. switched to strict since).
		if err := t.m.store.SetScaleSetID(cfg.ID, id); err != nil {
			t.log.Warn("save scale set id", "error", err)
		}
	}
	return id, nil
}

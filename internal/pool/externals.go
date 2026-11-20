package pool

import (
	"archive/tar"
	"context"
	"io"
	"os"
	"strings"

	"github.com/TuroYT/pitlane/internal/nodeshim"
	"github.com/docker/docker/api/types/container"
)

const externalsDir = "/home/runner/externals"

var externalsCopies = make(chan struct{}, 2)

// ensureExternals copies the runner image's externals into the slot volume.
// The copy is redone when the runner image or the slot owner changes (tracked
// in Pitlane's state file, never in the volume, which slot jobs can reach);
// between jobs of the same owner it is shared like the other caches.
func (p *Pool) ensureExternals(ctx context.Context, i int, img string) error {
	id, err := p.ops().imageID(ctx, img)
	if err != nil {
		return err
	}
	id = externalsKey(id)
	p.mu.Lock()
	done := p.externals[i] == id
	p.mu.Unlock()
	if done {
		return nil
	}
	// Copies are disk-heavy (~600 MB): never run many at once.
	select {
	case externalsCopies <- struct{}{}:
		defer func() { <-externalsCopies }()
	case <-ctx.Done():
		return ctx.Err()
	}
	p.log.Info("copying runner externals into slot", "slot", i)
	if err := p.ops().copyExternals(ctx, i, img); err != nil {
		return err
	}
	p.mu.Lock()
	p.externals[i] = id
	p.saveLocked()
	p.mu.Unlock()
	return nil
}

// externalsKey identifies what a slot's externals volume holds: the runner
// image's externals with this version of the Node shim.
func externalsKey(imageID string) string { return imageID + "+shim" + nodeshim.Version }

// shimInContainer is where copyExternals puts Pitlane's own binary, the Node
// shim (internal/nodeshim), in the container that copies the externals.
const shimInContainer = "/pitlane-node-shim"

// shimScript installs the shim binary in front of every Node runtime of the
// externals copied to dir, the real binary kept next to it as node.pitlane.
// It is best effort and never leaves a runtime without a working node: when
// the shim is missing or does not work in this image (checked by running it
// in front of itself, which prints Pitlane's version), nothing is installed;
// a runtime whose shim cannot be written keeps its original binary. Jobs then
// use GitHub's cache. The runtimes share one copy of the binary (hard links).
func shimScript(dir, shim string) string {
	return `(s='` + shim + `'
t=$(mktemp -d) && cp "$s" "$t/node" && ln -s "$s" "$t/node.pitlane" && v=$("$t/node" version 2>/dev/null)
[ -n "$t" ] && rm -rf "$t"
case "$v" in pitlane\ *) ;; *) false ;; esac || { echo "pitlane: no working node shim, the runner externals keep their node" >&2; exit 0; }
f=
for n in ` + dir + `/node*/bin/node; do
  [ -f "$n" ] && [ ! -L "$n" ] && [ ! -e "$n.pitlane" ] || continue
  { { [ -n "$f" ] && ln "$f" "$n.tmp" 2>/dev/null; } || cp "$s" "$n.tmp"; } && chmod 755 "$n.tmp" && mv "$n" "$n.pitlane" && mv "$n.tmp" "$n" && f=$n ||
    { [ -e "$n" ] || mv "$n.pitlane" "$n"; rm -f "$n.tmp"; }
done); true`
}

// openShim opens the binary to install as the Node shim: the running
// Pitlane, through /proc/self/exe so that it is still the right file after a
// self-update replaced it on disk.
func (p *Pool) openShim() (*os.File, error) {
	if p.shimExe != "" {
		return os.Open(p.shimExe)
	}
	f, err := os.Open("/proc/self/exe")
	if err == nil {
		return f, nil
	}
	exe, eerr := os.Executable()
	if eerr != nil {
		return nil, err
	}
	return os.Open(exe)
}

// stageShim copies Pitlane's running binary to shimInContainer in a created
// (not yet started) container.
func (p *Pool) stageShim(ctx context.Context, id string) error {
	f, err := p.openShim()
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		err := tw.WriteHeader(&tar.Header{Name: strings.TrimPrefix(shimInContainer, "/"), Mode: 0o755, Size: st.Size(), Typeflag: tar.TypeReg})
		if err == nil {
			_, err = io.Copy(tw, f)
		}
		if err == nil {
			err = tw.Close()
		}
		pw.CloseWithError(err)
	}()
	defer func() { _ = pr.Close() }() // stops the writer if the copy fails
	return p.cli.CopyToContainer(ctx, id, "/", pr, container.CopyToContainerOptions{})
}

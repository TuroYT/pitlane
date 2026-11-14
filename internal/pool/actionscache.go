package pool

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"

	"github.com/docker/docker/api/types/network"

	"github.com/TuroYT/pitlane/internal/cache"
)

// CacheBroker is the actions cache server (internal/cache) as the pool sees
// it: it hands out per-runner credentials and gives disk space back.
type CacheBroker interface {
	// Register lets a runner use the cache of an owner (one repository when
	// repo is set) and returns its token.
	Register(runner string, id cache.Identity) string
	// Revoke invalidates a runner's token.
	Revoke(runner string)
	// SetLimit bounds the stored bytes (LRU eviction).
	SetLimit(bytes int64)
	// Evict frees at least n bytes, least recently used entries first, and
	// returns the bytes freed.
	Evict(n int64) int64
}

// SetCache attaches the actions cache server, listening on port on the
// host. Call it before adopting runners (RunnerContainers) so that they keep
// their token; runners started afterwards get a cache URL when the feature
// is on.
func (p *Pool) SetCache(b CacheBroker, port int) {
	p.mu.Lock()
	p.cache, p.cachePort = b, port
	limit := int64(p.cfg.ActionsCacheGB) << 30
	p.mu.Unlock()
	if b != nil {
		b.SetLimit(limit)
	}
}

func (p *Pool) cacheBroker() (CacheBroker, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cache, p.cachePort
}

// externalsKey identifies what a slot's externals volume holds: the runner
// image's externals with this version of the Node shim.
func externalsKey(imageID string) string { return imageID + "+shim" + cache.ShimVersion }

// shimScript installs cache.NodeShim in front of every Node runtime of the
// externals copied to dir. It is best effort: a runtime whose shim cannot
// be written keeps its original binary (jobs then use GitHub's cache).
func shimScript(dir string) string {
	return `for n in ` + dir + `/node*/bin/node; do
  [ -f "$n" ] && [ ! -L "$n" ] && [ ! -e "$n.pitlane" ] || continue
  { cat > "$n.tmp" <<'PITLANE_SHIM'
` + cache.NodeShim + `PITLANE_SHIM
  } && chmod 755 "$n.tmp" && mv "$n" "$n.pitlane" && mv "$n.tmp" "$n" || { [ -e "$n" ] || mv "$n.pitlane" "$n"; rm -f "$n.tmp"; }
done; true`
}

// cacheEnv registers a runner with the actions cache and returns the
// environment variable that makes its JavaScript actions use it, or "" when
// the job must use GitHub's cache: strict jobs, feature off, no server, or
// no route from the slot to the host.
func (p *Pool) cacheEnv(ctx context.Context, cfg Config, spec RunnerSpec) string {
	b, port := p.cacheBroker()
	if b == nil || port == 0 || !cfg.ActionsCache || spec.Lease.Strict {
		return ""
	}
	host, err := p.slotGateway(ctx, spec.Lease.Slot)
	if err != nil {
		p.log.Warn("actions cache unavailable for this job (it uses GitHub's cache)", "slot", spec.Lease.Slot, "error", err)
		return ""
	}
	tok := b.Register(spec.Name, cache.Identity{Owner: spec.Owner, Repo: spec.Repo})
	return "PITLANE_CACHE_URL=http://" + net.JoinHostPort(host, strconv.Itoa(port)) + "/" + tok + "/"
}

// revokeCache invalidates a runner's actions cache token.
func (p *Pool) revokeCache(runner string) {
	if runner == "" {
		return
	}
	if b, _ := p.cacheBroker(); b != nil {
		b.Revoke(runner)
	}
}

// adoptCache gives an adopted (non-strict) runner its actions cache token
// back: the token is derived from the runner name, so the job keeps using
// the URL it was started with.
func (p *Pool) adoptCache(o Orphan, repo string) {
	if o.Strict || o.Untrusted || o.Name == "" || o.Owner == "" {
		return
	}
	if b, _ := p.cacheBroker(); b != nil {
		b.Register(o.Name, cache.Identity{Owner: o.Owner, Repo: repo})
	}
}

// slotGateway returns the address of the host on a slot's network (its
// bridge gateway), where runners reach Pitlane's cache listener.
func (p *Pool) slotGateway(ctx context.Context, slot int) (string, error) {
	n, err := p.cli.NetworkInspect(ctx, slotNetwork(slot), network.InspectOptions{})
	if err != nil {
		return "", err
	}
	var v6 string
	for _, c := range n.IPAM.Config {
		ip := net.ParseIP(c.Gateway)
		switch {
		case ip == nil:
		case ip.To4() != nil:
			return ip.String(), nil
		case v6 == "":
			v6 = ip.String()
		}
	}
	if v6 != "" {
		return v6, nil
	}
	return "", errors.New("slot network " + slotNetwork(slot) + " has no gateway")
}

// CachePort extracts the port of a listen address such as ":8091".
func CachePort(listen string) int {
	_, port, err := net.SplitHostPort(strings.TrimSpace(listen))
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(port)
	if err != nil || n <= 0 || n > 65535 {
		return 0
	}
	return n
}

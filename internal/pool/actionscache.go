package pool

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/docker/docker/api/types/container"
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
	// SetLimits bounds the stored bytes (LRU eviction) and what one owner
	// may store, in percent of that.
	SetLimits(bytes int64, ownerPercent int)
	// Evict frees at least n bytes, least recently used entries first, and
	// returns the bytes freed.
	Evict(n int64) int64
	// Dir is where the entries are stored.
	Dir() string
}

// SetCache attaches the actions cache server, listening on port on the
// host. Call it before adopting runners (RunnerContainers) so that they keep
// their token; runners started afterwards get a cache URL when the feature
// is on.
func (p *Pool) SetCache(b CacheBroker, port int) {
	p.mu.Lock()
	p.cache, p.cachePort = b, port
	p.mu.Unlock()
	p.applyCacheLimits()
}

// applyCacheLimits gives the cache server the limits of the current
// configuration. Calls are serialized and each reads the configuration
// under the lock, so a slower, older Configure cannot apply stale limits
// after a newer one.
func (p *Pool) applyCacheLimits() {
	p.cacheLimitsMu.Lock()
	defer p.cacheLimitsMu.Unlock()
	p.mu.Lock()
	b, limit, share := p.cache, int64(p.cfg.ActionsCacheGB)<<30, p.cfg.ActionsCacheOwnerPct
	p.mu.Unlock()
	if b != nil {
		b.SetLimits(limit, share)
	}
}

func (p *Pool) cacheBroker() (CacheBroker, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cache, p.cachePort
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
	host, err := p.cacheHost(ctx, spec.Lease.Slot)
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

// cacheForwarder relays the cache port when Pitlane runs in a container.
const cacheForwarder = "pitlane-cache-forward"

// cacheHost returns the address where a slot's runners reach Pitlane's
// cache listener. On the host, it is the slot network's gateway. When Pitlane
// runs in a container (Compose), a published port is not reachable from
// another bridge network, and joining the slot networks itself would expose
// its UI and API to jobs: an unprivileged forwarder container that only
// relays the cache port joins them instead, like the mirror.
func (p *Pool) cacheHost(ctx context.Context, slot int) (string, error) {
	self := p.selfContainer(ctx)
	if self == nil {
		return p.slotGateway(ctx, slot)
	}
	_, port := p.cacheBroker()
	if err := p.ensureForwarder(ctx, *self, port); err != nil {
		return "", err
	}
	err := p.cli.NetworkConnect(ctx, slotNetwork(slot), cacheForwarder, nil)
	if err != nil && !alreadyConnected(err) {
		return "", err
	}
	info, err := p.cli.ContainerInspect(ctx, cacheForwarder)
	if err != nil {
		return "", err
	}
	if info.NetworkSettings != nil {
		if n := info.NetworkSettings.Networks[slotNetwork(slot)]; n != nil && n.IPAddress != "" {
			return n.IPAddress, nil
		}
	}
	return "", errors.New("the cache forwarder has no address on " + slotNetwork(slot))
}

// selfInfo describes the container Pitlane runs in.
type selfInfo struct {
	ID, Image, Exe string
	Network        string // a network of Pitlane's container the forwarder joins
	Target         string // Pitlane's address on it (name, or IP on the default bridge)
}

// selfContainer returns the container Pitlane runs in, nil when it runs on
// the host. It is looked up once.
func (p *Pool) selfContainer(ctx context.Context) *selfInfo {
	p.selfOnce.Do(func() {
		if _, err := os.Stat("/.dockerenv"); err != nil {
			return
		}
		host, err := os.Hostname()
		if err != nil {
			return
		}
		exe, err := os.Executable()
		if err != nil {
			return
		}
		info, err := p.cli.ContainerInspect(ctx, host)
		if err != nil || info.NetworkSettings == nil || info.Config == nil {
			p.log.Warn("running in a container that Docker does not know by its host name: runners reach the actions cache through the slot gateway", "hostname", host, "error", err)
			return
		}
		self := &selfInfo{ID: info.ID, Image: info.Image, Exe: exe}
		for name, n := range info.NetworkSettings.Networks {
			if n == nil || n.IPAddress == "" || strings.HasPrefix(name, "pitlane-") {
				continue
			}
			target := strings.TrimPrefix(info.Name, "/")
			if name == "bridge" { // no name resolution on the default bridge
				target = n.IPAddress
			}
			if self.Network == "" || name < self.Network {
				self.Network, self.Target = name, target
			}
		}
		if self.Network == "" {
			p.log.Warn("Pitlane's container has no network the actions cache forwarder can join: runners reach it through the slot gateway")
			return
		}
		p.self = self
	})
	return p.self
}

// ensureForwarder (re)creates the forwarder for this Pitlane container.
func (p *Pool) ensureForwarder(ctx context.Context, self selfInfo, port int) error {
	p.fwdMu.Lock()
	defer p.fwdMu.Unlock()
	spec := self.ID + "/" + strconv.Itoa(port)
	if info, err := p.cli.ContainerInspect(ctx, cacheForwarder); err == nil {
		if info.Config != nil && info.Config.Labels[LabelSpec] == spec && info.State != nil && info.State.Running {
			return nil
		}
		_ = p.cli.ContainerRemove(ctx, cacheForwarder, container.RemoveOptions{Force: true})
	}
	ps := strconv.Itoa(port)
	_, err := p.cli.ContainerCreate(ctx, &container.Config{
		Image: self.Image, Entrypoint: []string{self.Exe}, Cmd: []string{"cache-forward", ":" + ps, net.JoinHostPort(self.Target, ps)},
		User:   "65534:65534", // nobody: it only relays bytes
		Labels: map[string]string{LabelManaged: "true", LabelRole: "cache-forward", LabelSpec: spec},
	}, &container.HostConfig{
		RestartPolicy:  container.RestartPolicy{Name: container.RestartPolicyUnlessStopped},
		CapDrop:        []string{"ALL"},
		SecurityOpt:    []string{"no-new-privileges"},
		ReadonlyRootfs: true,
		LogConfig:      runnerLogConfig(),
	}, &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{self.Network: {}}}, nil, cacheForwarder)
	if err != nil {
		return fmt.Errorf("create the actions cache forwarder: %w", err)
	}
	return p.cli.ContainerStart(ctx, cacheForwarder, container.StartOptions{})
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

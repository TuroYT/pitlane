package pool

import (
	"context"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
)

// RunnerNames returns the runner names of every Pitlane runner container
// that still exists, running or not. It changes no slot state.
func (p *Pool) RunnerNames(ctx context.Context) (map[string]bool, error) {
	list, err := p.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: filters.NewArgs(
		filters.Arg("label", LabelManaged+"=true"), filters.Arg("label", LabelRole+"=runner"))})
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, c := range list {
		out[c.Labels[LabelRunner]] = true
		for _, n := range c.Names {
			if len(n) > 1 {
				out[n[1:]] = true // "/pitlane-xxxx"
			}
		}
	}
	return out, nil
}

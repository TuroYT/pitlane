package pool

import (
	"context"
	"testing"
)

func TestExecThroughDocker(t *testing.T) {
	p := newTestPool("owner")
	routes, f := execRoutes(t, func(_, script string) (string, int) { return "out of " + script, 3 }, "box")
	newDockerAPI(t, p, routes)
	out, code, err := p.exec(context.Background(), "box", "echo hi")
	if err != nil || code != 3 || out != "out of echo hi" {
		t.Fatalf("exec: %q %d %v", out, code, err)
	}
	if got := f.ran(); len(got) != 1 || got[0] != "box: echo hi" {
		t.Fatalf("ran %q", got)
	}
	// Docker refusing to create the exec is an error, not an exit code.
	routes, _ = execRoutes(t, func(_, _ string) (string, int) { return "", -1 }, "box")
	newDockerAPI(t, p, routes)
	if _, code, err := p.exec(context.Background(), "box", "x"); err == nil || code != -1 {
		t.Fatalf("exec on a failing daemon: %d %v", code, err)
	}
}

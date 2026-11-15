package server

import (
	"container/list"
	"sync"
)

// maxJobIDs bounds the cache of GitHub job IDs (see handleGitHubJob).
const maxJobIDs = 4096

// jobKey is a job a runner ran: the runner name and the workflow run, so a
// reused runner name never gets another job's ID.
type jobKey struct {
	runner string
	runID  int64
}

// jobIDCache maps a runner's job to its GitHub job ID. It keeps at most max
// entries (maxJobIDs when 0) and drops the least recently used one first.
// The zero value is ready to use.
type jobIDCache struct {
	mu    sync.Mutex
	max   int
	order *list.List // of *jobIDEntry, most recently used first
	byKey map[jobKey]*list.Element
}

type jobIDEntry struct {
	key jobKey
	id  int64
}

func (c *jobIDCache) get(k jobKey) (int64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.byKey[k]
	if !ok {
		return 0, false
	}
	c.order.MoveToFront(e)
	return e.Value.(*jobIDEntry).id, true
}

func (c *jobIDCache) put(k jobKey, id int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byKey == nil {
		c.byKey, c.order = map[jobKey]*list.Element{}, list.New()
	}
	if e, ok := c.byKey[k]; ok {
		e.Value.(*jobIDEntry).id = id
		c.order.MoveToFront(e)
		return
	}
	c.byKey[k] = c.order.PushFront(&jobIDEntry{k, id})
	limit := c.max
	if limit <= 0 {
		limit = maxJobIDs
	}
	for c.order.Len() > limit {
		last := c.order.Back()
		c.order.Remove(last)
		delete(c.byKey, last.Value.(*jobIDEntry).key)
	}
}

func (c *jobIDCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.byKey)
}

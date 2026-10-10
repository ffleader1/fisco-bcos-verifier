package fisco

import (
	"context"
	"sync"
	"time"

	"buoyup/fisco/internal/ledger"
)

// commitLog remembers when each node told us it had written a block. Every node pushes a block-number notice to
// its own channel connection the moment it commits a block, so the arrival times show the order in which the
// nodes wrote it (as heard by this service: loopback latency included).
type commitLog struct {
	mu   sync.Mutex
	seen map[int64]map[string]time.Time
	max  int64
}

const commitLogKeep = 512 // blocks kept in memory

func (c *commitLog) record(node string, n int64, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seen == nil {
		c.seen = map[int64]map[string]time.Time{}
	}
	if c.seen[n] == nil {
		c.seen[n] = map[string]time.Time{}
	}
	if _, dup := c.seen[n][node]; !dup {
		c.seen[n][node] = at
	}
	if n > c.max {
		c.max = n
		for b := range c.seen {
			if b < c.max-commitLogKeep {
				delete(c.seen, b)
			}
		}
	}
}

func (c *commitLog) get(n int64, node string) (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	at, ok := c.seen[n][node]
	return at, ok
}

// watchCommits subscribes to the block notices of every connected node. A node that cannot subscribe simply never
// reports; the demo then shows it as "not heard from".
func (l *Ledger) watchCommits() {
	for _, n := range l.nodes {
		if n.c == nil {
			continue
		}
		name := n.name
		_ = n.c.SubscribeBlockNumberNotify(func(bn int64) { l.commits.record(name, bn, time.Now()) })
	}
}

// BlockCommits implements ledger.CommitWatcher.
func (l *Ledger) BlockCommits(ctx context.Context, n int64, wait time.Duration) []ledger.NodeCommit {
	deadline := time.Now().Add(wait)
	for {
		out, all := make([]ledger.NodeCommit, 0, len(l.nodes)), true
		for _, nd := range l.nodes {
			at, ok := l.commits.get(n, nd.name)
			out = append(out, ledger.NodeCommit{Node: nd.name, At: at, Seen: ok})
			all = all && ok
		}
		if all || time.Now().After(deadline) {
			return out
		}
		select {
		case <-ctx.Done():
			return out
		case <-time.After(5 * time.Millisecond):
		}
	}
}

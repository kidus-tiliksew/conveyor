package httpapi

// The workspace attention-count cache the pending-proposals badge reads
// (req-operator-next-action AC-2.7; component-next-action, One attention
// definition).

import (
	"context"
	"sync"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/changes"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// attentionCountReuse bounds how long a counted workspace total is served:
// a badge may lag a change by at most this long, and the count is never
// counted twice inside the window (req-operator-next-action AC-2.7).
const attentionCountReuse = 5 * time.Second

type attentionCountEntry struct {
	count int
	at    time.Time
}

// attentionCountCache serves one workspace's attention count for
// attentionCountReuse. It is keyed by workspace, so one workspace's change
// never evicts another's, and it is invalidated by the daemon's own change
// notices: a notice for a workspace drops that workspace's entry, so the
// next read counts again rather than waiting out the window. A dropped
// notice costs at most attentionCountReuse, the notice channels hold one
// notice and are drained non-blockingly on the read path
// (component-persistence, Change notices; DEC-99(1)).
type attentionCountCache struct {
	mu      sync.Mutex
	entries map[string]attentionCountEntry
	// clock replaces time.Now in tests; nil selects time.Now.
	clock   func() time.Time
	notices []*changes.Subscription
}

// subscribe starts listening for the notices that can change a count: a
// recorded task event, a claimable or rescheduled job, and a moved deadline.
func (c *attentionCountCache) subscribe(hub *changes.Hub) {
	if hub == nil {
		return
	}
	for _, kind := range []changes.Kind{changes.Task, changes.Job, changes.Deadline} {
		c.notices = append(c.notices, hub.SubscribeAll(kind))
	}
}

// drain drops the entries the delivered notices name. It never blocks: the
// read path calls it before every lookup, so a stale entry cannot outlive
// the notice that named it.
func (c *attentionCountCache) drain() {
	if len(c.notices) == 0 {
		return
	}
	var delivered []changes.Notice
	for _, subscription := range c.notices {
	drain:
		for {
			select {
			case notice := <-subscription.C():
				delivered = append(delivered, notice)
			default:
				break drain
			}
		}
	}
	if len(delivered) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, notice := range delivered {
		if notice.Workspace == "" {
			c.entries = nil
			return
		}
		delete(c.entries, notice.Workspace)
	}
}

func (c *attentionCountCache) now() time.Time {
	if c.clock != nil {
		return c.clock()
	}
	return time.Now()
}

// get returns the workspace's cached count when it is younger than
// attentionCountReuse.
func (c *attentionCountCache) get(workspace string) (int, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[workspace]
	if !ok || c.now().Sub(entry.at) >= attentionCountReuse {
		return 0, false
	}
	return entry.count, true
}

func (c *attentionCountCache) store(workspace string, count int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]attentionCountEntry{}
	}
	c.entries[workspace] = attentionCountEntry{count: count, at: c.now()}
}

// attentionCount is the workspace's task attention count, served from the
// cache when a recent count is fresh. A failed count is never cached.
func (s *Server) attentionCount(ctx context.Context) (int, error) {
	if s.attention == nil {
		return s.Store.CountOperatorAttention(ctx)
	}
	workspace, _ := store.WorkspaceFromContext(ctx)
	s.attention.drain()
	if count, ok := s.attention.get(workspace); ok {
		return count, nil
	}
	count, err := s.Store.CountOperatorAttention(ctx)
	if err != nil {
		return 0, err
	}
	s.attention.store(workspace, count)
	return count, nil
}

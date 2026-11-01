package httpapi

// The shared merge-readiness cache that task detail and next-action reads use
// (component-http-api, Request-scoped detail reads), and the caller capability
// check those reads share.

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

const mergeReadinessReuse = 10 * time.Second

// taskRunMergeExecutionGate projects the remaining merge act of an approved
// task behind the merge gate (req-review-gates-evidence AC-1.1;
// component-mcp-protocol, Run-order plane). A gate-off approved task never
// reaches here: the runtime's merge path owns it. The readiness read is the
// task detail's ReadMergeReadiness (component-submission-merge); this
// projection never requests a merge. A missing hook or failed read yields a
// gate without readiness, which no client treats as mergeable.

// mergeReadinessCache reuses successful merge-readiness reads for
// mergeReadinessReuse. Failed reads are never cached, and a key covers
// the task state, merge approval, and approved head so a transition or new
// approval reads again. Entries are stored per full key under the task, so a
// completion for one key never evicts another key's entry. Concurrent misses
// for one task and key share one in-flight hook call; the mutex is never held
// across the hook (component-http-api, Request-scoped detail reads).
type mergeReadinessCache struct {
	mu      sync.Mutex
	entries map[string]map[string]mergeReadinessEntry
	flights map[string][]*mergeReadinessFlight
	clock   func() time.Time
}

type mergeReadinessEntry struct {
	key       string
	readiness dispatch.MergeReadiness
	readAt    time.Time
}

// mergeReadinessFlight is one in-flight hook call. done closes after
// readiness and err are set. superseded marks a flight whose generation
// forget advanced, so its result is never stored.
type mergeReadinessFlight struct {
	key        string
	done       chan struct{}
	readiness  dispatch.MergeReadiness
	err        error
	superseded bool
}

func (c *mergeReadinessCache) now() time.Time {
	if c.clock != nil {
		return c.clock()
	}
	return time.Now()
}

func (c *mergeReadinessCache) read(ctx context.Context, task core.Task, hook func(context.Context, core.Task) (dispatch.MergeReadiness, error)) (dispatch.MergeReadiness, error) {
	readiness, _, err := c.readObserved(ctx, task, hook, time.Time{})
	return readiness, err
}

// readObserved reads readiness through the cache and reports whether the
// result came from a hook call that completed at or after since: this
// caller's own call, a shared flight, or an entry stored at or after since.
// Such a call may have written task state, so the caller reads the task
// again. An entry stored before since was already observed by the caller's
// own task read.
func (c *mergeReadinessCache) readObserved(ctx context.Context, task core.Task, hook func(context.Context, core.Task) (dispatch.MergeReadiness, error), since time.Time) (dispatch.MergeReadiness, bool, error) {
	id := mergeReadinessID(ctx, task.ID)
	key := mergeReadinessKey(task)
	for {
		c.mu.Lock()
		if entry, ok := c.entries[id][key]; ok && c.now().Sub(entry.readAt) < mergeReadinessReuse {
			c.mu.Unlock()
			return entry.readiness, !entry.readAt.Before(since), nil
		}
		var shared *mergeReadinessFlight
		for _, flight := range c.flights[id] {
			if flight.key == key && !flight.superseded {
				shared = flight
			}
		}
		if shared != nil {
			c.mu.Unlock()
			select {
			case <-ctx.Done():
				return dispatch.MergeReadiness{}, false, ctx.Err()
			case <-shared.done:
			}
			if shared.err != nil && mergeReadinessContextError(shared.err) && ctx.Err() == nil {
				// The leader's own context ended the call; this caller is
				// still live, so it calls the hook itself.
				continue
			}
			return shared.readiness, shared.err == nil, shared.err
		}
		flight := &mergeReadinessFlight{key: key, done: make(chan struct{})}
		if c.flights == nil {
			c.flights = map[string][]*mergeReadinessFlight{}
		}
		c.flights[id] = append(c.flights[id], flight)
		c.mu.Unlock()
		readiness, err := hook(ctx, task)
		c.finish(id, flight, readiness, err)
		return readiness, err == nil, err
	}
}

func (c *mergeReadinessCache) finish(id string, flight *mergeReadinessFlight, readiness dispatch.MergeReadiness, err error) {
	c.mu.Lock()
	defer close(flight.done)
	defer c.mu.Unlock()
	flight.readiness, flight.err = readiness, err
	remaining := c.flights[id][:0]
	for _, other := range c.flights[id] {
		if other != flight {
			remaining = append(remaining, other)
		}
	}
	if len(remaining) == 0 {
		delete(c.flights, id)
	} else {
		c.flights[id] = remaining
	}
	if err != nil || flight.superseded {
		return
	}
	now := c.now()
	if c.entries == nil {
		c.entries = map[string]map[string]mergeReadinessEntry{}
	}
	for cachedID, keys := range c.entries {
		for cachedKey, entry := range keys {
			if now.Sub(entry.readAt) >= mergeReadinessReuse {
				delete(keys, cachedKey)
			}
		}
		if len(keys) == 0 {
			delete(c.entries, cachedID)
		}
	}
	if c.entries[id] == nil {
		c.entries[id] = map[string]mergeReadinessEntry{}
	}
	c.entries[id][flight.key] = mergeReadinessEntry{key: flight.key, readiness: readiness, readAt: now}
}

// forget drops every key's entry for the task and advances its generation:
// every flight already running for it is superseded, so none can store its
// result afterward, and later readers start a new call.
func (c *mergeReadinessCache) forget(ctx context.Context, taskID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := mergeReadinessID(ctx, taskID)
	delete(c.entries, id)
	for _, flight := range c.flights[id] {
		flight.superseded = true
	}
}

func mergeReadinessContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func mergeReadinessID(ctx context.Context, taskID string) string {
	workspaceID, _ := store.WorkspaceFromContext(ctx)
	return workspaceID + "\x00" + taskID
}

func (s *Server) callerCapability(ctx context.Context, capability core.Capability) (bool, error) {
	credential, ok := store.CredentialFromContext(ctx)
	if !ok || credential.Kind != core.CredentialUser {
		return false, nil
	}
	workspaceID, scoped := store.WorkspaceFromContext(ctx)
	if scoped && s.Memberships != nil {
		return s.Memberships.AuthorizeWorkspace(ctx, credential.OwnerUserID, workspaceID, capability)
	}
	return credential.Scope == core.CredentialScopeOperator, nil
}

// lastKnown returns the cached readiness peek for a next-action projection.
// It never calls the forge, so a next-action read cannot loop forge calls;
// a missing or expired entry yields nil, which projects as UNKNOWN (DEC-64(3)).
func (c *mergeReadinessCache) lastKnown(ctx context.Context, task core.Task) *nextaction.MergeReadiness {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[mergeReadinessID(ctx, task.ID)][mergeReadinessKey(task)]
	if !ok || c.now().Sub(entry.readAt) >= mergeReadinessReuse {
		return nil
	}
	return &nextaction.MergeReadiness{State: entry.readiness.State, HeadSHA: entry.readiness.HeadSHA, Number: entry.readiness.Number}
}

// mergeReadinessKey is the one readiness cache key for every reader and
// writer: the reviewed head at an awaiting merge_approval gate, otherwise the
// approved head (falling back to the reviewed head), so a replaced head
// misses (DEC-64(3)).
func mergeReadinessKey(task core.Task) string {
	head := task.ApprovedHeadSHA
	if head == "" || (task.State == core.TaskAwaiting && task.GateKind == core.GateKindMergeApproval) {
		head = task.ReviewedHeadSHA
	}
	return string(task.State) + "\x00" + strconv.FormatBool(task.MergeApproval) + "\x00" + head
}

package httpapi

// The shared merge-readiness cache that task detail and next-action reads use
// (component-http-api, Request-scoped detail reads), and the caller capability
// check those reads share.

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// mergeReadinessReuse bounds forge reads from repeated detail and next-action
// reads: a successful readiness result is reused for this long per task, task
// state, and approved head. The merge route always rechecks readiness itself.
const mergeReadinessReuse = 10 * time.Second

// mergeReadinessCache reuses successful merge-readiness reads for
// mergeReadinessReuse. Failed reads are never cached, and a key covers
// the task state, merge approval, and approved head so a transition or new
// approval reads again. Entries are stored per full key under the task, so a
// completion for one key never evicts another key's entry. Concurrent misses
// for one task and key share one in-flight hook call; the mutex is never held
// across the hook (component-http-api, Request-scoped detail reads).
//
// A change of the value a next-action read can peek, and the expiry of one,
// raises an in-process workspace notice through notify: a stored value that
// differs from the live one, forget dropping a live value, and an expired
// value found or swept. A response memo and a conditional statement read
// that peeked the old value are dropped or answer a new tag (DEC-140(3);
// component-next-action invariant 58). The notice stays in this daemon,
// because each daemon reads its own cache; it is never persisted or sent to
// a peer.
type mergeReadinessCache struct {
	mu      sync.Mutex
	entries map[string]map[string]mergeReadinessEntry
	flights map[string][]*mergeReadinessFlight
	clock   func() time.Time
	// notify publishes a workspace notice after the mutex is released.
	notify func(workspace string)
	// revision advances each time a stored value differs from the live one,
	// so a read can tell a value replaced and restored from one held still.
	revision uint64
}

type mergeReadinessEntry struct {
	key       string
	workspace string
	readiness dispatch.MergeReadiness
	readAt    time.Time
	revision  uint64
}

// expires is the instant the entry stops being served.
func (e mergeReadinessEntry) expires() time.Time { return e.readAt.Add(mergeReadinessReuse) }

// publish sends one in-process workspace notice per named workspace. The
// caller has released the mutex.
func (c *mergeReadinessCache) publish(workspaces map[string]struct{}) {
	if c.notify == nil {
		return
	}
	for workspace := range workspaces {
		c.notify(workspace)
	}
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
		workspaceID, _ := store.WorkspaceFromContext(ctx)
		c.finish(id, workspaceID, flight, readiness, err)
		return readiness, err == nil, err
	}
}

func (c *mergeReadinessCache) finish(id, workspaceID string, flight *mergeReadinessFlight, readiness dispatch.MergeReadiness, err error) {
	changed := map[string]struct{}{}
	defer c.publish(changed)
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
	previous, hadLive := c.entries[id][flight.key]
	hadLive = hadLive && now.Sub(previous.readAt) < mergeReadinessReuse
	for cachedID, keys := range c.entries {
		for cachedKey, entry := range keys {
			if now.Sub(entry.readAt) >= mergeReadinessReuse {
				delete(keys, cachedKey)
				changed[entry.workspace] = struct{}{}
			}
		}
		if len(keys) == 0 {
			delete(c.entries, cachedID)
		}
	}
	if c.entries[id] == nil {
		c.entries[id] = map[string]mergeReadinessEntry{}
	}
	// An unchanged live value keeps its revision and raises no notice: what
	// a read peeks is the same, and its later expiry only moves a horizon
	// outward.
	revision := previous.revision
	if !hadLive || previous.readiness != readiness {
		c.revision++
		revision = c.revision
		changed[workspaceID] = struct{}{}
	}
	c.entries[id][flight.key] = mergeReadinessEntry{key: flight.key, workspace: workspaceID, readiness: readiness, readAt: now, revision: revision}
}

// forget drops every key's entry for the task and advances its generation:
// every flight already running for it is superseded, so none can store its
// result afterward, and later readers start a new call.
func (c *mergeReadinessCache) forget(ctx context.Context, taskID string) {
	changed := map[string]struct{}{}
	defer c.publish(changed)
	c.mu.Lock()
	defer c.mu.Unlock()
	id := mergeReadinessID(ctx, taskID)
	for _, entry := range c.entries[id] {
		// A dropped live value changes what a read peeks; a dropped expired
		// value expired unseen and is announced here.
		changed[entry.workspace] = struct{}{}
	}
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
//
// The peek joins the read's coverage: the value's expiry bounds the
// response memo and the conditional tag (memoHorizon), and a statement read
// records the value so its answer can check it again (readinessReads). An
// expired value found here is dropped and announced, so a memo that read
// it is dropped even when no new forge read follows (DEC-140(3);
// component-next-action invariant 58).
func (c *mergeReadinessCache) lastKnown(ctx context.Context, task core.Task) *nextaction.MergeReadiness {
	id, key := mergeReadinessID(ctx, task.ID), mergeReadinessKey(task)
	value, expires, revision := c.peek(id, key)
	if !expires.IsZero() {
		memoHorizon(ctx, expires)
	}
	if reads, ok := readinessReadsFrom(ctx); ok {
		reads.add(id, key, value, revision)
	}
	if value == nil {
		return nil
	}
	return &nextaction.MergeReadiness{State: value.State, HeadSHA: value.HeadSHA, Number: value.Number}
}

// peek returns the live value for one task and key with its expiry and
// revision, all read under one hold of the mutex, or nil, a zero time, and
// revision 0. It drops and announces an expired value.
func (c *mergeReadinessCache) peek(id, key string) (*dispatch.MergeReadiness, time.Time, uint64) {
	changed := map[string]struct{}{}
	defer c.publish(changed)
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[id][key]
	if !ok {
		return nil, time.Time{}, 0
	}
	if c.now().Sub(entry.readAt) >= mergeReadinessReuse {
		delete(c.entries[id], key)
		if len(c.entries[id]) == 0 {
			delete(c.entries, id)
		}
		changed[entry.workspace] = struct{}{}
		return nil, time.Time{}, 0
	}
	readiness := entry.readiness
	return &readiness, entry.expires(), entry.revision
}

type readinessReadsKey struct{}

// readinessReads records the merge-readiness values one statement read
// peeked, so its answer can check that each is still the live value
// (component-next-action invariant 58).
type readinessReads struct {
	mu    sync.Mutex
	reads []readinessRead
}

type readinessRead struct {
	id, key  string
	value    *dispatch.MergeReadiness
	revision uint64
}

func (r *readinessReads) add(id, key string, value *dispatch.MergeReadiness, revision uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reads = append(r.reads, readinessRead{id: id, key: key, value: value, revision: revision})
}

func (r *readinessReads) addAll(reads []readinessRead) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reads = append(r.reads, reads...)
}

func (r *readinessReads) snapshot() []readinessRead {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]readinessRead(nil), r.reads...)
}

// digest encodes every recorded peek, in a stable order, for a tag: a read
// that peeked another value, or no value, gets another tag even when the
// body it served is the same (component-next-action invariant 58).
func (r *readinessReads) digest() []byte {
	reads := r.snapshot()
	lines := make([]string, 0, len(reads))
	for _, read := range reads {
		value := "none"
		if read.value != nil {
			value = read.value.State + "\x00" + read.value.HeadSHA + "\x00" + read.value.URL + "\x00" + strconv.Itoa(read.value.Number)
		}
		lines = append(lines, read.id+"\x00"+read.key+"\x00"+value)
	}
	slices.Sort(lines)
	lines = slices.Compact(lines)
	return []byte(strings.Join(lines, "\n"))
}

// readinessReadsFrom returns the recorder of the statement read ctx serves.
func readinessReadsFrom(ctx context.Context) (*readinessReads, bool) {
	reads, ok := ctx.Value(readinessReadsKey{}).(*readinessReads)
	return reads, ok
}

// unchanged reports whether every recorded peek still returns its value at
// the same revision.
func (r *readinessReads) unchanged(c *mergeReadinessCache) bool {
	r.mu.Lock()
	reads := append([]readinessRead(nil), r.reads...)
	r.mu.Unlock()
	for _, read := range reads {
		current, _, revision := c.peek(read.id, read.key)
		if revision != read.revision || (current == nil) != (read.value == nil) || (current != nil && *current != *read.value) {
			return false
		}
	}
	return true
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

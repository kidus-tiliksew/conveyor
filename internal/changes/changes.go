// Package changes delivers change notices inside one daemon: a committed
// transaction tells the parts of the daemon that wait on it that something
// they care about changed, so nothing has to read the database on a timer
// (DEC-99(1); feature-quiet-idle-runtime, Change notices;
// component-persistence, Change notices).
//
// A Hub routes notices to subscribers. A Batch holds the notices one
// transaction raises until it commits. No notice is persisted: a lost notice
// is recovered by the next catch-up, the peer tail, or the next start.
package changes

import (
	"context"
	"strings"
	"sync"
	"time"
)

// Kind names what changed.
type Kind string

const (
	// Job: a job became claimable or was rescheduled in the workspace.
	Job Kind = "job"
	// Task: an event was recorded on a task.
	Task Kind = "task"
	// Deadline: a recorded deadline moved earlier; At carries it when known.
	Deadline Kind = "deadline"
	// Workspace: a workspace was created or its configuration changed.
	Workspace Kind = "workspace"
)

func (k Kind) valid() bool {
	switch k {
	case Job, Task, Deadline, Workspace:
		return true
	}
	return false
}

// Notice is one change. An empty Workspace means every workspace; a Task
// notice with an empty TaskID means every task of its workspace.
type Notice struct {
	Workspace string
	Kind      Kind
	TaskID    string
	At        time.Time
}

// Payload is the peer wire form, "<workspace>:<kind>[:<task ID>]". A
// deadline's time does not travel; a peer re-reads its deadlines.
func (n Notice) Payload() string {
	payload := n.Workspace + ":" + string(n.Kind)
	if n.Kind == Task && n.TaskID != "" {
		payload += ":" + n.TaskID
	}
	return payload
}

// ParsePayload reads the peer wire form. It reports false for anything else.
func ParsePayload(payload string) (Notice, bool) {
	parts := strings.SplitN(payload, ":", 3)
	if len(parts) < 2 || parts[0] == "" {
		return Notice{}, false
	}
	n := Notice{Workspace: parts[0], Kind: Kind(parts[1])}
	if !n.Kind.valid() {
		return Notice{}, false
	}
	if len(parts) == 3 {
		if n.Kind != Task || parts[2] == "" {
			return Notice{}, false
		}
		n.TaskID = parts[2]
	}
	return n, true
}

// Subscription receives notices on a channel buffered for one notice.
type Subscription struct {
	hub       *Hub
	workspace string
	every     bool
	kind      Kind
	taskID    string
	c         chan Notice
	once      sync.Once
}

// C is the subscription's channel. It is never closed.
func (s *Subscription) C() <-chan Notice {
	return s.c
}

// Close stops delivery. Closing twice is harmless.
func (s *Subscription) Close() {
	s.once.Do(func() {
		if s.hub != nil {
			s.hub.remove(s)
		}
	})
}

func (s *Subscription) matches(n Notice) bool {
	if n.Kind != s.kind {
		return false
	}
	if !s.every && n.Workspace != "" && n.Workspace != s.workspace {
		return false
	}
	if s.taskID == "" {
		return true
	}
	return n.Workspace == "" || n.TaskID == "" || n.TaskID == s.taskID
}

// deliver never blocks. A full buffer keeps one pending notice and folds the
// new one into it, so the subscriber still wakes once and re-reads state.
func (s *Subscription) deliver(n Notice) {
	select {
	case s.c <- n:
		return
	default:
	}
	select {
	case pending := <-s.c:
		n = merge(pending, n)
	default:
	}
	select {
	case s.c <- n:
	default:
	}
}

// merge folds two notices for the same subscriber. A wildcard wins, and two
// deadlines keep the earlier known time; an unknown time means "re-read now".
func merge(pending, next Notice) Notice {
	out := pending
	if next.Workspace == "" {
		out.Workspace = ""
	}
	if next.TaskID != out.TaskID {
		out.TaskID = ""
	}
	if out.Kind == Deadline {
		switch {
		case pending.At.IsZero() || next.At.IsZero():
			out.At = time.Time{}
		case next.At.Before(pending.At):
			out.At = next.At
		}
	}
	return out
}

// Hub routes notices to subscriptions. The zero value is not usable; a nil
// Hub publishes nothing and returns subscriptions that never fire.
type Hub struct {
	mu   sync.Mutex
	subs map[*Subscription]struct{}
}

// NewHub returns an empty hub.
func NewHub() *Hub {
	return &Hub{subs: map[*Subscription]struct{}{}}
}

// Subscribe receives the workspace's notices of one kind, including notices
// for every workspace.
func (h *Hub) Subscribe(workspace string, kind Kind) *Subscription {
	return h.add(&Subscription{workspace: workspace, kind: kind, c: make(chan Notice, 1)})
}

// SubscribeAll receives notices of one kind for every workspace. The
// daemon's workspace registration watcher uses it to learn of workspaces a
// peer created (component-runtime, Start-up checks and background loops).
func (h *Hub) SubscribeAll(kind Kind) *Subscription {
	return h.add(&Subscription{every: true, kind: kind, c: make(chan Notice, 1)})
}

// SubscribeTask receives the task's Task notices, including notices for
// every task of the workspace and for every workspace.
func (h *Hub) SubscribeTask(workspace, taskID string) *Subscription {
	return h.add(&Subscription{workspace: workspace, kind: Task, taskID: taskID, c: make(chan Notice, 1)})
}

func (h *Hub) add(s *Subscription) *Subscription {
	if h == nil {
		return s
	}
	s.hub = h
	h.mu.Lock()
	h.subs[s] = struct{}{}
	h.mu.Unlock()
	return s
}

func (h *Hub) remove(s *Subscription) {
	h.mu.Lock()
	delete(h.subs, s)
	h.mu.Unlock()
}

// Publish delivers n to every matching subscription. It never blocks.
func (h *Hub) Publish(n Notice) {
	if h == nil || !n.Kind.valid() {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs {
		if s.matches(n) {
			s.deliver(n)
		}
	}
}

// Begin starts the batch for one transaction.
func (h *Hub) Begin() *Batch {
	return &Batch{hub: h}
}

// Batch holds the notices one transaction raises until it commits.
type Batch struct {
	hub     *Hub
	parent  *Batch
	mu      sync.Mutex
	notices []Notice
	done    bool
}

// Raise adds a notice; a duplicate of a held notice is dropped.
func (b *Batch) Raise(n Notice) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.done {
		return
	}
	for i, held := range b.notices {
		if held.Workspace == n.Workspace && held.Kind == n.Kind && held.TaskID == n.TaskID {
			if n.Kind == Deadline {
				b.notices[i] = merge(held, n)
			}
			return
		}
	}
	b.notices = append(b.notices, n)
}

// Notices returns the held notices in raise order.
func (b *Batch) Notices() []Notice {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]Notice(nil), b.notices...)
}

// Commit publishes the held notices; call it only after the transaction
// committed. A savepoint batch hands them to its parent instead.
func (b *Batch) Commit() {
	if b == nil {
		return
	}
	b.mu.Lock()
	if b.done {
		b.mu.Unlock()
		return
	}
	b.done = true
	notices := b.notices
	b.notices = nil
	b.mu.Unlock()
	for _, n := range notices {
		if b.parent != nil {
			b.parent.Raise(n)
		} else {
			b.hub.Publish(n)
		}
	}
}

// Discard drops the held notices: the transaction rolled back. Discarding a
// committed batch does nothing.
func (b *Batch) Discard() {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.done = true
	b.notices = nil
	b.mu.Unlock()
}

// Savepoint starts a child batch whose Commit hands its notices to b.
func (b *Batch) Savepoint() *Batch {
	if b == nil {
		return nil
	}
	return &Batch{hub: b.hub, parent: b}
}

type batchKey struct{}

// WithBatch binds a transaction's batch to ctx. A nil batch leaves ctx alone.
func WithBatch(ctx context.Context, b *Batch) context.Context {
	if b == nil {
		return ctx
	}
	return context.WithValue(ctx, batchKey{}, b)
}

// BatchFrom returns the batch bound to ctx.
func BatchFrom(ctx context.Context) (*Batch, bool) {
	b, ok := ctx.Value(batchKey{}).(*Batch)
	return b, ok && b != nil
}

// Raise adds n to the batch bound to ctx, or publishes it on hub at once
// when ctx carries none: the change has already committed.
func Raise(ctx context.Context, hub *Hub, n Notice) {
	if b, ok := BatchFrom(ctx); ok {
		b.Raise(n)
		return
	}
	hub.Publish(n)
}

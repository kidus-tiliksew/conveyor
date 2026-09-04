package logqueue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
	queueargs "github.com/kidus-tiliksew/conveyor/internal/queue"
)

// Shadow runs the log queue in observe-only mode beside another driver.
//
// Two jobs. First, it keeps the log's job streams truthful while River
// executes: every claim and outcome River makes is mirrored onto the
// stream, so at cutover the log queue inherits real state rather than a
// backlog of jobs it believes are still waiting. Second, at every claim it
// asks the log queue's own scheduler what it would have picked and records
// agreement or the kind of disagreement. The soak gate for phase 3 is a
// report with no unknown or not-claimable verdicts; order differences
// between two schedulers with different tie-breaks are expected and shown
// separately.
//
// Log-core migration plan, phase 3, task 3.3.
type Shadow struct {
	log     eventlog.Store
	runtime *Runtime
	now     func() time.Time
	logf    func(string, ...any)
	mu      sync.Mutex
	counts  map[string]*ShadowCounts // workspace|kind
	recent  []ShadowDisagreement
	started bool
}

// Verdict classifies one River claim against the log queue's choice.
type Verdict string

const (
	// VerdictAgree: the log queue would have claimed the same job.
	VerdictAgree Verdict = "agree"
	// VerdictOrder: the job was claimable in the log but not its first
	// choice; the two schedulers break ties differently.
	VerdictOrder Verdict = "order"
	// VerdictNotClaimable: the log had the job but would not run it yet,
	// usually a retry time that differs from River's.
	VerdictNotClaimable Verdict = "not_claimable"
	// VerdictUnknown: the log had no active job for the key at all. A gap in
	// dual-enqueue, or a job enqueued before shadowing began.
	VerdictUnknown Verdict = "unknown"
)

// ShadowCounts tallies verdicts for one workspace and kind.
type ShadowCounts struct {
	Workspace    string `json:"workspace"`
	Kind         string `json:"kind"`
	Claims       int    `json:"claims"`
	Agree        int    `json:"agree"`
	Order        int    `json:"order"`
	NotClaimable int    `json:"not_claimable"`
	Unknown      int    `json:"unknown"`
	// Mirrored counts outcomes written to the log for River's jobs.
	Mirrored int `json:"mirrored"`
	// MirrorErrors counts appends that failed; each is also logged.
	MirrorErrors int `json:"mirror_errors"`
}

// ShadowDisagreement is one claim the log queue would have made differently.
type ShadowDisagreement struct {
	At        time.Time `json:"at"`
	Workspace string    `json:"workspace"`
	Kind      string    `json:"kind"`
	Key       string    `json:"key"`
	Verdict   Verdict   `json:"verdict"`
	// Expected is the key the log queue would have claimed, when it had one.
	Expected string `json:"expected,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// ShadowReport is the comparison so far.
type ShadowReport struct {
	Counts []ShadowCounts       `json:"counts"`
	Recent []ShadowDisagreement `json:"recent_disagreements"`
}

// Clean reports whether every claim was one the log queue could have made:
// no unknown jobs and none it would not have run yet.
func (r ShadowReport) Clean() bool {
	for _, c := range r.Counts {
		if c.Unknown > 0 || c.NotClaimable > 0 || c.MirrorErrors > 0 {
			return false
		}
	}
	return true
}

const shadowRecentLimit = 50

// ShadowOptions configure a shadow.
type ShadowOptions struct {
	Workspaces   []string
	PollInterval time.Duration
	Now          func() time.Time
	Logf         func(string, ...any)
}

func NewShadow(log eventlog.Store, opts ShadowOptions) *Shadow {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	return &Shadow{
		log: log, now: opts.Now, logf: opts.Logf, counts: map[string]*ShadowCounts{},
		runtime: NewRuntime(log, Options{Workspaces: opts.Workspaces, PollInterval: opts.PollInterval, ClockInterval: -1, Now: opts.Now, Logf: opts.Logf, Observe: true}),
	}
}

// Start begins tailing; Stop ends it.
func (s *Shadow) Start(ctx context.Context) error {
	s.mu.Lock()
	s.started = true
	s.mu.Unlock()
	return s.runtime.Start(ctx)
}

func (s *Shadow) Stop(ctx context.Context) error { return s.runtime.Stop(ctx) }

// EnsureWorkspace adds a workspace to the observer.
func (s *Shadow) EnsureWorkspace(workspace string) error { return s.runtime.EnsureWorkspace(workspace) }

func (s *Shadow) counter(workspace, kind string) *ShadowCounts {
	key := workspace + "|" + kind
	c, ok := s.counts[key]
	if !ok {
		c = &ShadowCounts{Workspace: workspace, Kind: kind}
		s.counts[key] = c
	}
	return c
}

// Claimed records that River claimed a job and mirrors the claim. args are
// River's encoded args; attempt is the execution River is about to run.
func (s *Shadow) Claimed(ctx context.Context, workspace, kind, key string, args json.RawMessage, attempt, maxAttempts int) {
	now := s.now().UTC()
	// Read our own writes before judging: the runtime's index may lag the
	// store's dual-enqueue by up to one poll.
	_ = s.runtime.CatchUpNow(ctx, workspace)
	stream := StreamFor(kind, key)
	expected, hasExpected := s.runtime.Peek(workspace, kind, now)
	current, known := s.runtime.Job(workspace, stream)

	verdict := VerdictAgree
	detail := ""
	switch {
	case !known || !current.Active():
		verdict = VerdictUnknown
	case !current.Claimable(now):
		verdict = VerdictNotClaimable
		detail = fmt.Sprintf("state %s scheduled_at %s", current.State, current.ScheduledAt.Format(time.RFC3339))
	case hasExpected && expected.Key != key:
		verdict = VerdictOrder
	}
	s.mu.Lock()
	c := s.counter(workspace, kind)
	c.Claims++
	switch verdict {
	case VerdictAgree:
		c.Agree++
	case VerdictOrder:
		c.Order++
	case VerdictNotClaimable:
		c.NotClaimable++
	case VerdictUnknown:
		c.Unknown++
	}
	if verdict != VerdictAgree {
		d := ShadowDisagreement{At: now, Workspace: workspace, Kind: kind, Key: key, Verdict: verdict, Detail: detail}
		if hasExpected {
			d.Expected = expected.Key
		}
		s.recent = append(s.recent, d)
		if len(s.recent) > shadowRecentLimit {
			s.recent = s.recent[len(s.recent)-shadowRecentLimit:]
		}
		s.logf("queue shadow: %s %s %s: %s (log queue would claim %q) %s", workspace, kind, key, verdict, d.Expected, detail)
	}
	s.mu.Unlock()

	// Mirror. A job the log never saw is created first so the stream
	// carries River's execution history from here on.
	job, err := Load(ctx, s.log, workspace, stream)
	if err != nil {
		s.mirrorError(workspace, kind, "load", err)
		return
	}
	if !job.Active() {
		payload, _ := json.Marshal(enqueuedPayload{Kind: kind, Key: key, Args: args, MaxAttempts: maxAttempts, ScheduledAt: now})
		head, err := s.log.Append(ctx, workspace, stream, job.Head, []eventlog.NewEvent{{Kind: KindEnqueued, ActorID: "queue-shadow", ActorRole: "system", Payload: payload, At: now}})
		if err != nil {
			s.mirrorError(workspace, kind, "enqueue", err)
			return
		}
		job.Head = head
	}
	claim, _ := json.Marshal(claimedPayload{Attempt: attempt, Worker: "river", ClaimedAt: now})
	if _, err := s.log.Append(ctx, workspace, stream, job.Head, []eventlog.NewEvent{{Kind: KindClaimed, ActorID: "queue-shadow", ActorRole: "system", Payload: claim, At: now}}); err != nil {
		s.mirrorError(workspace, kind, "claim", err)
		return
	}
	s.mu.Lock()
	s.counter(workspace, kind).Mirrored++
	s.mu.Unlock()
}

// Outcome mirrors how River's execution ended. err is the handler's
// result; retryAt is River's next attempt time for a retry, zero otherwise.
func (s *Shadow) Outcome(ctx context.Context, workspace, kind, key string, attempt, maxAttempts int, err error, retryAt time.Time) {
	now := s.now().UTC()
	stream := StreamFor(kind, key)
	job, loadErr := Load(ctx, s.log, workspace, stream)
	if loadErr != nil {
		s.mirrorError(workspace, kind, "load", loadErr)
		return
	}
	if job.State != StateRunning {
		// Nothing to close: the claim was never mirrored.
		s.mirrorError(workspace, kind, "outcome", fmt.Errorf("stream %s is %s, not running", stream, job.State))
		return
	}
	payload := outcomePayload{Attempt: attempt}
	outcomeKind := KindCompleted
	var snooze *queueargs.SnoozeError
	switch {
	case err == nil:
	case errors.As(err, &snooze):
		outcomeKind = KindSnoozed
		payload.Until = now.Add(snooze.Duration)
	case attempt >= maxAttempts:
		outcomeKind = KindDiscarded
		payload.Error = err.Error()
	default:
		outcomeKind = KindFailed
		payload.Error = err.Error()
		payload.NextAt = retryAt
		if retryAt.IsZero() {
			payload.NextAt = now.Add(defaultRetryDelay(attempt))
		}
	}
	encoded, _ := json.Marshal(payload)
	if _, err := s.log.Append(ctx, workspace, stream, job.Head, []eventlog.NewEvent{{Kind: outcomeKind, ActorID: "queue-shadow", ActorRole: "system", Payload: encoded, At: now}}); err != nil {
		s.mirrorError(workspace, kind, outcomeKind, err)
		return
	}
	s.mu.Lock()
	s.counter(workspace, kind).Mirrored++
	s.mu.Unlock()
}

func (s *Shadow) mirrorError(workspace, kind, step string, err error) {
	s.mu.Lock()
	s.counter(workspace, kind).MirrorErrors++
	s.mu.Unlock()
	s.logf("queue shadow: %s %s: mirror %s: %v", workspace, kind, step, err)
}

// Report returns the comparison so far.
func (s *Shadow) Report() ShadowReport {
	s.mu.Lock()
	defer s.mu.Unlock()
	report := ShadowReport{Recent: append([]ShadowDisagreement(nil), s.recent...)}
	for _, c := range s.counts {
		report.Counts = append(report.Counts, *c)
	}
	sort.Slice(report.Counts, func(i, j int) bool {
		if report.Counts[i].Workspace != report.Counts[j].Workspace {
			return report.Counts[i].Workspace < report.Counts[j].Workspace
		}
		return report.Counts[i].Kind < report.Counts[j].Kind
	})
	return report
}

// Summary is the report as one log line per workspace and kind.
func (s *Shadow) Summary() []string {
	report := s.Report()
	if len(report.Counts) == 0 {
		return []string{"queue shadow: no claims observed yet"}
	}
	lines := make([]string, 0, len(report.Counts))
	for _, c := range report.Counts {
		lines = append(lines, fmt.Sprintf("queue shadow: %s %s: claims %d agree %d order %d not_claimable %d unknown %d mirrored %d mirror_errors %d",
			c.Workspace, c.Kind, c.Claims, c.Agree, c.Order, c.NotClaimable, c.Unknown, c.Mirrored, c.MirrorErrors))
	}
	return lines
}

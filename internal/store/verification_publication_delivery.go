package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/queue/logqueue"
)

// VerificationDeliveryStore owns PR delivery (component-verification-publication) independently of verification
// execution. Every callback holds the same backend lock used by intent creation.
// Forge failures are saved with a fixed public classification, never raw errors.
type VerificationDeliveryStore interface {
	RecordVerificationPullRequest(context.Context, core.Event) error
	TranslateVerificationPublication(context.Context, VerificationPublication) error
	RunVerificationDelivery(context.Context, queue.VerificationPublicationArgs, func(*core.VerificationDelivery, func(string, core.VerificationDelivery) error) error) error
	ReconcileVerificationDeliveries(context.Context) error
}

type VerificationPR struct {
	Repository string `json:"repository"`
	Number     int    `json:"number"`
	Head       string `json:"head_sha"`
}

func VerificationPRFromEvents(events []core.Event) (VerificationPR, bool) {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind != "pull_request.opened" {
			continue
		}
		var p VerificationPR
		if json.Unmarshal(events[i].Payload, &p) == nil && p.Number > 0 && p.Repository != "" && p.Head != "" {
			return p, true
		}
	}
	return VerificationPR{}, false
}

func VerificationDeliveryKey(repository string, number int) string {
	sum := sha256.Sum256([]byte(repository + "\x00" + strconv.Itoa(number)))
	return hex.EncodeToString(sum[:])
}

func VerificationDeliveryArgs(d core.VerificationDelivery) queue.VerificationPublicationArgs {
	return queue.VerificationPublicationArgs{WorkspaceID: d.WorkspaceID, Repository: d.Repository, PullRequestNumber: d.PullRequestNumber}
}

// PrepareVerificationDelivery is shared by evidence-first, PR-first and legacy
// translation. It never rewrites source rows or their receipt identities.
func PrepareVerificationDelivery(ws, task string, pr VerificationPR, rows []VerificationRow, prior []core.VerificationDelivery, sourceID string, now time.Time) ([]VerificationRow, []core.VerificationDelivery, error) {
	if ws == "" || task == "" || pr.Repository == "" || pr.Number <= 0 || pr.Head == "" {
		return nil, nil, ErrVerificationInvalid
	}
	contexts := map[string]VerificationContext{}
	var sources []VerificationPublication
	for _, r := range rows {
		if r.TaskID != task {
			continue
		}
		if r.Table == "verification_contexts" {
			contexts[r.ID] = verificationDecode[VerificationContext](r)
		}
		if r.Table == "verification_publications" {
			p := verificationDecode[VerificationPublication](r)
			if p.PullRequestNumber == pr.Number {
				sources = append(sources, p)
			}
		}
	}
	var added []VerificationRow
	if sourceID != "" {
		found := false
		for _, p := range sources {
			if p.ID == sourceID {
				found = true
			}
		}
		if !found {
			return nil, nil, ErrVerificationAccess
		}
	} else {
		var vc VerificationContext
		for _, r := range rows {
			if r.TaskID == task && r.Table == "verification_evidence" && r.State == "evidence" {
				c := contexts[r.ContextID]
				if c.CreatedAt.After(vc.CreatedAt) || c.CreatedAt.Equal(vc.CreatedAt) && c.ID > vc.ID {
					vc = c
				}
			}
		}
		if vc.ID != "" {
			summary := verificationDeliverySummary(ws, task, pr.Head, vc, rows)
			mutation := VerificationMutation{}
			c := VerificationCommand{Access: VerificationAccess{TaskID: task}, ContextID: vc.ID}
			if err := verificationPublicationMutation(c, VerificationPublication{PullRequestNumber: pr.Number, HeadSHA: pr.Head, BodyDigest: verificationHash([]byte(summary))}, rows, now, &mutation); err != nil {
				return nil, nil, err
			}
			added = mutation.Rows
			if mutation.Publication != nil {
				sources = append(sources, *mutation.Publication)
			}
		}
	}
	if len(sources) == 0 {
		return added, nil, nil
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].Generation < sources[j].Generation })
	desired := sources[len(sources)-1]
	current, _ := VerificationDeliveryLatest(prior)
	known := map[string]core.VerificationDelivery{}
	maxGeneration := 0
	for _, d := range prior {
		if d.WorkspaceID != ws || d.Repository != pr.Repository || d.PullRequestNumber != pr.Number || d.TaskID != task {
			return nil, nil, ErrVerificationAccess
		}
		known[d.SourcePublicationID] = d
		if d.Generation > maxGeneration {
			maxGeneration = d.Generation
		}
	}
	var updates []core.VerificationDelivery
	for _, source := range sources {
		if _, ok := known[source.ID]; ok {
			continue
		}
		vc, ok := contexts[source.ContextID]
		if !ok {
			return nil, nil, ErrVerificationAccess
		}
		maxGeneration++
		summary := verificationDeliverySummary(ws, task, pr.Head, vc, rows)
		d := core.VerificationDelivery{WorkspaceID: ws, Repository: pr.Repository, TaskID: task, ContextID: vc.ID, SourcePublicationID: source.ID, SourceGeneration: source.Generation, PullRequestNumber: pr.Number, Generation: maxGeneration, State: "superseded", TargetHead: pr.Head, TargetDigest: verificationHash([]byte(summary)), Summary: summary, CreatedAt: now, UpdatedAt: now}
		d.ID = verificationHash([]byte(ws + "\x00" + source.ID + "\x00" + VerificationDeliveryKey(pr.Repository, pr.Number)))
		d.IdempotencyKey = d.ID
		if source.ID == desired.ID && (current.ID == "" || source.Generation > current.SourceGeneration) {
			d.State = "pending"
			// A newer source supersedes every older delivery that is not
			// already superseded, whatever its last state: a published,
			// failed or retrying older source must not keep retrying or
			// stay current beside the new generation
			// (component-verification-publication).
			for _, prev := range prior {
				if prev.State == "superseded" || prev.SourceGeneration >= source.Generation {
					continue
				}
				nextState, err := core.TransitionVerificationDelivery(prev.State, "supersede")
				if err != nil {
					return nil, nil, err
				}
				prev.State = nextState
				prev.UpdatedAt = now
				updates = append(updates, prev)
			}
		}
		updates = append(updates, d)
	}
	return added, updates, nil
}

// Only identifiers, source hashes and enumerated outcomes enter the summary.
// No evidence payload, artifact URL, arbitrary explanation or provider error is
// copied. Links are rendered by the forge worker against its trusted public URL.
func verificationDeliverySummary(ws, task, head string, vc VerificationContext, rows []VerificationRow) string {
	lines := []string{"### Durable verification", "", "Workspace: `" + verificationPublicationScalar(ws) + "`", "Task: `" + verificationPublicationScalar(task) + "`", "PR head: `" + verificationPublicationScalar(head) + "`", "Context: `" + verificationPublicationScalar(vc.ID) + "`"}
	for i, r := range vc.Revisions {
		if i >= 20 {
			lines = append(lines, "Additional source revisions omitted.")
			break
		}
		lines = append(lines, "Source: `"+verificationPublicationScalar(r.Repository)+"` at `"+verificationPublicationScalar(r.SHA)+"`")
	}
	base := strings.TrimRight(os.Getenv(config.PublicURLEnv), "/")
	if u, err := url.Parse(base); err == nil && len(base) <= 512 && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" {
		root := base + "/v1/tasks/" + url.PathEscape(task) + "/verification"
		query := "?workspace_id=" + url.QueryEscape(ws)
		contextRoot := root + "/contexts/" + url.PathEscape(vc.ID)
		lines = append(lines, "[Contexts]("+root+query+") · [Attempts]("+contextRoot+"/attempts"+query+") · [Evidence references]("+contextRoot+"/evidence"+query+")")
	}
	var refs []string
	for _, row := range rows {
		if row.ContextID != vc.ID {
			continue
		}
		switch row.Table {
		case "verification_evidence":
			if row.State == "evidence" {
				refs = append(refs, "Evidence: `"+verificationPublicationScalar(row.ID)+"`")
			}
		case "verification_attempts", "verification_operations":
			state := row.State
			switch state {
			case "pending", "running", "succeeded", "failed", "blocked", "cancelled", "registered", "dispatching", "outcome_unknown", "applied", "not_applied", "completed":
			default:
				state = "unknown"
			}
			refs = append(refs, strings.TrimPrefix(row.Table, "verification_")+": `"+verificationPublicationScalar(row.ID)+"` — "+state)
		}
	}
	lines = append(lines, fmt.Sprintf("Records: %d", len(refs)))
	sort.Strings(refs)
	if len(refs) > 50 {
		refs = append(refs[:50], "Additional records omitted; inspect Conveyor for the complete history.")
	}
	return strings.Join(append(lines, refs...), "\n")
}
func verificationPublicationScalar(s string) string {
	if len(s) > 160 {
		return "[omitted]"
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-._/", r)) {
			return "[omitted]"
		}
	}
	return s
}

func AdvanceVerificationDelivery(old core.VerificationDelivery, command string, next core.VerificationDelivery, now time.Time) (core.VerificationDelivery, error) {
	state, err := core.TransitionVerificationDelivery(old.State, command)
	if err != nil {
		return old, err
	}
	if old.ID != next.ID || old.WorkspaceID != next.WorkspaceID || old.Repository != next.Repository || old.TaskID != next.TaskID || old.ContextID != next.ContextID || old.SourcePublicationID != next.SourcePublicationID || old.Generation != next.Generation || old.SourceGeneration != next.SourceGeneration || old.PullRequestNumber != next.PullRequestNumber || old.TargetHead != next.TargetHead || old.TargetDigest != next.TargetDigest || old.Summary != next.Summary || old.IdempotencyKey != next.IdempotencyKey || !old.CreatedAt.Equal(next.CreatedAt) {
		return old, ErrVerificationConflict
	}
	if command == "attempt" {
		next.Attempts = old.Attempts + 1
		next.CycleAttempts = old.CycleAttempts + 1
		next.LastAttemptAt = &now
		next.NextAttemptAt = nil
	} else if next.Attempts != old.Attempts || next.CycleAttempts != old.CycleAttempts {
		return old, ErrVerificationInvalid
	}
	if command == "retry" {
		next.CycleAttempts = 0
		next.NextAttemptAt = nil
	}
	if command == "publish" && (next.ObservedHead != old.TargetHead || next.ObservedDigest != old.TargetDigest) {
		return old, ErrVerificationState
	}
	if len(next.ObservedHead) > 160 || len(next.ObservedDigest) > 64 {
		return old, ErrVerificationInvalid
	}
	switch next.ErrorClass {
	case "":
		next.ErrorMessage = ""
	case "auth":
		next.ErrorMessage = "Workspace GitHub App access is unavailable."
	case "head_mismatch":
		next.ErrorMessage = "Pull request head differs from the publication target."
	case "readback_mismatch":
		next.ErrorMessage = "The managed verification region did not match on readback."
	case "forge":
		next.ErrorMessage = "GitHub publication could not be verified."
	default:
		return old, ErrVerificationInvalid
	}
	next.State = state
	next.UpdatedAt = now
	return next, nil
}

func JoinVerificationDelivery(item *VerificationReadItem, d core.VerificationDelivery) error {
	if d.SourcePublicationID != item.ID || d.ContextID != item.ContextID {
		return ErrVerificationAccess
	}
	var meta map[string]string
	if err := json.Unmarshal(item.Metadata, &meta); err != nil {
		return err
	}
	meta["delivery_state"] = d.State
	meta["current"] = strconv.FormatBool(d.State != "superseded")
	meta["target_head"] = d.TargetHead
	meta["target_digest"] = d.TargetDigest
	meta["source_generation"] = strconv.Itoa(d.SourceGeneration)
	meta["updated_at"] = d.UpdatedAt.UTC().Format(time.RFC3339Nano)
	if d.LastAttemptAt != nil {
		meta["last_attempt_at"] = d.LastAttemptAt.UTC().Format(time.RFC3339Nano)
	}
	meta["generation"] = strconv.Itoa(d.Generation)
	meta["attempts"] = strconv.Itoa(d.Attempts)
	meta["observed_head"] = d.ObservedHead
	meta["observed_digest"] = d.ObservedDigest
	meta["error_class"] = d.ErrorClass
	meta["error_message"] = d.ErrorMessage
	if d.NextAttemptAt != nil {
		meta["next_attempt_at"] = d.NextAttemptAt.UTC().Format(time.RFC3339Nano)
	}
	for k, v := range meta {
		bounded, cut := verificationReadBound(v)
		meta[k] = bounded
		if cut {
			meta["truncated"] = "true"
		}
	}
	item.State = d.State
	item.Metadata = verificationJSON(meta)
	return nil
}

func VerificationDeliveryLatest(ds []core.VerificationDelivery) (core.VerificationDelivery, bool) {
	var latest core.VerificationDelivery
	for _, d := range ds {
		if d.State != "superseded" && d.Generation > latest.Generation {
			latest = d
		}
	}
	return latest, latest.ID != ""
}

func VerificationDeliveryMaxGeneration(ds []core.VerificationDelivery) int {
	max := 0
	for _, d := range ds {
		if d.Generation > max {
			max = d.Generation
		}
	}
	return max
}

// ConvergeVerificationDelivery moves one due row given the newest generation
// for its pull-request key. Older pending, retrying and failed rows supersede
// and drop any scheduled retry, so they leave the due window for good
// (component-verification-publication invariants 5, 19 and 27); a terminal
// head mismatch never schedules another automatic retry.
func ConvergeVerificationDelivery(d core.VerificationDelivery, latestGeneration int, now time.Time) (core.VerificationDelivery, bool, error) {
	if d.Generation < latestGeneration {
		switch d.State {
		case "pending", "retrying", "failed":
			state, err := core.TransitionVerificationDelivery(d.State, "supersede")
			if err != nil {
				return d, false, err
			}
			d.State = state
			d.NextAttemptAt = nil
			d.UpdatedAt = now
			return d, false, nil
		}
		return d, false, nil
	}
	switch d.State {
	case "pending", "retrying":
		return d, true, nil
	case "failed":
		if d.ErrorClass == "head_mismatch" {
			if d.NextAttemptAt != nil {
				d.NextAttemptAt = nil
				d.UpdatedAt = now
			}
			return d, false, nil
		}
		if d.NextAttemptAt == nil || d.NextAttemptAt.After(now) {
			return d, false, nil
		}
		updated, err := AdvanceVerificationDelivery(d, "retry", d, now)
		if err != nil {
			return d, false, err
		}
		return updated, true, nil
	}
	return d, false, nil
}

// VerificationDeliveryDiscardStops reports whether a delivery job discarded
// after its final attempt stopped the row the reconcile pass read under its
// pull-request lock. Only a row read as pending or retrying qualifies, and
// only when the discard came after the row's last change: a row the last
// attempt saved as failed keeps its scheduled retry, and a discard older than
// the row's current state belongs to an earlier cycle (component-verification-
// publication invariant 26; req-bounded-automatic-recovery AC-4.1). Both times
// compare at microsecond precision, the precision both SQL logs keep, so a
// discard recorded before the change never reads as after it.
func VerificationDeliveryDiscardStops(read core.VerificationDelivery, job logqueue.Job, discarded bool) bool {
	if !discarded || job.DiscardedAt.IsZero() || read.State != "pending" && read.State != "retrying" {
		return false
	}
	return job.DiscardedAt.Truncate(time.Microsecond).After(read.UpdatedAt.Truncate(time.Microsecond))
}

// ReconcileVerificationDeliveryRow converges one delivery row that a reconcile
// pass read again under its pull-request lock, given the newest generation of
// that key. put saves a changed row. Every log call uses ctx, so a SQL backend
// passes its transaction-bound context and the row write, the enqueue and the
// queue reads commit together. repaired reports a pending or retrying row that
// had no unfinished job and got one; discarded reports a row a discarded job
// stopped. Both are reported after the commit
// (component-verification-publication invariants 19, 26 and 27).
func ReconcileVerificationDeliveryRow(ctx context.Context, log eventlog.Store, read core.VerificationDelivery, latestGeneration int, now time.Time, put func(core.VerificationDelivery) error) (repaired, discarded bool, err error) {
	next, enqueue, err := ConvergeVerificationDelivery(read, latestGeneration, now)
	if err != nil {
		return false, false, err
	}
	a := VerificationDeliveryArgs(next)
	stream := logqueue.StreamFor(a.Kind(), a.UniqueKey())
	if enqueue && read.State != "failed" {
		job, ended, loadErr := logqueue.LoadDiscarded(ctx, log, read.WorkspaceID, stream)
		if loadErr != nil {
			return false, false, loadErr
		}
		if VerificationDeliveryDiscardStops(read, job, ended) {
			next, err = FailDiscardedVerificationDelivery(next, now)
			if err != nil {
				return false, false, err
			}
			enqueue = false
			discarded = true
		}
	}
	if VerificationDeliveryNeedsWrite(read, next) {
		if err = put(next); err != nil {
			return false, false, err
		}
	}
	if !enqueue {
		return false, discarded, nil
	}
	// Enqueue reads the stream again and adds no job while one is unfinished,
	// so a row whose job became active after the due read gets no second job.
	inserted, err := logqueue.Enqueue(ctx, log, read.WorkspaceID, a.Kind(), a.UniqueKey(), a, 5, now)
	if err != nil {
		return false, false, err
	}
	return inserted && read.State != "failed", discarded, nil
}

// ErrVerificationDeliveryBusy reports a reconcile pass that skipped at least
// one pull-request key because another transaction held its lock, such as a
// delivery handler waiting on GitHub. The pass finished every other key. The
// caller retries the pass: the order clock job returns the error so the queue
// runs it again, and the start-up check retries a failed pass in the
// background (component-verification-publication invariant 12).
var ErrVerificationDeliveryBusy = errors.New("verification delivery: a pull request's delivery lock is held; the pass will run again")

type verificationDeliveryClockKey struct{}

// WithVerificationDeliveryClock binds the clock a reconcile pass reads to
// decide which failed rows are due. Tests use it to move past a scheduled
// next_attempt_at; production leaves it unset and the pass reads time.Now.
func WithVerificationDeliveryClock(ctx context.Context, now func() time.Time) context.Context {
	return context.WithValue(ctx, verificationDeliveryClockKey{}, now)
}

// VerificationDeliveryNow is the reconcile pass's current time in UTC.
func VerificationDeliveryNow(ctx context.Context) time.Time {
	if now, ok := ctx.Value(verificationDeliveryClockKey{}).(func() time.Time); ok && now != nil {
		return now().UTC()
	}
	return time.Now().UTC()
}

// VerificationDeliveryStreamKey is the pull-request key of a delivery job
// stream, or false for another stream. The SQL backends read the unfinished
// delivery streams from the job index and leave those keys' newest rows out
// of the due window, because the unfinished job serves them.
func VerificationDeliveryStreamKey(stream string) (string, bool) {
	prefix := string(logqueue.StreamFor(queue.VerificationPublicationArgs{}.Kind(), ""))
	rest, ok := strings.CutPrefix(stream, prefix)
	if !ok {
		return "", false
	}
	cut := strings.LastIndex(rest, "#")
	if cut <= 0 {
		return "", false
	}
	number, err := strconv.Atoi(rest[cut+1:])
	if err != nil || number <= 0 {
		return "", false
	}
	return VerificationDeliveryKey(rest[:cut], number), true
}

func VerificationDeliveryNeedsWrite(old, next core.VerificationDelivery) bool {
	if old.State != next.State {
		return true
	}
	if old.ErrorClass != next.ErrorClass || old.ErrorMessage != next.ErrorMessage {
		return true
	}
	switch {
	case old.NextAttemptAt == nil && next.NextAttemptAt == nil:
		return false
	case old.NextAttemptAt == nil || next.NextAttemptAt == nil:
		return true
	default:
		return !old.NextAttemptAt.Equal(*next.NextAttemptAt)
	}
}

// FailDiscardedVerificationDelivery records a terminal forge failure for a
// delivery whose job was discarded, without copying the discard reason.
func FailDiscardedVerificationDelivery(d core.VerificationDelivery, now time.Time) (core.VerificationDelivery, error) {
	if d.State != "pending" && d.State != "retrying" {
		return d, nil
	}
	current := d
	if current.State == "pending" {
		stepped, err := AdvanceVerificationDelivery(current, "attempt", current, now)
		if err != nil {
			return d, err
		}
		current = stepped
	}
	next := current
	next.ErrorClass = "forge"
	next.NextAttemptAt = nil
	return AdvanceVerificationDelivery(current, "fail", next, now)
}

func VerificationDeliveryIdentityError() error {
	return fmt.Errorf("verification publication requires a matching trusted workspace")
}

// AttachVerificationDeliverySnapshot joins only transient read values. It never
// changes the immutable ledger record supplied by a backend.
func AttachVerificationDeliverySnapshot(snapshot *VerificationSnapshot, ds []core.VerificationDelivery) {
	for i := range snapshot.Publications {
		p := &snapshot.Publications[i]
		for _, d := range ds {
			if d.SourcePublicationID == p.ID && d.TaskID == p.TaskID && d.ContextID == p.ContextID {
				copy := d
				copy.Summary = ""
				p.Delivery = &copy
				break
			}
		}
	}
}

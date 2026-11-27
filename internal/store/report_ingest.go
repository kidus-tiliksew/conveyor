package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/redact"
)

// Report bounds (component-feedback-reports invariants 3 and 8;
// req-production-feedback AC-4.1).
const (
	MaxFeedbackReportEntries  = 50
	MaxFeedbackReportCount    = 1_000_000_000
	MaxFeedbackReportLine     = 1_000_000
	MaxOpenFeedbackReports    = 200
	feedbackReportKeyMax      = 200
	feedbackReportSummaryMax  = 1000
	feedbackReportServiceMax  = 100
	feedbackReportVersionMax  = 200
	feedbackReportFileMax     = 500
	feedbackReportRepoMaxSize = 255
)

// The kinds of a report entry.
const (
	FeedbackReportError   = "error"
	FeedbackReportAlert   = "alert"
	FeedbackReportMessage = "message"
)

// The states of a report.
const (
	FeedbackReportOpen   = "open"
	FeedbackReportClosed = "closed"
)

// FeedbackSourceReport is the source kind of a report, and of a reopening,
// that a watcher's report request made. The label is the key's label
// (component-feedback-reports invariant 9; req-environments AC-2.4).
const FeedbackSourceReport = "report"

// The seven workspace event kinds of component-feedback-reports. Each names
// no task, carries the report's ID and the environment's ID, and holds no
// text of the report (invariant 20).
const (
	FeedbackReportOpenedEvent        = "feedback_report.opened"
	FeedbackReportReopenedEvent      = "feedback_report.reopened"
	FeedbackReportLinkedEvent        = "feedback_report.linked"
	FeedbackReportLinkConfirmedEvent = "feedback_report.link_confirmed"
	FeedbackReportLinkRemovedEvent   = "feedback_report.link_removed"
	FeedbackReportClosedEvent        = "feedback_report.closed"
	FeedbackReportFixTaskSetEvent    = "feedback_report.fix_task_set"
)

// FeedbackReportEventKinds lists the seven kinds in the order the design
// names them.
var FeedbackReportEventKinds = []string{
	FeedbackReportOpenedEvent, FeedbackReportReopenedEvent, FeedbackReportLinkedEvent,
	FeedbackReportLinkConfirmedEvent, FeedbackReportLinkRemovedEvent, FeedbackReportClosedEvent,
	FeedbackReportFixTaskSetEvent,
}

// ErrOpenReportLimit refuses a whole request that would store a new report
// beyond the open-report limit of its environment
// (component-feedback-reports invariant 8).
var ErrOpenReportLimit = errors.New("open report limit")

// FeedbackReportEntry is one entry of a report request, as decoded. An empty
// text was not stated. Count 0 and a nil time were not stated. Line is set
// exactly when File is. After NormalizeFeedbackReports, each time holds the
// full precision stated, so the order check and the reopen check compare the
// exact times. A backend stores them at microsecond precision.
type FeedbackReportEntry struct {
	Kind       string
	Key        string
	Summary    string
	Service    string
	Repository string
	Commit     string
	Version    string
	Count      int64
	FirstSeen  *time.Time
	LastSeen   *time.Time
	File       string
	Line       int
}

// FeedbackReportBatch is one report request. EnvironmentID and KeyID come
// from the verified key and ArrivedAt is the server's time of arrival, so
// nothing in the body chooses a scope or a time (component-feedback-reports
// invariants 2 and 4).
type FeedbackReportBatch struct {
	EnvironmentID string
	KeyID         string
	ArrivedAt     time.Time
	Entries       []FeedbackReportEntry
}

// FeedbackReportVersion is the repository, commit, and version an entry
// states. The zero value states none.
type FeedbackReportVersion struct {
	Repository string `json:"repository"`
	Commit     string `json:"commit"`
	Version    string `json:"version"`
}

// FeedbackReportOrigin is what made a report or its reopening: the source,
// the key's label, and the actor.
type FeedbackReportOrigin struct {
	Source string `json:"source"`
	Label  string `json:"label"`
	Actor  string `json:"actor"`
}

// FeedbackReport is the one record of one problem of an environment
// (component-feedback-reports invariant 6). FirstSeen is the earliest
// first-seen time of every entry, and FirstEntrySeen is the first entry's own
// first-seen time, which no later entry changes, so linking reads the first
// entry alone (invariants 11 and 13). Kind, Summary, Service, File, Line, and
// First are the first entry's. Summary is redacted, and SummaryRedactions
// counts its removals by pattern class (invariant 5). ClosedAt, ClosedBy,
// and CloseReason are set while the report is closed. Reopened and
// ReopenOrigin record the latest reopening by a report (invariant 7).
// LinkingEndedAt and the fix task belong to the sibling linking and act
// tasks, and ingest never sets them.
type FeedbackReport struct {
	ID                string                `json:"id"`
	EnvironmentID     string                `json:"environment_id"`
	Kind              string                `json:"kind"`
	Key               string                `json:"key"`
	Summary           string                `json:"summary"`
	SummaryRedactions core.RedactionStats   `json:"summary_redactions"`
	Service           string                `json:"service"`
	Count             int64                 `json:"count"`
	FirstSeen         time.Time             `json:"first_seen"`
	LastSeen          time.Time             `json:"last_seen"`
	FirstEntrySeen    time.Time             `json:"first_entry_seen"`
	File              string                `json:"file"`
	Line              int                   `json:"line"`
	First             FeedbackReportVersion `json:"first"`
	Latest            FeedbackReportVersion `json:"latest"`
	State             string                `json:"state"`
	InInbox           bool                  `json:"in_inbox"`
	Origin            FeedbackReportOrigin  `json:"origin"`
	CreatedAt         time.Time             `json:"created_at"`
	ClosedAt          *time.Time            `json:"closed_at"`
	ClosedBy          string                `json:"closed_by"`
	CloseReason       string                `json:"close_reason"`
	ReopenedAt        *time.Time            `json:"reopened_at"`
	ReopenOrigin      *FeedbackReportOrigin `json:"reopen_origin"`
	LinkingEndedAt    *time.Time            `json:"linking_ended_at"`
	FixTaskID         string                `json:"fix_task_id"`
}

// ReportStore keeps feedback reports (component-feedback-reports;
// component-persistence). It changes no task, work order, gate, or next
// action (invariant 1; DEC-149(1); req-production-feedback REQ-11).
type ReportStore interface {
	// CheckFeedbackReportRepositories refuses a request that states a
	// repository outside the current workspace configuration, with the
	// FeedbackFieldError that RecordFeedbackReports would answer. It writes
	// nothing. RecordFeedbackReports checks again, because the configuration
	// can change in between.
	CheckFeedbackReportRepositories(ctx context.Context, batch FeedbackReportBatch) error
	// RecordFeedbackReports records one request in one transaction, as
	// PlanFeedbackReports plans it, with the key's last use at most once a
	// minute and one event for each new and each reopened report. The
	// requests of one environment are serialized with its signals. A key that
	// is revoked, or whose environment is archived, by the time of the write
	// answers ErrInvalidEnvironmentKey, a value the rules refuse answers
	// FeedbackFieldError, and a request over the open-report limit answers
	// ErrOpenReportLimit. Each records nothing.
	RecordFeedbackReports(ctx context.Context, batch FeedbackReportBatch) error
	// ListEnvironmentFeedbackReports returns every report of one environment,
	// open and closed, ordered by ID.
	ListEnvironmentFeedbackReports(ctx context.Context, environmentID string) ([]FeedbackReport, error)
}

// FeedbackReportField is the path that a refusal of an entry's member names.
func FeedbackReportField(entry int, member string) string {
	return fmt.Sprintf("reports[%d].%s", entry, member)
}

// FeedbackReportKeyDigest identifies a problem key within its environment.
// Every backend keys reports by it, so no database collation can make two
// keys equal.
func FeedbackReportKeyDigest(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// NewFeedbackReportID mints rpt-<yymmdd>-<6 hex> (component-feedback-reports
// invariant 6).
func NewFeedbackReportID(now time.Time) (string, error) {
	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate report ID: %w", err)
	}
	return "rpt-" + now.UTC().Format("060102") + "-" + hex.EncodeToString(b), nil
}

// checkFeedbackSummary bounds a summary. It is redacted, not refused, when it
// holds a secret (invariant 5).
func checkFeedbackSummary(field, value string) error {
	if !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
		return feedbackFieldError(field, "must be UTF-8 text without a NUL character")
	}
	if count := utf8.RuneCountInString(value); count < 1 || count > feedbackReportSummaryMax {
		return feedbackFieldError(field, "must be 1 to %d characters", feedbackReportSummaryMax)
	}
	return nil
}

// feedbackReportTime is a stated time or the arrival, and the arrival when
// the stated time is later (invariant 4). It keeps the stated precision, so
// two stated times within one microsecond keep their order.
func feedbackReportTime(stated *time.Time, arrived time.Time) time.Time {
	if stated == nil || stated.After(arrived) {
		return arrived
	}
	return stated.UTC()
}

// NormalizeFeedbackReports applies the rules of component-feedback-reports
// invariants 3 to 5 that need no stored state, and redacts no text. A
// request that breaks one rule is refused whole. It sets each count and
// time. CheckFeedbackReportRepositories is the rule that needs the
// workspace.
func NormalizeFeedbackReports(batch FeedbackReportBatch) (FeedbackReportBatch, error) {
	if batch.ArrivedAt.IsZero() {
		return FeedbackReportBatch{}, fmt.Errorf("a feedback report request needs its arrival time")
	}
	batch.ArrivedAt = EnvironmentTime(batch.ArrivedAt)
	if len(batch.Entries) < 1 || len(batch.Entries) > MaxFeedbackReportEntries {
		return FeedbackReportBatch{}, feedbackFieldError("reports", "must hold 1 to %d entries", MaxFeedbackReportEntries)
	}
	redactor := redact.New(nil)
	entries := make([]FeedbackReportEntry, len(batch.Entries))
	seen := make(map[string]bool, len(batch.Entries))
	for i, entry := range batch.Entries {
		switch entry.Kind {
		case FeedbackReportError, FeedbackReportAlert, FeedbackReportMessage:
		default:
			return FeedbackReportBatch{}, feedbackFieldError(FeedbackReportField(i, "kind"), "must be error, alert, or message")
		}
		if err := checkFeedbackText(redactor, FeedbackReportField(i, "key"), entry.Key, feedbackReportKeyMax); err != nil {
			return FeedbackReportBatch{}, err
		}
		if seen[entry.Key] {
			return FeedbackReportBatch{}, feedbackFieldError(FeedbackReportField(i, "key"), "must differ from the key of every other report of the request")
		}
		seen[entry.Key] = true
		if err := checkFeedbackSummary(FeedbackReportField(i, "summary"), entry.Summary); err != nil {
			return FeedbackReportBatch{}, err
		}
		if entry.Service != "" {
			if err := checkFeedbackText(redactor, FeedbackReportField(i, "service"), entry.Service, feedbackReportServiceMax); err != nil {
				return FeedbackReportBatch{}, err
			}
		}
		if entry.Repository != "" && (!utf8.ValidString(entry.Repository) || len(entry.Repository) > feedbackReportRepoMaxSize) {
			return FeedbackReportBatch{}, feedbackFieldError(FeedbackReportField(i, "repository"), "must name a repository of the workspace")
		}
		if entry.Commit != "" && !feedbackCommitValid(entry.Commit) {
			return FeedbackReportBatch{}, feedbackFieldError(FeedbackReportField(i, "commit"), "must be %d to %d hexadecimal characters", feedbackCommitMinCharacters, feedbackCommitMaxCharacters)
		}
		if entry.Version != "" {
			if err := checkFeedbackText(redactor, FeedbackReportField(i, "version"), entry.Version, feedbackReportVersionMax); err != nil {
				return FeedbackReportBatch{}, err
			}
		}
		switch {
		case entry.Count == 0:
			entry.Count = 1
		case entry.Count < 1 || entry.Count > MaxFeedbackReportCount:
			return FeedbackReportBatch{}, feedbackFieldError(FeedbackReportField(i, "count"), "must be a whole number from 1 to %d", MaxFeedbackReportCount)
		}
		if (entry.File == "") != (entry.Line == 0) {
			return FeedbackReportBatch{}, feedbackFieldError(FeedbackReportField(i, "file"), "must be stated together with line, or neither")
		}
		if entry.File != "" {
			if err := checkFeedbackText(redactor, FeedbackReportField(i, "file"), entry.File, feedbackReportFileMax); err != nil {
				return FeedbackReportBatch{}, err
			}
			if entry.Line < 1 || entry.Line > MaxFeedbackReportLine {
				return FeedbackReportBatch{}, feedbackFieldError(FeedbackReportField(i, "line"), "must be a whole number from 1 to %d", MaxFeedbackReportLine)
			}
		}
		first, last := feedbackReportTime(entry.FirstSeen, batch.ArrivedAt), feedbackReportTime(entry.LastSeen, batch.ArrivedAt)
		if first.After(last) {
			return FeedbackReportBatch{}, feedbackFieldError(FeedbackReportField(i, "first_seen"), "must not be later than last_seen")
		}
		entry.FirstSeen, entry.LastSeen = &first, &last
		entries[i] = entry
	}
	batch.Entries = entries
	return batch, nil
}

// CheckFeedbackReportRepositories refuses a stated repository outside the
// workspace configuration, matched exactly (invariant 3).
func CheckFeedbackReportRepositories(batch FeedbackReportBatch, repositories map[string]bool) error {
	for i, entry := range batch.Entries {
		if entry.Repository != "" && !repositories[entry.Repository] {
			return feedbackFieldError(FeedbackReportField(i, "repository"), "must name a repository of the workspace")
		}
	}
	return nil
}

// PreparedFeedbackReports is a normalized request with each summary
// redacted. Redactions holds the removals of each entry's summary, by entry.
type PreparedFeedbackReports struct {
	FeedbackReportBatch
	Redactions []core.RedactionStats
}

// PrepareFeedbackReports normalizes a request and redacts each summary with
// the current stored secrets. A backend calls it before it takes a lock, so
// the secret read never waits on the write (invariant 5;
// req-security-boundaries REQ-3 and AC-3.1).
func PrepareFeedbackReports(ctx context.Context, source redact.SecretSource, batch FeedbackReportBatch) (PreparedFeedbackReports, error) {
	batch, err := NormalizeFeedbackReports(batch)
	if err != nil {
		return PreparedFeedbackReports{}, err
	}
	redactor, err := redact.WithSecrets(ctx, source, nil)
	if err != nil {
		return PreparedFeedbackReports{}, err
	}
	prepared := PreparedFeedbackReports{FeedbackReportBatch: batch, Redactions: make([]core.RedactionStats, len(batch.Entries))}
	for i := range prepared.Entries {
		prepared.Entries[i].Summary, prepared.Redactions[i] = redactor.Redact(prepared.Entries[i].Summary)
	}
	return prepared, nil
}

// FeedbackReportOriginFor is the origin of a report, or a reopening, that a
// request under the key's label made: the source report and the system as
// the actor (invariant 9).
func FeedbackReportOriginFor(label string) FeedbackReportOrigin {
	return FeedbackReportOrigin{Source: FeedbackSourceReport, Label: label, Actor: SystemActor().ID}
}

// FeedbackReportChange is what one entry changes. Exactly one of New,
// Reopened, and Updated is true, and Report is the record to store.
type FeedbackReportChange struct {
	Report   FeedbackReport
	New      bool
	Reopened bool
	Updated  bool
}

// FeedbackReportPlan is what one request changes, in entry order.
type FeedbackReportPlan struct {
	Changes []FeedbackReportChange
}

// NewReports is the number of reports the plan stores for the first time.
func (p FeedbackReportPlan) NewReports() int {
	count := 0
	for _, change := range p.Changes {
		if change.New {
			count++
		}
	}
	return count
}

// Events are the events the plan appends: one opened event for each new
// report and one reopened event for each reopening, with the report's ID
// and the environment's ID and no text of the report. An entry that only
// updates a report appends none (invariant 20).
func (p FeedbackReportPlan) Events(at time.Time) []core.Event {
	events := []core.Event{}
	for _, change := range p.Changes {
		kind := ""
		switch {
		case change.New:
			kind = FeedbackReportOpenedEvent
		case change.Reopened:
			kind = FeedbackReportReopenedEvent
		default:
			continue
		}
		system := SystemActor()
		events = append(events, core.Event{Kind: kind, ActorID: system.ID, ActorRole: system.Role, At: at, Payload: FeedbackReportEventPayload(change.Report)})
	}
	return events
}

// FeedbackReportEventPayload is the payload of an opened or reopened event.
func FeedbackReportEventPayload(report FeedbackReport) json.RawMessage {
	return core.JSONPayload(map[string]string{"report_id": report.ID, "environment_id": report.EnvironmentID})
}

// addFeedbackCount adds an entry's count to a stored count. The sum is kept
// in 64 bits and stops at the largest value rather than wrap.
func addFeedbackCount(stored, added int64) int64 {
	if added > 0 && stored > math.MaxInt64-added {
		return math.MaxInt64
	}
	return stored + added
}

// PlanFeedbackReports compares a prepared request with the stored reports
// of its keys, given by key, and with the number of open reports of the
// environment. Every backend records exactly this result, under the lock
// that serializes the environment's writes.
//
//   - The first entry under a key stores an open report with a new ID, the
//     Inbox flag set, and linking not ended (invariants 6 and 19).
//   - A later entry adds its count, keeps the earlier first-seen and the later
//     last-seen time, and replaces latest with what it states, or with none.
//     It changes nothing else (invariant 6; req-production-feedback AC-4.3).
//   - A later entry whose last-seen time is later than the closing time of a
//     closed report reopens it with the system as its actor and sets the
//     Inbox flag. Any other entry leaves a closed report closed (invariant
//     7; AC-4.4).
//   - A request that stores at least one new report while the environment
//     holds open reports that, with the new ones, pass the limit is refused
//     whole. A reopening does not count against the limit and is never
//     refused by it (invariant 8).
func PlanFeedbackReports(prepared PreparedFeedbackReports, label string, stored map[string]FeedbackReport, open int) (FeedbackReportPlan, error) {
	origin := FeedbackReportOriginFor(label)
	arrived := prepared.ArrivedAt
	plan := FeedbackReportPlan{Changes: make([]FeedbackReportChange, 0, len(prepared.Entries))}
	for i, entry := range prepared.Entries {
		stated := FeedbackReportVersion{Repository: entry.Repository, Commit: entry.Commit, Version: entry.Version}
		report, exists := stored[entry.Key]
		if !exists {
			id, err := NewFeedbackReportID(arrived)
			if err != nil {
				return FeedbackReportPlan{}, err
			}
			plan.Changes = append(plan.Changes, FeedbackReportChange{New: true, Report: FeedbackReport{
				ID: id, EnvironmentID: prepared.EnvironmentID, Kind: entry.Kind, Key: entry.Key,
				Summary: entry.Summary, SummaryRedactions: prepared.Redactions[i], Service: entry.Service,
				Count: entry.Count, FirstSeen: EnvironmentTime(*entry.FirstSeen), LastSeen: EnvironmentTime(*entry.LastSeen), FirstEntrySeen: EnvironmentTime(*entry.FirstSeen),
				File: entry.File, Line: entry.Line, First: stated, Latest: stated,
				State: FeedbackReportOpen, InInbox: true, Origin: origin, CreatedAt: arrived,
			}})
			continue
		}
		change := FeedbackReportChange{Updated: true}
		report.Count = addFeedbackCount(report.Count, entry.Count)
		// Stored times are kept at microsecond precision.
		if first := EnvironmentTime(*entry.FirstSeen); first.Before(report.FirstSeen) {
			report.FirstSeen = first
		}
		if last := EnvironmentTime(*entry.LastSeen); last.After(report.LastSeen) {
			report.LastSeen = last
		}
		report.Latest = stated
		// The entry's exact last-seen time is compared with the closing
		// time, so a time strictly after the closing reopens the report even
		// within the same microsecond (invariant 7; AC-4.4).
		if report.State == FeedbackReportClosed && report.ClosedAt != nil && entry.LastSeen.After(*report.ClosedAt) {
			reopenedAt, reopenOrigin := arrived, origin
			report.State, report.InInbox = FeedbackReportOpen, true
			report.ClosedAt, report.ClosedBy, report.CloseReason = nil, "", ""
			report.ReopenedAt, report.ReopenOrigin = &reopenedAt, &reopenOrigin
			change = FeedbackReportChange{Reopened: true}
		}
		change.Report = report
		plan.Changes = append(plan.Changes, change)
	}
	if added := plan.NewReports(); added > 0 && open+added > MaxOpenFeedbackReports {
		return FeedbackReportPlan{}, ErrOpenReportLimit
	}
	return plan, nil
}

// FeedbackReportRedactionsJSON renders the removals for a durable backend's
// row, and ApplyFeedbackReportRedactions reads them back.
func FeedbackReportRedactionsJSON(stats core.RedactionStats) string {
	return string(core.JSONPayload(stats))
}

func ApplyFeedbackReportRedactions(report *FeedbackReport, raw []byte) error {
	if err := json.Unmarshal(raw, &report.SummaryRedactions); err != nil {
		return fmt.Errorf("decode report redactions: %w", err)
	}
	return nil
}

// FeedbackReportKeys returns the distinct problem keys of a prepared request.
func FeedbackReportKeys(prepared PreparedFeedbackReports) []string {
	keys := make([]string, len(prepared.Entries))
	for i, entry := range prepared.Entries {
		keys[i] = entry.Key
	}
	return keys
}

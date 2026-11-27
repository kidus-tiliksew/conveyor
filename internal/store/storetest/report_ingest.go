package storetest

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/changes"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/redact"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// runFeedbackReports holds every backend to component-feedback-reports
// invariants 1, 3 to 9, and 20 (feature-production-feedback-listening FR-1;
// req-production-feedback AC-4.1, AC-4.3, AC-4.4, and AC-11.1;
// req-environments AC-2.4; req-security-boundaries REQ-3 and AC-3.1). Each
// case starts from a fresh workspace.
func runFeedbackReports(t *testing.T, factory Factory) {
	for _, c := range []struct {
		name string
		run  func(*testing.T, Fixture)
	}{
		{"FirstEntry", runFeedbackReportFirstEntry},
		{"LaterEntry", runFeedbackReportLaterEntry},
		{"LatestReplacedByNone", runFeedbackReportLatestCleared},
		{"Reopen", runFeedbackReportReopen},
		{"TimesWithinOneMicrosecond", runFeedbackReportSubMicrosecond},
		{"ClosedStaysClosed", runFeedbackReportStaysClosed},
		{"OpenReportLimit", runFeedbackReportLimit},
		{"ConcurrentLastSlot", runFeedbackReportLastSlot},
		{"TwoEntriesOneRequest", runFeedbackReportTwoEntries},
		{"RefusalsRecordNothing", runFeedbackReportRefusals},
		{"Isolation", runFeedbackReportIsolation},
		{"KeyValidityAtTheWrite", runFeedbackReportKeyValidity},
		{"KeyLastUse", runFeedbackReportLastUse},
		{"EventsAndNotices", runFeedbackReportEvents},
		{"ObservesOnly", runFeedbackReportObservesOnly},
	} {
		t.Run(c.name, func(t *testing.T) {
			x := factory.fresh(t, nil)
			if x.CloseFeedbackReport == nil || x.WorkspaceEvents == nil || x.AgeEnvironmentKey == nil {
				t.Fatal("conformance fixture must provide CloseFeedbackReport, WorkspaceEvents, and AgeEnvironmentKey for the report cases")
			}
			c.run(t, x)
		})
	}
}

var feedbackReportIDPattern = regexp.MustCompile(`^rpt-[0-9]{6}-[0-9a-f]{6}$`)

func feedbackReportTimePtr(offset time.Duration) *time.Time {
	at := feedbackTime(offset)
	return &at
}

// reportEntry is an entry with a kind, a key, and a summary.
func reportEntry(key string) store.FeedbackReportEntry {
	return store.FeedbackReportEntry{Kind: store.FeedbackReportError, Key: key, Summary: "Request failed for " + key}
}

func (e feedbackEnvironment) reportBatch(arrival time.Duration, entries ...store.FeedbackReportEntry) store.FeedbackReportBatch {
	return store.FeedbackReportBatch{EnvironmentID: e.id, KeyID: e.key.ID, ArrivedAt: feedbackTime(arrival), Entries: entries}
}

func (e feedbackEnvironment) sendReports(t *testing.T, arrival time.Duration, entries ...store.FeedbackReportEntry) {
	t.Helper()
	requireOK(t, e.x.Backend.RecordFeedbackReports(e.x.Context, e.reportBatch(arrival, entries...)))
}

func (e feedbackEnvironment) reports(t *testing.T) []store.FeedbackReport {
	t.Helper()
	reports, err := e.x.Backend.ListEnvironmentFeedbackReports(e.x.Context, e.id)
	requireOK(t, err)
	return reports
}

// reportByKey returns the one stored report of a problem key.
func (e feedbackEnvironment) reportByKey(t *testing.T, key string) store.FeedbackReport {
	t.Helper()
	var found []store.FeedbackReport
	for _, report := range e.reports(t) {
		if report.Key == key {
			found = append(found, report)
		}
	}
	if len(found) != 1 {
		t.Fatalf("reports of key %q=%+v, want one", key, found)
	}
	return found[0]
}

// openReports counts the open reports of the environment.
func (e feedbackEnvironment) openReports(t *testing.T) int {
	t.Helper()
	open := 0
	for _, report := range e.reports(t) {
		if report.State == store.FeedbackReportOpen {
			open++
		}
	}
	return open
}

// reportState renders everything a refused request must leave unchanged:
// the reports, the report events, and the environment state with each
// key's last use.
func (e feedbackEnvironment) reportState(t *testing.T) string {
	t.Helper()
	events, err := e.x.WorkspaceEvents(e.x.Context, "feedback_report.")
	requireOK(t, err)
	raw, err := json.Marshal([]any{e.reports(t), events, environmentState(t, e.x)})
	requireOK(t, err)
	return string(raw)
}

// fillReports stores count new reports in requests of the largest size.
func (e feedbackEnvironment) fillReports(t *testing.T, arrival time.Duration, prefix string, count int) {
	t.Helper()
	for start := 0; start < count; start += store.MaxFeedbackReportEntries {
		var entries []store.FeedbackReportEntry
		for i := start; i < min(count, start+store.MaxFeedbackReportEntries); i++ {
			entries = append(entries, reportEntry(fmt.Sprintf("%s-%d", prefix, i)))
		}
		e.sendReports(t, arrival, entries...)
	}
}

func reportEvents(t *testing.T, x Fixture) []core.Event {
	t.Helper()
	events, err := x.WorkspaceEvents(x.Context, "feedback_report.")
	requireOK(t, err)
	return events
}

// requireReportEvent checks one event's kind, actor, and exact payload: the
// report's ID and the environment's ID and nothing else.
func requireReportEvent(t *testing.T, event core.Event, kind string, report store.FeedbackReport) {
	t.Helper()
	if event.Kind != kind || event.TaskID != "" || event.ActorID != store.SystemActor().ID || event.ActorRole != core.ActorSystem {
		t.Fatalf("event=%+v, want a task-less %s by the system", event, kind)
	}
	var payload map[string]any
	requireOK(t, json.Unmarshal(event.Payload, &payload))
	want := map[string]any{"report_id": report.ID, "environment_id": report.EnvironmentID}
	if len(payload) != len(want) || payload["report_id"] != want["report_id"] || payload["environment_id"] != want["environment_id"] {
		t.Fatalf("%s payload=%v, want exactly %v", kind, payload, want)
	}
}

func runFeedbackReportFirstEntry(t *testing.T, x Fixture) {
	e := newFeedbackEnvironment(t, x, "Production", "watcher")
	if reports := e.reports(t); len(reports) != 0 {
		t.Fatalf("reports before any request=%+v", reports)
	}
	full := store.FeedbackReportEntry{
		Kind: store.FeedbackReportAlert, Key: "checkout-timeout", Summary: "Checkout timed out",
		Service: "api", Repository: "app", Commit: "abcdef0", Version: "1.2.3", Count: 7,
		FirstSeen: feedbackReportTimePtr(-time.Hour), LastSeen: feedbackReportTimePtr(-time.Minute),
		File: "internal/checkout/pay.go", Line: 42,
	}
	bare := reportEntry("bare")
	e.sendReports(t, 0, full, bare)
	got := e.reportByKey(t, "checkout-timeout")
	if !feedbackReportIDPattern.MatchString(got.ID) || !strings.HasPrefix(got.ID, "rpt-"+feedbackTime(0).Format("060102")+"-") {
		t.Fatalf("report ID=%q, want rpt-<yymmdd>-<6 hex> of the arrival", got.ID)
	}
	stated := store.FeedbackReportVersion{Repository: "app", Commit: "abcdef0", Version: "1.2.3"}
	want := store.FeedbackReport{
		ID: got.ID, EnvironmentID: e.id, Kind: store.FeedbackReportAlert, Key: "checkout-timeout", Summary: "Checkout timed out",
		Service: "api", Count: 7, FirstSeen: feedbackTime(-time.Hour), LastSeen: feedbackTime(-time.Minute), FirstEntrySeen: feedbackTime(-time.Hour),
		File: "internal/checkout/pay.go", Line: 42, First: stated, Latest: stated,
		State: store.FeedbackReportOpen, InInbox: true,
		Origin:    store.FeedbackReportOrigin{Source: store.FeedbackSourceReport, Label: "watcher", Actor: store.SystemActor().ID},
		CreatedAt: feedbackTime(0),
	}
	requireReport(t, got, want)
	// An omitted count is 1 and each omitted time is the arrival.
	gotBare := e.reportByKey(t, "bare")
	requireReport(t, gotBare, store.FeedbackReport{
		ID: gotBare.ID, EnvironmentID: e.id, Kind: store.FeedbackReportError, Key: "bare", Summary: bare.Summary,
		Count: 1, FirstSeen: feedbackTime(0), LastSeen: feedbackTime(0), FirstEntrySeen: feedbackTime(0),
		State: store.FeedbackReportOpen, InInbox: true, Origin: want.Origin, CreatedAt: feedbackTime(0),
	})
	if got.ID == gotBare.ID {
		t.Fatalf("two reports share the ID %q", got.ID)
	}
	// A time later than the arrival is taken as the arrival.
	future := reportEntry("future")
	future.FirstSeen, future.LastSeen = feedbackReportTimePtr(time.Hour), feedbackReportTimePtr(2*time.Hour)
	e.sendReports(t, 10*time.Second, future)
	if report := e.reportByKey(t, "future"); !report.FirstSeen.Equal(feedbackTime(10*time.Second)) || !report.LastSeen.Equal(feedbackTime(10*time.Second)) {
		t.Fatalf("future times stored as first=%v last=%v, want the arrival", report.FirstSeen, report.LastSeen)
	}
	// A summary is redacted and its removals are counted by class.
	secret := "token ghp_0123456789abcdefghij0123456789abcdefghij leaked"
	redacted, stats := redact.New(nil).Redact(secret)
	if stats.Total() == 0 {
		t.Fatal("the fixture secret is not one the redactor removes")
	}
	leak := reportEntry("leak")
	leak.Summary = secret
	e.sendReports(t, 20*time.Second, leak)
	if report := e.reportByKey(t, "leak"); report.Summary != redacted || report.SummaryRedactions != stats || strings.Contains(report.Summary, "ghp_") {
		t.Fatalf("stored summary=%q redactions=%+v, want %q with %+v", report.Summary, report.SummaryRedactions, redacted, stats)
	}
}

// requireReport compares a stored report with the expected one.
func requireReport(t *testing.T, got, want store.FeedbackReport) {
	t.Helper()
	gotJSON, err := json.Marshal(got)
	requireOK(t, err)
	wantJSON, err := json.Marshal(want)
	requireOK(t, err)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("report=%s\nwant  %s", gotJSON, wantJSON)
	}
}

func runFeedbackReportLaterEntry(t *testing.T, x Fixture) {
	e := newFeedbackEnvironment(t, x, "Production", "watcher")
	first := store.FeedbackReportEntry{
		Kind: store.FeedbackReportError, Key: "db-down", Summary: "Database down", Service: "api",
		Repository: "app", Commit: "abcdef0", Count: 3,
		FirstSeen: feedbackReportTimePtr(-10 * time.Minute), LastSeen: feedbackReportTimePtr(-5 * time.Minute),
		File: "db.go", Line: 9,
	}
	e.sendReports(t, 0, first)
	stored := e.reportByKey(t, "db-down")
	events := len(reportEvents(t, x))
	notices := x.Backend.Changes().Subscribe(x.Workspace, changes.Workspace)
	defer notices.Close()
	// The later entry states other values for every member a later entry
	// cannot change, an earlier first-seen, and a later last-seen.
	later := store.FeedbackReportEntry{
		Kind: store.FeedbackReportMessage, Key: "db-down", Summary: "Another text", Service: "web",
		Repository: "conveyor", Commit: "1234567", Version: "2.0", Count: 4,
		FirstSeen: feedbackReportTimePtr(-20 * time.Minute), LastSeen: feedbackReportTimePtr(-time.Minute),
		File: "other.go", Line: 77,
	}
	e.sendReports(t, 30*time.Second, later)
	want := stored
	want.Count = 7
	want.FirstSeen, want.LastSeen = feedbackTime(-20*time.Minute), feedbackTime(-time.Minute)
	want.Latest = store.FeedbackReportVersion{Repository: "conveyor", Commit: "1234567", Version: "2.0"}
	got := e.reportByKey(t, "db-down")
	requireReport(t, got, want)
	// The earlier first-seen moved the report's earliest time back and left
	// the first entry's own first-seen time as it was.
	if !got.FirstEntrySeen.Equal(feedbackTime(-10 * time.Minute)) {
		t.Fatalf("first entry's first-seen=%v, want it unchanged", got.FirstEntrySeen)
	}
	// An entry with an older last-seen keeps the later one and an earlier
	// first-seen keeps the earlier one.
	older := reportEntry("db-down")
	older.Repository = "app"
	older.FirstSeen, older.LastSeen = feedbackReportTimePtr(-15*time.Minute), feedbackReportTimePtr(-15*time.Minute)
	e.sendReports(t, time.Minute, older)
	want.Count = 8
	want.Latest = store.FeedbackReportVersion{Repository: "app"}
	requireReport(t, e.reportByKey(t, "db-down"), want)
	if after := len(reportEvents(t, x)); after != events {
		t.Fatalf("updates appended %d events", after-events)
	}
	expectQuiet(t, notices, "an entry that only updates a report")
}

func runFeedbackReportLatestCleared(t *testing.T, x Fixture) {
	e := newFeedbackEnvironment(t, x, "Production", "watcher")
	first := reportEntry("crash")
	first.Repository, first.Commit, first.Version = "app", "abcdef0", "1.0"
	e.sendReports(t, 0, first)
	e.sendReports(t, 10*time.Second, reportEntry("crash"))
	got := e.reportByKey(t, "crash")
	if got.Latest != (store.FeedbackReportVersion{}) || got.First != (store.FeedbackReportVersion{Repository: "app", Commit: "abcdef0", Version: "1.0"}) || got.Count != 2 {
		t.Fatalf("report=%+v, want latest replaced by none and first kept", got)
	}
	again := reportEntry("crash")
	again.Version = "1.1"
	e.sendReports(t, 20*time.Second, again)
	if got := e.reportByKey(t, "crash"); got.Latest != (store.FeedbackReportVersion{Version: "1.1"}) {
		t.Fatalf("latest=%+v, want only the stated version", got.Latest)
	}
}

func runFeedbackReportReopen(t *testing.T, x Fixture) {
	e := newFeedbackEnvironment(t, x, "Production", "watcher")
	e.sendReports(t, 0, reportEntry("flaky"))
	report := e.reportByKey(t, "flaky")
	closedAt := feedbackTime(time.Minute)
	x.CloseFeedbackReport(t, x.Context, report.ID, closedAt)
	closed := e.reportByKey(t, "flaky")
	if closed.State != store.FeedbackReportClosed || closed.InInbox || closed.ClosedAt == nil || !closed.ClosedAt.Equal(closedAt) {
		t.Fatalf("fixture closing=%+v", closed)
	}
	events := len(reportEvents(t, x))
	notices := x.Backend.Changes().Subscribe(x.Workspace, changes.Workspace)
	defer notices.Close()
	second, err := x.Backend.IssueEnvironmentKey(x.Context, e.id, "second")
	requireOK(t, err)
	entry := reportEntry("flaky")
	entry.FirstSeen, entry.LastSeen = feedbackReportTimePtr(time.Minute), feedbackReportTimePtr(time.Minute+time.Microsecond)
	batch := e.reportBatch(2*time.Minute, entry)
	batch.KeyID = second.ID
	requireOK(t, x.Backend.RecordFeedbackReports(x.Context, batch))
	got := e.reportByKey(t, "flaky")
	want := report
	want.Count = 2
	want.LastSeen = feedbackTime(time.Minute + time.Microsecond)
	reopenedAt := feedbackTime(2 * time.Minute)
	want.ReopenedAt = &reopenedAt
	want.ReopenOrigin = &store.FeedbackReportOrigin{Source: store.FeedbackSourceReport, Label: "second", Actor: store.SystemActor().ID}
	requireReport(t, got, want)
	all := reportEvents(t, x)
	if len(all) != events+1 {
		t.Fatalf("a reopening appended %d events, want 1", len(all)-events)
	}
	requireReportEvent(t, all[len(all)-1], store.FeedbackReportReopenedEvent, got)
	if n := awaitNotice(t, notices, "workspace notice after a reopening"); n.Kind != changes.Workspace || n.Workspace != x.Workspace {
		t.Fatalf("notice=%+v", n)
	}
}

// runFeedbackReportSubMicrosecond holds every backend to the exact order of
// stated times that differ by less than one microsecond, which no backend can
// store (component-feedback-reports invariants 4 and 7;
// req-production-feedback AC-4.1, AC-4.4). A reversed pair is refused. A
// last-seen time strictly after the closing time reopens the report, and the
// stored times keep microsecond precision.
func runFeedbackReportSubMicrosecond(t *testing.T, x Fixture) {
	e := newFeedbackEnvironment(t, x, "Production", "watcher")
	base := feedbackTime(-time.Minute)
	at := func(nanoseconds int) *time.Time {
		value := base.Add(time.Duration(nanoseconds))
		return &value
	}
	before := e.reportState(t)
	reversed := reportEntry("reversed")
	reversed.FirstSeen, reversed.LastSeen = at(500), at(100)
	requireFeedbackField(t, x.Backend.RecordFeedbackReports(x.Context, e.reportBatch(0, reversed)), "reports[0].first_seen")
	if after := e.reportState(t); after != before {
		t.Fatalf("a reversed pair within one microsecond changed the store:\nbefore %s\nafter  %s", before, after)
	}
	ordered := reportEntry("ordered")
	ordered.FirstSeen, ordered.LastSeen = at(100), at(500)
	e.sendReports(t, 0, ordered)
	if got := e.reportByKey(t, "ordered"); !got.FirstSeen.Equal(base) || !got.LastSeen.Equal(base) || !got.FirstEntrySeen.Equal(base) {
		t.Fatalf("stored times first=%v last=%v first entry=%v, want each at %v", got.FirstSeen, got.LastSeen, got.FirstEntrySeen, base)
	}
	// The closing time is stored at microsecond precision. A last-seen
	// time equal to it leaves the report closed, and one 500 nanoseconds
	// later reopens it.
	report := e.reportByKey(t, "ordered")
	x.CloseFeedbackReport(t, x.Context, report.ID, base)
	same := reportEntry("ordered")
	same.FirstSeen, same.LastSeen = at(0), at(0)
	e.sendReports(t, 10*time.Second, same)
	if got := e.reportByKey(t, "ordered"); got.State != store.FeedbackReportClosed || got.Count != 2 {
		t.Fatalf("a last-seen equal to the closing time gave %+v, want the report closed", got)
	}
	events := len(reportEvents(t, x))
	later := reportEntry("ordered")
	later.FirstSeen, later.LastSeen = at(0), at(500)
	e.sendReports(t, 20*time.Second, later)
	got := e.reportByKey(t, "ordered")
	if got.State != store.FeedbackReportOpen || got.ReopenedAt == nil || !got.ReopenedAt.Equal(feedbackTime(20*time.Second)) || got.Count != 3 || !got.LastSeen.Equal(base) {
		t.Fatalf("a last-seen 500 nanoseconds after the closing time gave %+v, want the report reopened", got)
	}
	all := reportEvents(t, x)
	if len(all) != events+1 {
		t.Fatalf("the reopening appended %d events, want 1", len(all)-events)
	}
	requireReportEvent(t, all[len(all)-1], store.FeedbackReportReopenedEvent, got)
}

func runFeedbackReportStaysClosed(t *testing.T, x Fixture) {
	e := newFeedbackEnvironment(t, x, "Production", "watcher")
	e.sendReports(t, 0, reportEntry("stale"))
	report := e.reportByKey(t, "stale")
	closedAt := feedbackTime(time.Minute)
	x.CloseFeedbackReport(t, x.Context, report.ID, closedAt)
	closed := e.reportByKey(t, "stale")
	events := len(reportEvents(t, x))
	// A last-seen equal to the closing time, and one before it, update the
	// closed report and leave it closed.
	for i, lastSeen := range []time.Duration{time.Minute, 30 * time.Second} {
		entry := reportEntry("stale")
		entry.FirstSeen, entry.LastSeen = feedbackReportTimePtr(-time.Hour), feedbackReportTimePtr(lastSeen)
		entry.Commit = "abcdef0"
		e.sendReports(t, 2*time.Minute+time.Duration(i)*time.Second, entry)
	}
	want := closed
	want.Count = 3
	want.FirstSeen = feedbackTime(-time.Hour)
	want.LastSeen = feedbackTime(time.Minute)
	want.Latest = store.FeedbackReportVersion{Commit: "abcdef0"}
	requireReport(t, e.reportByKey(t, "stale"), want)
	if after := len(reportEvents(t, x)); after != events {
		t.Fatalf("entries that left a report closed appended %d events", after-events)
	}
}

func runFeedbackReportLimit(t *testing.T, x Fixture) {
	e := newFeedbackEnvironment(t, x, "Production", "watcher")
	e.fillReports(t, 0, "fill", store.MaxOpenFeedbackReports-2)
	// A request whose new reports exceed the remaining slots is refused
	// whole, its update included.
	update := reportEntry("fill-0")
	update.Count = 5
	before := e.reportState(t)
	mixed := e.reportBatch(time.Second, update, reportEntry("new-a"), reportEntry("new-b"), reportEntry("new-c"))
	if err := x.Backend.RecordFeedbackReports(x.Context, mixed); !errors.Is(err, store.ErrOpenReportLimit) {
		t.Fatalf("three new reports at %d open err=%v, want the limit", store.MaxOpenFeedbackReports-2, err)
	}
	if after := e.reportState(t); after != before {
		t.Fatalf("a refused request changed the store:\nbefore %s\nafter  %s", before, after)
	}
	// One new report is admitted at 199 open reports.
	e.sendReports(t, time.Second, reportEntry("new-a"))
	e.sendReports(t, 2*time.Second, update, reportEntry("new-b"))
	if open := e.openReports(t); open != store.MaxOpenFeedbackReports {
		t.Fatalf("open reports=%d, want %d", open, store.MaxOpenFeedbackReports)
	}
	// At 200 open reports a new report is refused, and updates alone are not.
	before = e.reportState(t)
	if err := x.Backend.RecordFeedbackReports(x.Context, e.reportBatch(3*time.Second, update, reportEntry("new-c"))); !errors.Is(err, store.ErrOpenReportLimit) {
		t.Fatalf("a new report at %d open err=%v, want the limit", store.MaxOpenFeedbackReports, err)
	}
	if after := e.reportState(t); after != before {
		t.Fatalf("a refused request changed the store:\nbefore %s\nafter  %s", before, after)
	}
	e.sendReports(t, 4*time.Second, update, reportEntry("fill-1"))
	if got := e.reportByKey(t, "fill-0"); got.Count != 1+5+5 {
		t.Fatalf("count after two updates=%d", got.Count)
	}
	// Reopening is never refused by the limit: close one, fill its slot,
	// and reopen it with 200 reports open.
	closing := e.reportByKey(t, "fill-2")
	x.CloseFeedbackReport(t, x.Context, closing.ID, feedbackTime(5*time.Second))
	e.sendReports(t, 6*time.Second, reportEntry("new-c"))
	if open := e.openReports(t); open != store.MaxOpenFeedbackReports {
		t.Fatalf("open reports=%d, want %d", open, store.MaxOpenFeedbackReports)
	}
	reopen := reportEntry("fill-2")
	reopen.LastSeen = feedbackReportTimePtr(7 * time.Second)
	e.sendReports(t, 7*time.Second, reopen)
	if got := e.reportByKey(t, "fill-2"); got.State != store.FeedbackReportOpen {
		t.Fatalf("report after a reopening at the limit=%+v", got)
	}
	if open := e.openReports(t); open != store.MaxOpenFeedbackReports+1 {
		t.Fatalf("open reports=%d, want %d", open, store.MaxOpenFeedbackReports+1)
	}
	// Another environment has its own limit.
	other := newFeedbackEnvironment(t, x, "Staging", "watcher")
	other.sendReports(t, 8*time.Second, reportEntry("new-d"))
}

func runFeedbackReportLastSlot(t *testing.T, x Fixture) {
	e := newFeedbackEnvironment(t, x, "Production", "watcher")
	e.fillReports(t, 0, "fill", store.MaxOpenFeedbackReports-1)
	second, err := x.Backend.IssueEnvironmentKey(x.Context, e.id, "second")
	requireOK(t, err)
	var acts []func() error
	for i, key := range []string{e.key.ID, second.ID, e.key.ID, second.ID} {
		batch := e.reportBatch(time.Second, reportEntry(fmt.Sprintf("race-%d", i)))
		batch.KeyID = key
		acts = append(acts, func() error { return x.Backend.RecordFeedbackReports(x.Context, batch) })
	}
	results := environmentRace(acts...)
	if environmentWinners(results) != 1 {
		t.Fatalf("concurrent requests for the last slot=%v, want one winner", results)
	}
	for _, err := range results {
		if err != nil && !errors.Is(err, store.ErrOpenReportLimit) {
			t.Fatalf("a losing request err=%v, want the limit", err)
		}
	}
	if open := e.openReports(t); open != store.MaxOpenFeedbackReports {
		t.Fatalf("open reports=%d, want %d", open, store.MaxOpenFeedbackReports)
	}
	if events := reportEvents(t, x); len(events) != store.MaxOpenFeedbackReports {
		t.Fatalf("opened events=%d, want %d", len(events), store.MaxOpenFeedbackReports)
	}
}

func runFeedbackReportTwoEntries(t *testing.T, x Fixture) {
	e := newFeedbackEnvironment(t, x, "Production", "watcher")
	e.sendReports(t, 0, reportEntry("one"))
	events := len(reportEvents(t, x))
	e.sendReports(t, 10*time.Second, reportEntry("one"), reportEntry("two"))
	if one, two := e.reportByKey(t, "one"), e.reportByKey(t, "two"); one.Count != 2 || two.Count != 1 || two.State != store.FeedbackReportOpen {
		t.Fatalf("after one request with two entries: one=%+v two=%+v", one, two)
	}
	all := reportEvents(t, x)
	if len(all) != events+1 {
		t.Fatalf("one new report and one update appended %d events, want 1", len(all)-events)
	}
	requireReportEvent(t, all[len(all)-1], store.FeedbackReportOpenedEvent, e.reportByKey(t, "two"))
	// A request whose second entry is invalid records nothing, its valid
	// first entry included.
	before := e.reportState(t)
	bad := reportEntry("three")
	bad.Kind = "panic"
	requireFeedbackField(t, x.Backend.RecordFeedbackReports(x.Context, e.reportBatch(20*time.Second, reportEntry("one"), bad)), "reports[1].kind")
	ghost := reportEntry("three")
	ghost.Repository = "ghost"
	requireFeedbackField(t, x.Backend.RecordFeedbackReports(x.Context, e.reportBatch(20*time.Second, reportEntry("one"), ghost)), "reports[1].repository")
	requireFeedbackField(t, x.Backend.CheckFeedbackReportRepositories(x.Context, e.reportBatch(20*time.Second, reportEntry("one"), ghost)), "reports[1].repository")
	if after := e.reportState(t); after != before {
		t.Fatalf("a refused request changed the store:\nbefore %s\nafter  %s", before, after)
	}
}

func runFeedbackReportRefusals(t *testing.T, x Fixture) {
	e := newFeedbackEnvironment(t, x, "Production", "watcher")
	e.sendReports(t, 0, reportEntry("kept"))
	before := e.reportState(t)
	with := func(change func(*store.FeedbackReportEntry)) store.FeedbackReportEntry {
		entry := reportEntry("new")
		change(&entry)
		return entry
	}
	var tooMany []store.FeedbackReportEntry
	for i := range store.MaxFeedbackReportEntries + 1 {
		tooMany = append(tooMany, reportEntry(fmt.Sprint(i)))
	}
	secret := "ghp_0123456789abcdefghij0123456789abcdefghij"
	for _, c := range []struct {
		field   string
		entries []store.FeedbackReportEntry
	}{
		{"reports", nil},
		{"reports", tooMany},
		{"reports[1].key", []store.FeedbackReportEntry{reportEntry("same"), reportEntry("same")}},
		{"reports[0].kind", []store.FeedbackReportEntry{with(func(e *store.FeedbackReportEntry) { e.Kind = "" })}},
		{"reports[0].key", []store.FeedbackReportEntry{with(func(e *store.FeedbackReportEntry) { e.Key = "" })}},
		{"reports[0].key", []store.FeedbackReportEntry{with(func(e *store.FeedbackReportEntry) { e.Key = strings.Repeat("é", 201) })}},
		{"reports[0].key", []store.FeedbackReportEntry{with(func(e *store.FeedbackReportEntry) { e.Key = secret })}},
		{"reports[0].summary", []store.FeedbackReportEntry{with(func(e *store.FeedbackReportEntry) { e.Summary = "" })}},
		{"reports[0].summary", []store.FeedbackReportEntry{with(func(e *store.FeedbackReportEntry) { e.Summary = strings.Repeat("é", 1001) })}},
		{"reports[0].summary", []store.FeedbackReportEntry{with(func(e *store.FeedbackReportEntry) { e.Summary = "a\x00b" })}},
		{"reports[0].service", []store.FeedbackReportEntry{with(func(e *store.FeedbackReportEntry) { e.Service = strings.Repeat("s", 101) })}},
		{"reports[0].service", []store.FeedbackReportEntry{with(func(e *store.FeedbackReportEntry) { e.Service = secret })}},
		{"reports[0].version", []store.FeedbackReportEntry{with(func(e *store.FeedbackReportEntry) { e.Version = strings.Repeat("v", 201) })}},
		{"reports[0].version", []store.FeedbackReportEntry{with(func(e *store.FeedbackReportEntry) { e.Version = secret })}},
		{"reports[0].commit", []store.FeedbackReportEntry{with(func(e *store.FeedbackReportEntry) { e.Commit = "abcdef" })}},
		{"reports[0].commit", []store.FeedbackReportEntry{with(func(e *store.FeedbackReportEntry) { e.Commit = "xyzxyzx" })}},
		{"reports[0].count", []store.FeedbackReportEntry{with(func(e *store.FeedbackReportEntry) { e.Count = store.MaxFeedbackReportCount + 1 })}},
		{"reports[0].count", []store.FeedbackReportEntry{with(func(e *store.FeedbackReportEntry) { e.Count = -1 })}},
		{"reports[0].file", []store.FeedbackReportEntry{with(func(e *store.FeedbackReportEntry) { e.File = "main.go" })}},
		{"reports[0].file", []store.FeedbackReportEntry{with(func(e *store.FeedbackReportEntry) { e.Line = 3 })}},
		{"reports[0].file", []store.FeedbackReportEntry{with(func(e *store.FeedbackReportEntry) { e.File, e.Line = strings.Repeat("f", 501), 1 })}},
		{"reports[0].file", []store.FeedbackReportEntry{with(func(e *store.FeedbackReportEntry) { e.File, e.Line = secret, 1 })}},
		{"reports[0].line", []store.FeedbackReportEntry{with(func(e *store.FeedbackReportEntry) { e.File, e.Line = "main.go", store.MaxFeedbackReportLine+1 })}},
		{"reports[0].first_seen", []store.FeedbackReportEntry{with(func(e *store.FeedbackReportEntry) {
			e.FirstSeen, e.LastSeen = feedbackReportTimePtr(-time.Minute), feedbackReportTimePtr(-2*time.Minute)
		})}},
		{"reports[0].repository", []store.FeedbackReportEntry{with(func(e *store.FeedbackReportEntry) { e.Repository = "App" })}},
	} {
		err := x.Backend.RecordFeedbackReports(x.Context, e.reportBatch(10*time.Second, c.entries...))
		requireFeedbackField(t, err, c.field)
		if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "ghost") {
			t.Fatalf("refusal %q repeats a submitted value", err)
		}
	}
	if after := e.reportState(t); after != before {
		t.Fatalf("refused requests changed the store:\nbefore %s\nafter  %s", before, after)
	}
}

func runFeedbackReportIsolation(t *testing.T, x Fixture) {
	production := newFeedbackEnvironment(t, x, "Production", "watcher")
	staging := newFeedbackEnvironment(t, x, "Staging", "watcher")
	production.sendReports(t, 0, reportEntry("shared"))
	staging.sendReports(t, 0, reportEntry("shared"))
	staging.sendReports(t, 10*time.Second, reportEntry("shared"))
	a, b := production.reportByKey(t, "shared"), staging.reportByKey(t, "shared")
	if a.ID == b.ID || a.Count != 1 || b.Count != 2 || a.EnvironmentID != production.id || b.EnvironmentID != staging.id {
		t.Fatalf("reports of one key in two environments: %+v and %+v", a, b)
	}
	// Problem keys compare exactly, so a key that differs only in case is
	// another problem.
	production.sendReports(t, 20*time.Second, reportEntry("Shared"))
	if reports := production.reports(t); len(reports) != 2 {
		t.Fatalf("reports of keys that differ in case=%+v, want two", reports)
	}
	// A key of one environment cannot write a report of another.
	batch := staging.reportBatch(30*time.Second, reportEntry("crossed"))
	batch.EnvironmentID = production.id
	if err := x.Backend.RecordFeedbackReports(x.Context, batch); !errors.Is(err, store.ErrInvalidEnvironmentKey) {
		t.Fatalf("a key of another environment err=%v", err)
	}
	if reports := production.reports(t); len(reports) != 2 {
		t.Fatalf("a refused key wrote reports=%+v", reports)
	}
}

func runFeedbackReportKeyValidity(t *testing.T, x Fixture) {
	e := newFeedbackEnvironment(t, x, "Production", "watcher")
	e.sendReports(t, 0, reportEntry("kept"))
	before := e.reportState(t)
	archiveEnvironment(t, x, e.id, true)
	archived := e.reportState(t)
	if err := x.Backend.RecordFeedbackReports(x.Context, e.reportBatch(10*time.Second, reportEntry("kept"), reportEntry("new"))); !errors.Is(err, store.ErrInvalidEnvironmentKey) {
		t.Fatalf("a request of an archived environment err=%v", err)
	}
	if after := e.reportState(t); after != archived {
		t.Fatalf("a refused request changed the store:\nbefore %s\nafter  %s", archived, after)
	}
	archiveEnvironment(t, x, e.id, false)
	requireOK(t, x.Backend.RevokeEnvironmentKey(x.Context, e.id, e.key.ID))
	revoked := e.reportState(t)
	if err := x.Backend.RecordFeedbackReports(x.Context, e.reportBatch(20*time.Second, reportEntry("kept"))); !errors.Is(err, store.ErrInvalidEnvironmentKey) {
		t.Fatalf("a request under a revoked key err=%v", err)
	}
	if after := e.reportState(t); after != revoked {
		t.Fatalf("a refused request changed the store:\nbefore %s\nafter  %s", revoked, after)
	}
	if before == revoked {
		t.Fatal("archive and revocation left no trace, so the comparison proves nothing")
	}
}

func runFeedbackReportLastUse(t *testing.T, x Fixture) {
	e := newFeedbackEnvironment(t, x, "Production", "watcher")
	if at := environmentLastUse(t, x, e.key.ID); at != nil {
		t.Fatalf("a new key has the last use %v", at)
	}
	e.sendReports(t, 0, reportEntry("one"))
	first := environmentLastUse(t, x, e.key.ID)
	if first == nil || time.Since(*first).Abs() > 10*time.Minute {
		t.Fatalf("last use after a report=%v", first)
	}
	e.sendReports(t, 10*time.Second, reportEntry("one"))
	if again := environmentLastUse(t, x, e.key.ID); !again.Equal(*first) {
		t.Fatalf("a second report within a minute moved the last use from %v to %v", first, again)
	}
	x.AgeEnvironmentKey(t, x.Context, e.key.ID, 2*time.Minute)
	aged := environmentLastUse(t, x, e.key.ID)
	requireFeedbackField(t, x.Backend.RecordFeedbackReports(x.Context, e.reportBatch(20*time.Second)), "reports")
	if again := environmentLastUse(t, x, e.key.ID); !again.Equal(*aged) {
		t.Fatalf("a refused report moved the last use from %v to %v", aged, again)
	}
	e.sendReports(t, 20*time.Second, reportEntry("one"))
	if again := environmentLastUse(t, x, e.key.ID); !again.After(*aged) {
		t.Fatalf("a report after a minute kept the last use at %v", again)
	}
	// The key's source label is on the report, and the last use is the only
	// write to the key.
	if report := e.reportByKey(t, "one"); report.Origin.Label != "watcher" {
		t.Fatalf("report origin=%+v", report.Origin)
	}
}

func runFeedbackReportEvents(t *testing.T, x Fixture) {
	e := newFeedbackEnvironment(t, x, "Production", "watcher")
	notices := x.Backend.Changes().Subscribe(x.Workspace, changes.Workspace)
	defer notices.Close()
	e.sendReports(t, 0, reportEntry("alpha-problem"), reportEntry("beta-problem"))
	if n := awaitNotice(t, notices, "workspace notice after a new report"); n.Kind != changes.Workspace || n.Workspace != x.Workspace {
		t.Fatalf("notice=%+v", n)
	}
	settle(notices)
	events := reportEvents(t, x)
	if len(events) != 2 {
		t.Fatalf("events of two new reports=%+v", events)
	}
	opened := map[string]bool{}
	for _, event := range events {
		var payload map[string]string
		requireOK(t, json.Unmarshal(event.Payload, &payload))
		report := e.reportByKey(t, map[string]string{e.reportByKey(t, "alpha-problem").ID: "alpha-problem", e.reportByKey(t, "beta-problem").ID: "beta-problem"}[payload["report_id"]])
		requireReportEvent(t, event, store.FeedbackReportOpenedEvent, report)
		opened[report.ID] = true
		// No text of the report is in the event.
		for _, text := range []string{report.Key, report.Summary} {
			if strings.Contains(string(event.Payload), text) {
				t.Fatalf("event payload %s holds report text %q", event.Payload, text)
			}
		}
	}
	if len(opened) != 2 {
		t.Fatalf("opened events name %v, want both reports", opened)
	}
	e.sendReports(t, 10*time.Second, reportEntry("alpha-problem"), reportEntry("beta-problem"))
	expectQuiet(t, notices, "a request that only updates reports")
	if after := reportEvents(t, x); len(after) != 2 {
		t.Fatalf("updates appended events: %+v", after)
	}
}

func runFeedbackReportObservesOnly(t *testing.T, x Fixture) {
	e := newFeedbackEnvironment(t, x, "Production", "watcher")
	requireOK(t, x.Backend.CreateTask(x.Context, core.Task{ID: core.NewTaskID(), Workspace: x.Workspace, Repo: "app", Title: "Task", State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: time.Now().UTC()}))
	e.send(t, 0, feedbackService("api", "abcdef0"))
	observe := func() string {
		t.Helper()
		tasks, err := x.Backend.ListTasks(x.Context)
		requireOK(t, err)
		orders, err := x.Backend.ListWorkOrders(x.Context)
		requireOK(t, err)
		var taskEvents []core.Event
		for _, task := range tasks {
			listed, err := x.Backend.ListEvents(x.Context, task.ID)
			requireOK(t, err)
			taskEvents = append(taskEvents, listed...)
		}
		newest, _ := e.newest(t)
		raw, err := json.Marshal([]any{tasks, orders, taskEvents, newest, e.running(t), e.deploys(t)})
		requireOK(t, err)
		return string(raw)
	}
	before := observe()
	e.sendReports(t, 10*time.Second, reportEntry("one"))
	e.sendReports(t, 20*time.Second, reportEntry("one"), reportEntry("two"))
	report := e.reportByKey(t, "one")
	x.CloseFeedbackReport(t, x.Context, report.ID, feedbackTime(25*time.Second))
	reopen := reportEntry("one")
	reopen.LastSeen = feedbackReportTimePtr(30 * time.Second)
	e.sendReports(t, 30*time.Second, reopen)
	if after := observe(); after != before {
		t.Fatalf("a report changed a task, a work order, a task event, or the signal state:\nbefore %s\nafter  %s", before, after)
	}
}

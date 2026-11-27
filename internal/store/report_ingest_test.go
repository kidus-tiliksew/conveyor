package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/redact"
)

// TestFeedbackReportTimesSweepOneMicrosecond holds the shared report rules
// to one rule: every decision about a report's times is made on exact values,
// and a time is cut to the stored microsecond only in the record to write.
// It sweeps offsets inside one microsecond for the arrival, the first-seen
// and last-seen times (each also left out), and the closing time, and checks
// the default, the clamp to the arrival, the order refusal, the merge, and
// the reopen decision for every combination (component-feedback-reports
// invariants 4, 6, and 7; req-production-feedback AC-4.1, AC-4.3, AC-4.4).
func TestFeedbackReportTimesSweepOneMicrosecond(t *testing.T) {
	base := time.Date(2026, 11, 25, 12, 0, 0, 123456000, time.UTC)
	offsets := []int{0, 200, 700, 900}
	stated := []*int{nil}
	for i := range offsets {
		stated = append(stated, &offsets[i])
	}
	at := func(offset int) time.Time { return base.Add(time.Duration(offset)) }
	effective := func(value *int, arrival int) int {
		if value == nil || *value > arrival {
			return arrival
		}
		return *value
	}
	name := func(value *int) string {
		if value == nil {
			return "omitted"
		}
		return fmt.Sprint(*value)
	}
	cases := 0
	for _, arrival := range offsets {
		for _, first := range stated {
			for _, last := range stated {
				entry := FeedbackReportEntry{Kind: FeedbackReportError, Key: "sweep", Summary: "Sweep"}
				if first != nil {
					value := at(*first)
					entry.FirstSeen = StatedFeedbackReportTime(value)
				}
				if last != nil {
					value := at(*last)
					entry.LastSeen = StatedFeedbackReportTime(value)
				}
				label := fmt.Sprintf("arrival=+%dns first=%s last=%s", arrival, name(first), name(last))
				normalized, err := NormalizeFeedbackReports(FeedbackReportBatch{EnvironmentID: "env", KeyID: "key", ArrivedAt: at(arrival), Entries: []FeedbackReportEntry{entry}})
				wantFirst, wantLast := effective(first, arrival), effective(last, arrival)
				if wantFirst > wantLast {
					var refused *FeedbackFieldError
					if !errors.As(err, &refused) || refused.Field != "reports[0].first_seen" {
						t.Fatalf("%s: err=%v, want the order refusal", label, err)
					}
					cases++
					continue
				}
				if err != nil {
					t.Fatalf("%s: err=%v", label, err)
				}
				got := normalized.Entries[0]
				if !normalized.ArrivedAt.Equal(at(arrival)) || !got.FirstSeen.Equal(at(wantFirst)) || !got.LastSeen.Equal(at(wantLast)) {
					t.Fatalf("%s: normalized arrival=%v first=%v last=%v, want exact +%d, +%d, +%d", label, normalized.ArrivedAt, got.FirstSeen, got.LastSeen, arrival, wantFirst, wantLast)
				}
				prepared := PreparedFeedbackReports{FeedbackReportBatch: normalized, Redactions: make([]core.RedactionStats, 1)}
				// A new report stores the cut times.
				plan, err := PlanFeedbackReports(t.Context(), prepared, "watcher", nil, 0, func(string) (bool, error) { return false, nil })
				if err != nil || len(plan.Changes) != 1 || !plan.Changes[0].New {
					t.Fatalf("%s: new report plan=%+v err=%v", label, plan, err)
				}
				report := plan.Changes[0].Report
				if !report.FirstSeen.Equal(base) || !report.LastSeen.Equal(base) || !report.FirstEntrySeen.Equal(base) || !report.CreatedAt.Equal(base) {
					t.Fatalf("%s: stored times first=%v last=%v first entry=%v created=%v, want each cut to %v", label, report.FirstSeen, report.LastSeen, report.FirstEntrySeen, report.CreatedAt, base)
				}
				for _, event := range plan.Events(normalized.ArrivedAt) {
					if !event.At.Equal(base) {
						t.Fatalf("%s: event time=%v, want %v", label, event.At, base)
					}
				}
				for _, closing := range offsets {
					closedAt := at(closing)
					stored := map[string]FeedbackReport{"sweep": {
						ID: "rpt-261125-000000", EnvironmentID: "env", Key: "sweep", Count: 1,
						FirstSeen: base.Add(-time.Hour), LastSeen: base.Add(-time.Hour), State: FeedbackReportClosed,
						ClosedAt: &closedAt, ClosedBy: "usr-closer", CloseReason: "fixed",
					}}
					plan, err := PlanFeedbackReports(t.Context(), prepared, "watcher", stored, 0, func(string) (bool, error) { return false, nil })
					if err != nil || len(plan.Changes) != 1 {
						t.Fatalf("%s close=+%d: plan=%+v err=%v", label, closing, plan, err)
					}
					change := plan.Changes[0]
					wantReopen := wantLast > closing
					if change.Reopened != wantReopen || change.Updated == wantReopen {
						t.Fatalf("%s close=+%dns: reopened=%v updated=%v, want reopened=%v", label, closing, change.Reopened, change.Updated, wantReopen)
					}
					merged := change.Report
					// The merge chooses on exact times and stores the cut one.
					if !merged.FirstSeen.Equal(base.Add(-time.Hour)) || !merged.LastSeen.Equal(base) {
						t.Fatalf("%s close=+%dns: merged first=%v last=%v", label, closing, merged.FirstSeen, merged.LastSeen)
					}
					if wantReopen && (merged.ReopenedAt == nil || !merged.ReopenedAt.Equal(base) || merged.State != FeedbackReportOpen || merged.ClosedAt != nil) {
						t.Fatalf("%s close=+%dns: reopened report=%+v, want it open with the reopening cut to %v", label, closing, merged, base)
					}
					if !wantReopen && (merged.State != FeedbackReportClosed || merged.ClosedAt == nil || !merged.ClosedAt.Equal(closedAt)) {
						t.Fatalf("%s close=+%dns: report=%+v, want it closed", label, closing, merged)
					}
					cases++
				}
			}
		}
	}
	if want := 4 * 5 * 5; cases < want {
		t.Fatalf("the sweep checked %d cases, want at least %d", cases, want)
	}
}

// TestFeedbackReportTimeRules is one table for the time rules of
// component-feedback-reports v2 invariant 4. For each stated time it checks
// the strict RFC 3339 parse, the refusal of a second of 60, of more than nine
// fraction digits, and of an instant before 1970-01-01T00:00:00Z (also through
// an offset), the clamp to the arrival (also for year 9999 whose offset puts
// its instant later), and the exact order check one nanosecond apart
// (req-production-feedback AC-4.1).
func TestFeedbackReportTimeRules(t *testing.T) {
	never := func(string) (bool, error) { return false, nil }
	arrival := time.Date(2026, 11, 25, 12, 0, 0, 123456789, time.UTC)
	floor := time.Unix(0, 0).UTC()
	field := func(member string) string { return "reports[0]." + member }
	for _, c := range []struct {
		name        string
		first, last string
		refusal     string // the fixed message, or empty when accepted
		member      string // the refused field
		storedFirst time.Time
		storedLast  time.Time
	}{
		// The parse and its refusals.
		{name: "uppercase T and Z", first: "2026-11-25T11:00:00Z", storedFirst: time.Date(2026, 11, 25, 11, 0, 0, 0, time.UTC), storedLast: EnvironmentTime(arrival)},
		{name: "lowercase t and z", first: "2026-11-25t11:00:00z", storedFirst: time.Date(2026, 11, 25, 11, 0, 0, 0, time.UTC), storedLast: EnvironmentTime(arrival)},
		{name: "a valid offset", first: "2026-11-25T13:00:00+02:00", last: "2026-11-25T11:30:00-00:30", storedFirst: time.Date(2026, 11, 25, 11, 0, 0, 0, time.UTC), storedLast: time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)},
		{name: "offset hour 24", first: "2026-11-25T11:00:00+24:00", refusal: FeedbackReportTimeInvalid, member: "first_seen"},
		{name: "offset minute 60", last: "2026-11-25T11:00:00+01:60", refusal: FeedbackReportTimeInvalid, member: "last_seen"},
		{name: "no offset", first: "2026-11-25T11:00:00", refusal: FeedbackReportTimeInvalid, member: "first_seen"},
		{name: "a day the month does not hold", first: "2026-02-29T11:00:00Z", refusal: FeedbackReportTimeInvalid, member: "first_seen"},
		{name: "second 60", first: "2026-11-25T11:00:60Z", refusal: FeedbackReportTimeLeapSecond, member: "first_seen"},
		{name: "a leap second of the table", last: "2016-12-31T23:59:60Z", refusal: FeedbackReportTimeLeapSecond, member: "last_seen"},
		{name: "second 60 through an offset", first: "2016-12-31T15:59:60-08:00", refusal: FeedbackReportTimeLeapSecond, member: "first_seen"},
		{name: "nine fraction digits", first: "2026-11-25T11:00:00.123456789Z", last: "2026-11-25T11:00:00.123456790Z", storedFirst: time.Date(2026, 11, 25, 11, 0, 0, 123456000, time.UTC), storedLast: time.Date(2026, 11, 25, 11, 0, 0, 123456000, time.UTC)},
		{name: "ten fraction digits", first: "2026-11-25T11:00:00.1234567891Z", refusal: FeedbackReportTimeTooPrecise, member: "first_seen"},
		{name: "ten zero fraction digits", last: "2026-11-25T11:00:00.0000000000Z", refusal: FeedbackReportTimeTooPrecise, member: "last_seen"},
		// The 1970 floor, compared on the instant.
		{name: "the floor", first: "1970-01-01T00:00:00Z", storedFirst: floor, storedLast: EnvironmentTime(arrival)},
		{name: "one nanosecond before the floor", first: "1969-12-31T23:59:59.999999999Z", refusal: FeedbackReportTimeBeforeEpoch, member: "first_seen"},
		{name: "the floor through a positive offset", first: "1970-01-01T01:00:00+01:00", storedFirst: floor, storedLast: EnvironmentTime(arrival)},
		{name: "the floor through a negative offset", first: "1969-12-31T23:00:00-01:00", storedFirst: floor, storedLast: EnvironmentTime(arrival)},
		{name: "before the floor through a positive offset", first: "1970-01-01T00:59:59.999999999+01:00", refusal: FeedbackReportTimeBeforeEpoch, member: "first_seen"},
		{name: "before the floor through a negative offset", last: "1969-12-31T22:59:59-01:00", refusal: FeedbackReportTimeBeforeEpoch, member: "last_seen"},
		{name: "a year before the floor", first: "0999-06-15T12:00:00Z", refusal: FeedbackReportTimeBeforeEpoch, member: "first_seen"},
		// After the arrival, a time is the arrival.
		{name: "year 9999 with a negative offset", first: "9999-12-31T23:30:00-01:00", storedFirst: EnvironmentTime(arrival), storedLast: EnvironmentTime(arrival)},
		{name: "one nanosecond after the arrival", first: "2026-11-25T12:00:00.12345679Z", storedFirst: EnvironmentTime(arrival), storedLast: EnvironmentTime(arrival)},
		// The order check on exact times.
		{name: "one nanosecond apart in order", first: "2026-11-25T11:00:00.000000001Z", last: "2026-11-25T11:00:00.000000002Z", storedFirst: time.Date(2026, 11, 25, 11, 0, 0, 0, time.UTC), storedLast: time.Date(2026, 11, 25, 11, 0, 0, 0, time.UTC)},
		{name: "one nanosecond apart reversed", first: "2026-11-25T11:00:00.000000002Z", last: "2026-11-25T11:00:00.000000001Z", refusal: "must not be later than last_seen", member: "first_seen"},
	} {
		t.Run(c.name, func(t *testing.T) {
			entry := FeedbackReportEntry{Kind: FeedbackReportError, Key: "times", Summary: "Times"}
			refusal, member := "", ""
			for _, stated := range []struct {
				name, text string
				target     **time.Time
			}{{"first_seen", c.first, &entry.FirstSeen}, {"last_seen", c.last, &entry.LastSeen}} {
				if stated.text == "" || refusal != "" {
					continue
				}
				at, parsed := ParseFeedbackReportTime(stated.text)
				if parsed != "" {
					refusal, member = parsed, stated.name
					continue
				}
				*stated.target = &at
			}
			normalized := FeedbackReportBatch{}
			if refusal == "" {
				var err error
				normalized, err = NormalizeFeedbackReports(FeedbackReportBatch{EnvironmentID: "env", KeyID: "key", ArrivedAt: arrival, Entries: []FeedbackReportEntry{entry}})
				var refused *FeedbackFieldError
				switch {
				case errors.As(err, &refused):
					refusal, member = refused.Message, strings.TrimPrefix(refused.Field, "reports[0].")
				case err != nil:
					t.Fatal(err)
				}
			}
			if c.refusal != "" {
				if refusal != c.refusal || field(member) != field(c.member) {
					t.Fatalf("refusal %q on %s, want %q on %s", refusal, member, c.refusal, c.member)
				}
				for _, text := range []string{c.first, c.last} {
					if text != "" && strings.Contains(refusal, text) {
						t.Fatalf("refusal %q repeats the submitted %q", refusal, text)
					}
				}
				return
			}
			if refusal != "" {
				t.Fatalf("refused %s: %s", member, refusal)
			}
			prepared := PreparedFeedbackReports{FeedbackReportBatch: normalized, Redactions: make([]core.RedactionStats, 1)}
			plan, err := PlanFeedbackReports(t.Context(), prepared, "watcher", nil, 0, never)
			if err != nil {
				t.Fatal(err)
			}
			report := plan.Changes[0].Report
			if !report.FirstSeen.Equal(c.storedFirst) || !report.LastSeen.Equal(c.storedLast) || !report.FirstEntrySeen.Equal(c.storedFirst) {
				t.Fatalf("stored first=%v last=%v first entry=%v, want %v and %v", report.FirstSeen, report.LastSeen, report.FirstEntrySeen, c.storedFirst, c.storedLast)
			}
		})
	}

	// The merge and the reopen decide on the exact stated nanoseconds.
	stored := time.Date(2026, 11, 25, 11, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name                string
		first, last         string
		wantFirst, wantLast time.Time
		reopened            bool
	}{
		{"equal to the closing time", "2026-11-25T11:00:00Z", "2026-11-25T11:00:00Z", stored.Add(-time.Hour), stored, false},
		{"one nanosecond after the closing time", "2026-11-25T11:00:00Z", "2026-11-25T11:00:00.000000001Z", stored.Add(-time.Hour), stored, true},
		{"an earlier first-seen one nanosecond before the stored one", "2026-11-25T09:59:59.999999999Z", "2026-11-25T10:00:00Z", stored.Add(-time.Hour - time.Microsecond), stored.Add(-time.Hour), false},
		{"a valid offset after the closing time", "2026-11-25T11:00:00Z", "2026-11-25T12:00:00.5+01:00", stored.Add(-time.Hour), stored.Add(500 * time.Millisecond), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			closedAt := stored
			reports := map[string]FeedbackReport{"times": {
				ID: "rpt-261125-000000", EnvironmentID: "env", Key: "times", Count: 1,
				FirstSeen: stored.Add(-time.Hour), LastSeen: stored.Add(-time.Hour), State: FeedbackReportClosed,
				ClosedAt: &closedAt, ClosedBy: "usr-closer", CloseReason: "fixed",
			}}
			first, refusal := ParseFeedbackReportTime(c.first)
			last, lastRefusal := ParseFeedbackReportTime(c.last)
			if refusal != "" || lastRefusal != "" {
				t.Fatalf("fixture times refused: %q %q", refusal, lastRefusal)
			}
			entry := FeedbackReportEntry{Kind: FeedbackReportError, Key: "times", Summary: "Times", FirstSeen: &first, LastSeen: &last}
			normalized, err := NormalizeFeedbackReports(FeedbackReportBatch{EnvironmentID: "env", KeyID: "key", ArrivedAt: arrival, Entries: []FeedbackReportEntry{entry}})
			if err != nil {
				t.Fatal(err)
			}
			prepared := PreparedFeedbackReports{FeedbackReportBatch: normalized, Redactions: make([]core.RedactionStats, 1)}
			plan, err := PlanFeedbackReports(t.Context(), prepared, "watcher", reports, 0, never)
			if err != nil {
				t.Fatal(err)
			}
			change := plan.Changes[0]
			if change.Reopened != c.reopened || !change.Report.FirstSeen.Equal(c.wantFirst) || !change.Report.LastSeen.Equal(c.wantLast) {
				t.Fatalf("reopened=%v first=%v last=%v, want reopened=%v first=%v last=%v", change.Reopened, change.Report.FirstSeen, change.Report.LastSeen, c.reopened, c.wantFirst, c.wantLast)
			}
		})
	}
}

// feedbackReportSecrets is a secret source that holds fixed stored secrets.
type feedbackReportSecrets []string

func (s feedbackReportSecrets) ListGitHubAppKeysForRedaction(context.Context) ([]string, error) {
	return s, nil
}

// TestFeedbackReportStoredSecretRefusal proves that a stored secret, one that
// matches no credential pattern and is not in the process registry, is
// refused in every text other than the summary, with a fixed message, and
// refuses the whole request. In the summary it is redacted and counted
// (component-feedback-reports invariant 5; req-security-boundaries REQ-3,
// AC-3.1).
func TestFeedbackReportStoredSecretRefusal(t *testing.T) {
	const secret = "plain-stored-secret-value-4815"
	source := feedbackReportSecrets{secret}
	arrival := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	batch := func(second FeedbackReportEntry) FeedbackReportBatch {
		return FeedbackReportBatch{EnvironmentID: "env", KeyID: "key", ArrivedAt: arrival, Entries: []FeedbackReportEntry{
			{Kind: FeedbackReportError, Key: "valid", Summary: "Valid"}, second,
		}}
	}
	if clean, _ := redact.New(nil).Redact(secret); clean != secret {
		t.Fatal("the fixture secret matches a pattern, so it does not prove the stored-secret check")
	}
	for _, c := range []struct {
		member string
		entry  FeedbackReportEntry
	}{
		{"key", FeedbackReportEntry{Kind: FeedbackReportError, Key: "k-" + secret, Summary: "s"}},
		{"service", FeedbackReportEntry{Kind: FeedbackReportError, Key: "other", Summary: "s", Service: secret}},
		{"repository", FeedbackReportEntry{Kind: FeedbackReportError, Key: "other", Summary: "s", Repository: secret}},
		{"version", FeedbackReportEntry{Kind: FeedbackReportError, Key: "other", Summary: "s", Version: "v-" + secret}},
		{"file", FeedbackReportEntry{Kind: FeedbackReportError, Key: "other", Summary: "s", File: "dir/" + secret + ".go", Line: 1}},
	} {
		// Without the stored secret the request is valid, so the refusal
		// comes from the stored secret alone.
		if _, err := PrepareFeedbackReports(t.Context(), feedbackReportSecrets{}, batch(c.entry)); err != nil {
			t.Fatalf("%s without the stored secret: %v", c.member, err)
		}
		prepared, err := PrepareFeedbackReports(t.Context(), source, batch(c.entry))
		var refused *FeedbackFieldError
		if !errors.As(err, &refused) || refused.Field != "reports[1]."+c.member || refused.Message != "must not hold a credential or other secret" || len(prepared.Entries) != 0 {
			t.Fatalf("%s holding a stored secret: prepared=%+v err=%v, want the whole request refused on reports[1].%s", c.member, prepared, err, c.member)
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("refusal %q repeats the secret", err)
		}
	}
	prepared, err := PrepareFeedbackReports(t.Context(), source, batch(FeedbackReportEntry{Kind: FeedbackReportError, Key: "other", Summary: "leaked " + secret}))
	if err != nil {
		t.Fatal(err)
	}
	if got := prepared.Entries[1].Summary; strings.Contains(got, secret) || prepared.Redactions[1].Exact != 1 {
		t.Fatalf("summary=%q redactions=%+v, want the secret removed and counted once as exact", got, prepared.Redactions[1])
	}
}

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
				if !normalized.ArrivedAt.Equal(at(arrival)) || !got.FirstSeen.At.Equal(at(wantFirst)) || !got.LastSeen.At.Equal(at(wantLast)) {
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

// TestFeedbackReportTimeRules sweeps the time rules together: the strict
// RFC 3339 parse with valid and invalid offsets, digits beyond nanoseconds in
// both orders, years 0000, 0999, 1000, and 9999, and the two offset cases at
// the ends of the stored range. For each stated pair it checks the parse, the
// clamp to the arrival, the range refusal, the order refusal, the stored
// value, the merge choice, and the reopen decision (component-feedback-reports
// invariants 4, 6, and 7; req-production-feedback AC-4.1, AC-4.3, AC-4.4).
func TestFeedbackReportTimeRules(t *testing.T) {
	never := func(string) (bool, error) { return false, nil }
	arrival := time.Date(2026, 11, 25, 12, 0, 0, 123456789, time.UTC)
	latest := time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)

	// The parse alone.
	for _, c := range []struct {
		text  string
		want  time.Time
		leap  bool
		extra string
	}{
		{"2026-11-25T11:00:00Z", time.Date(2026, 11, 25, 11, 0, 0, 0, time.UTC), false, ""},
		{"2026-11-25t11:00:00z", time.Date(2026, 11, 25, 11, 0, 0, 0, time.UTC), false, ""},
		{"2026-11-25T13:00:00+02:00", time.Date(2026, 11, 25, 11, 0, 0, 0, time.UTC), false, ""},
		{"2026-11-25T11:00:00-00:00", time.Date(2026, 11, 25, 11, 0, 0, 0, time.UTC), false, ""},
		{"2026-11-25T11:00:00+23:59", time.Date(2026, 11, 24, 11, 1, 0, 0, time.UTC), false, ""},
		{"2026-11-25T11:00:00.1234567899Z", time.Date(2026, 11, 25, 11, 0, 0, 123456789, time.UTC), false, "9"},
		{"2026-11-25T11:00:00.123456789000Z", time.Date(2026, 11, 25, 11, 0, 0, 123456789, time.UTC), false, ""},
		{"2016-12-31T23:59:60.5Z", time.Date(2016, 12, 31, 23, 59, 59, 500000000, time.UTC), true, ""},
		{"2016-12-31T15:59:60-08:00", time.Date(2016, 12, 31, 23, 59, 59, 0, time.UTC), true, ""},
		{"0000-02-29T00:00:00Z", time.Date(0, 2, 29, 0, 0, 0, 0, time.UTC), false, ""},
		{"0000-01-01T00:00:00+01:00", time.Date(-1, 12, 31, 23, 0, 0, 0, time.UTC), false, ""},
		{"9999-12-31T23:30:00-01:00", time.Date(10000, 1, 1, 0, 30, 0, 0, time.UTC), false, ""},
	} {
		got, ok := ParseFeedbackReportTime(c.text)
		if !ok || !got.At.Equal(c.want) || got.Leap != c.leap || got.Beyond != c.extra {
			t.Fatalf("parse %q=%+v ok=%v, want %v leap=%v beyond=%q", c.text, got, ok, c.want, c.leap, c.extra)
		}
	}
	for _, text := range []string{
		"2026-11-25T11:00:00+24:00", "2026-11-25T11:00:00+01:60", "2026-11-25T11:00:00+1:00", "2026-11-25T11:00:00+0100",
		"2026-11-25T11:00:00", "2026-11-25 11:00:00Z", "2026-11-25T11:00Z", "2026-11-25T24:00:00Z", "2026-11-25T11:60:00Z",
		"2026-02-29T11:00:00Z", "2026-13-01T11:00:00Z", "2026-00-01T11:00:00Z", "2026-11-00T11:00:00Z", "2026-11-25T11:00:00.Z",
		"2026-11-25T11:00:00,5Z", "2016-12-31T23:58:60Z", "+2026-11-25T11:00:00Z", "26-11-25T11:00:00Z", "2026-11-25T11:00:00ZZ",
	} {
		if got, ok := ParseFeedbackReportTime(text); ok {
			t.Fatalf("parse %q=%+v, want a refusal", text, got)
		}
	}

	// The rules on a pair of stated times.
	parse := func(text string) *FeedbackReportTime {
		got, ok := ParseFeedbackReportTime(text)
		if !ok {
			t.Fatalf("fixture time %q does not parse", text)
		}
		return &got
	}
	type outcome struct {
		field                   string // a refused field, or empty
		storedFirst, storedLast time.Time
	}
	for _, c := range []struct {
		name        string
		arrival     time.Time
		first, last string
		want        outcome
	}{
		{"beyond nanoseconds in order", arrival, "2026-11-25T11:00:00.1234567891Z", "2026-11-25T11:00:00.1234567899Z",
			outcome{storedFirst: time.Date(2026, 11, 25, 11, 0, 0, 123456000, time.UTC), storedLast: time.Date(2026, 11, 25, 11, 0, 0, 123456000, time.UTC)}},
		{"beyond nanoseconds reversed", arrival, "2026-11-25T11:00:00.1234567899Z", "2026-11-25T11:00:00.1234567891Z", outcome{field: "reports[0].first_seen"}},
		{"beyond nanoseconds past the arrival is the arrival", arrival, "2026-11-25T12:00:00.1234567891Z", "",
			outcome{storedFirst: EnvironmentTime(arrival), storedLast: EnvironmentTime(arrival)}},
		{"the exact arrival is kept", arrival, "2026-11-25T12:00:00.123456789Z", "",
			outcome{storedFirst: EnvironmentTime(arrival), storedLast: EnvironmentTime(arrival)}},
		{"a valid offset", arrival, "2026-11-25T13:00:00+02:00", "2026-11-25T11:30:00-00:30",
			outcome{storedFirst: time.Date(2026, 11, 25, 11, 0, 0, 0, time.UTC), storedLast: time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)}},
		{"offsets that reverse the local order", arrival, "2026-11-25T10:00:00-01:00", "2026-11-25T11:30:00+01:00", outcome{field: "reports[0].first_seen"}},
		{"a leap second after its second 59", arrival, "2016-12-31T23:59:59.9Z", "2016-12-31T23:59:60.1Z",
			outcome{storedFirst: time.Date(2016, 12, 31, 23, 59, 59, 900000000, time.UTC), storedLast: time.Date(2016, 12, 31, 23, 59, 59, 999999000, time.UTC)}},
		{"a leap second before the next minute", arrival, "2016-12-31T23:59:60.9Z", "2016-12-31T23:59:59.9Z", outcome{field: "reports[0].first_seen"}},
		{"year 0000", latest, "0000-01-01T00:00:00Z", "0000-12-31T23:59:59.9999999Z",
			outcome{storedFirst: time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC), storedLast: time.Date(0, 12, 31, 23, 59, 59, 999999000, time.UTC)}},
		{"year 0999", latest, "0999-06-15T12:00:00.000001Z", "0999-12-31T23:59:59Z",
			outcome{storedFirst: time.Date(999, 6, 15, 12, 0, 0, 1000, time.UTC), storedLast: time.Date(999, 12, 31, 23, 59, 59, 0, time.UTC)}},
		{"year 1000", latest, "1000-01-01T00:00:00Z", "1000-01-01T00:00:00.000001Z",
			outcome{storedFirst: time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC), storedLast: time.Date(1000, 1, 1, 0, 0, 0, 1000, time.UTC)}},
		{"year 9999", latest, "9999-01-01T00:00:00Z", "9999-12-31T23:59:59.999999Z",
			outcome{storedFirst: time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), storedLast: latest}},
		{"an offset below the range", latest, "0000-01-01T00:00:00+01:00", "", outcome{field: "reports[0].first_seen"}},
		{"an offset below the range on last-seen", latest, "", "0000-01-01T00:30:00+01:00", outcome{field: "reports[0].last_seen"}},
		{"an offset above the range is the arrival", latest, "9999-12-31T23:00:00Z", "9999-12-31T23:30:00-01:00",
			outcome{storedFirst: time.Date(9999, 12, 31, 23, 0, 0, 0, time.UTC), storedLast: latest}},
	} {
		t.Run(c.name, func(t *testing.T) {
			entry := FeedbackReportEntry{Kind: FeedbackReportError, Key: "times", Summary: "Times"}
			if c.first != "" {
				entry.FirstSeen = parse(c.first)
			}
			if c.last != "" {
				entry.LastSeen = parse(c.last)
			}
			normalized, err := NormalizeFeedbackReports(FeedbackReportBatch{EnvironmentID: "env", KeyID: "key", ArrivedAt: c.arrival, Entries: []FeedbackReportEntry{entry}})
			if c.want.field != "" {
				var refused *FeedbackFieldError
				if !errors.As(err, &refused) || refused.Field != c.want.field || refused.Message == "" || (c.first != "" && strings.Contains(refused.Message, c.first)) {
					t.Fatalf("err=%v, want a fixed refusal of %s", err, c.want.field)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			prepared := PreparedFeedbackReports{FeedbackReportBatch: normalized, Redactions: make([]core.RedactionStats, 1)}
			plan, err := PlanFeedbackReports(t.Context(), prepared, "watcher", nil, 0, never)
			if err != nil {
				t.Fatal(err)
			}
			report := plan.Changes[0].Report
			if !report.FirstSeen.Equal(c.want.storedFirst) || !report.LastSeen.Equal(c.want.storedLast) || !report.FirstEntrySeen.Equal(c.want.storedFirst) {
				t.Fatalf("stored first=%v last=%v first entry=%v, want %v and %v", report.FirstSeen, report.LastSeen, report.FirstEntrySeen, c.want.storedFirst, c.want.storedLast)
			}
		})
	}

	// The merge and the reopen decide on the exact stated value: a time that
	// differs from a stored time only beyond its stored digits is later.
	stored := time.Date(2026, 11, 25, 11, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name        string
		first, last string
		wantFirst   time.Time
		wantLast    time.Time
		reopened    bool
	}{
		{"equal to the closing time", "2026-11-25T11:00:00Z", "2026-11-25T11:00:00Z", stored.Add(-time.Hour), stored, false},
		{"beyond nanoseconds after the closing time", "2026-11-25T11:00:00Z", "2026-11-25T11:00:00.0000000001Z", stored.Add(-time.Hour), stored, true},
		{"nanoseconds after the closing time", "2026-11-25T11:00:00Z", "2026-11-25T11:00:00.000000001Z", stored.Add(-time.Hour), stored, true},
		{"an earlier first-seen beyond nanoseconds", "2026-11-25T09:59:59.9999999999Z", "2026-11-25T10:59:59Z", stored.Add(-time.Hour - time.Microsecond), stored.Add(-time.Second), false},
		{"a valid offset after the closing time", "2026-11-25T11:00:00Z", "2026-11-25T12:00:00.5+01:00", stored.Add(-time.Hour), stored.Add(500 * time.Millisecond), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			closedAt := stored
			reports := map[string]FeedbackReport{"times": {
				ID: "rpt-261125-000000", EnvironmentID: "env", Key: "times", Count: 1,
				FirstSeen: stored.Add(-time.Hour), LastSeen: stored.Add(-time.Hour), State: FeedbackReportClosed,
				ClosedAt: &closedAt, ClosedBy: "usr-closer", CloseReason: "fixed",
			}}
			entry := FeedbackReportEntry{Kind: FeedbackReportError, Key: "times", Summary: "Times", FirstSeen: parse(c.first), LastSeen: parse(c.last)}
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

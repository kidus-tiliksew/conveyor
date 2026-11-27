package store

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
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
					entry.FirstSeen = &value
				}
				if last != nil {
					value := at(*last)
					entry.LastSeen = &value
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

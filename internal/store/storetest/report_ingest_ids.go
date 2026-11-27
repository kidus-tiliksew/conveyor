package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// reportIDScript is a report ID draw that answers from a script and then, when
// fallback is set, from the random draw. It counts its draws.
type reportIDScript struct {
	mu       sync.Mutex
	script   []string
	fallback bool
	draws    int
}

func (s *reportIDScript) draw(at time.Time) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.draws++
	if len(s.script) > 0 {
		id := s.script[0]
		s.script = s.script[1:]
		return id, nil
	}
	if s.fallback {
		return store.NewFeedbackReportID(at)
	}
	return "", fmt.Errorf("the report ID script is empty after %d draws", s.draws-1)
}

func (s *reportIDScript) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.draws
}

func (s *reportIDScript) context(ctx context.Context) context.Context {
	return store.WithFeedbackReportIDDraw(ctx, s.draw)
}

// requireReportIDsAndEvents checks that the workspace's reports hold distinct
// IDs and that each report has exactly one opened event, which names it.
func requireReportIDsAndEvents(t *testing.T, x Fixture, environments ...feedbackEnvironment) map[string]string {
	t.Helper()
	ids := map[string]string{}
	for _, e := range environments {
		for _, report := range e.reports(t) {
			if !feedbackReportIDPattern.MatchString(report.ID) {
				t.Fatalf("report ID=%q, want rpt-<yymmdd>-<6 hex>", report.ID)
			}
			if other, taken := ids[report.ID]; taken {
				t.Fatalf("reports %s and %s/%s share the ID %s", other, e.id, report.Key, report.ID)
			}
			ids[report.ID] = e.id + "/" + report.Key
		}
	}
	opened := map[string]int{}
	for _, event := range reportEvents(t, x) {
		if event.Kind != store.FeedbackReportOpenedEvent {
			continue
		}
		var payload map[string]string
		requireOK(t, json.Unmarshal(event.Payload, &payload))
		opened[payload["report_id"]]++
	}
	for id, report := range ids {
		if opened[id] != 1 {
			t.Fatalf("report %s (%s) has %d opened events, want 1", id, report, opened[id])
		}
	}
	if len(opened) != len(ids) {
		t.Fatalf("opened events name %v, want exactly the reports %v", opened, ids)
	}
	return ids
}

// runFeedbackReportIDCollisions holds every backend to unique report IDs in
// the workspace with the rpt-<yymmdd>-<6 hex> format. A drawn ID that is
// taken is drawn again, so a valid request is neither refused nor half
// written because of it, and a successful request writes distinct IDs and one
// opened event for each new report (component-feedback-reports invariants 6
// and 20; req-production-feedback AC-4.1).
func runFeedbackReportIDCollisions(t *testing.T, x Fixture) {
	const (
		first  = "rpt-261125-aaaaaa"
		second = "rpt-261125-bbbbbb"
		third  = "rpt-261125-cccccc"
		fourth = "rpt-261125-dddddd"
	)
	production := newFeedbackEnvironment(t, x, "Production", "watcher")
	staging := newFeedbackEnvironment(t, x, "Staging", "watcher")
	record := func(e feedbackEnvironment, script *reportIDScript, arrival time.Duration, entries ...store.FeedbackReportEntry) error {
		return x.Backend.RecordFeedbackReports(script.context(x.Context), e.reportBatch(arrival, entries...))
	}

	// Within one request: the second new report draws the ID the first one
	// took, and draws again.
	batch := &reportIDScript{script: []string{first, first, second}}
	requireOK(t, record(production, batch, 0, reportEntry("batch-one"), reportEntry("batch-two")))
	if got := []string{production.reportByKey(t, "batch-one").ID, production.reportByKey(t, "batch-two").ID}; !slices.Equal(got, []string{first, second}) || batch.count() != 3 {
		t.Fatalf("IDs within one request=%v after %d draws, want %v after 3", got, batch.count(), []string{first, second})
	}
	requireReportIDsAndEvents(t, x, production, staging)

	// Against a stored report: of the same environment, and of another
	// environment of the workspace.
	same := &reportIDScript{script: []string{first, second, third}}
	requireOK(t, record(production, same, time.Second, reportEntry("batch-one"), reportEntry("stored-same")))
	if got := production.reportByKey(t, "stored-same").ID; got != third || same.count() != 3 {
		t.Fatalf("ID against stored reports of the environment=%s after %d draws, want %s after 3", got, same.count(), third)
	}
	other := &reportIDScript{script: []string{second, first, fourth}}
	requireOK(t, record(staging, other, time.Second, reportEntry("stored-other")))
	if got := staging.reportByKey(t, "stored-other").ID; got != fourth || other.count() != 3 {
		t.Fatalf("ID against stored reports of another environment=%s after %d draws, want %s after 3", got, other.count(), fourth)
	}
	requireReportIDsAndEvents(t, x, production, staging)

	// Concurrent requests in two environments draw the same ID first. The
	// writes of the workspace's report IDs are serialized, so the second
	// writer finds it taken and draws again, and both requests succeed.
	const shared = "rpt-261125-eeeeee"
	race := &reportIDScript{script: []string{shared, shared}, fallback: true}
	results := environmentRace(
		func() error { return record(production, race, 2*time.Second, reportEntry("race-production")) },
		func() error { return record(staging, race, 2*time.Second, reportEntry("race-staging")) },
	)
	if environmentWinners(results) != 2 {
		t.Fatalf("concurrent requests with one drawn ID=%v, want both accepted", results)
	}
	raced := []string{production.reportByKey(t, "race-production").ID, staging.reportByKey(t, "race-staging").ID}
	if raced[0] == raced[1] || !slices.Contains(raced, shared) || race.count() < 3 {
		t.Fatalf("concurrent IDs=%v after %d draws, want two distinct IDs, one of them %s, after a draw again", raced, race.count(), shared)
	}
	requireReportIDsAndEvents(t, x, production, staging)

	// A draw that never finds a free ID stops after the bound. The request,
	// whose update is valid, records nothing and can be sent again.
	before := production.reportState(t)
	stuck := &reportIDScript{}
	for range store.MaxFeedbackReportIDDraws {
		stuck.script = append(stuck.script, first)
	}
	err := record(production, stuck, 3*time.Second, reportEntry("batch-one"), reportEntry("never-free"))
	if !errors.Is(err, store.ErrRetryable) || stuck.count() != store.MaxFeedbackReportIDDraws {
		t.Fatalf("a draw with no free ID err=%v after %d draws, want ErrRetryable after %d", err, stuck.count(), store.MaxFeedbackReportIDDraws)
	}
	if after := production.reportState(t); after != before {
		t.Fatalf("a request with no free ID changed the store:\nbefore %s\nafter  %s", before, after)
	}
}

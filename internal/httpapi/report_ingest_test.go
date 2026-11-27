package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/redact"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

const feedbackMinimalReport = `{"reports":[{"kind":"error","key":"checkout-timeout","summary":"Checkout timed out"}]}`

// reportBody wraps entries, each a JSON object's members, in a request.
func reportBody(entries ...string) string {
	objects := make([]string, len(entries))
	for i, entry := range entries {
		objects[i] = "{" + entry + "}"
	}
	return `{"reports":[` + strings.Join(objects, ",") + `]}`
}

// reportEntryJSON is an entry with a kind, a key, and a summary, and more
// members when given.
func reportEntryJSON(key string, more ...string) string {
	return strings.Join(append([]string{`"kind":"error","key":"` + key + `","summary":"Request failed"`}, more...), ",")
}

// sendReport sends one report body under a key value.
func (h *feedbackHarness) sendReport(value, body string) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.do(value, http.MethodPost, environmentReportPath, body)
}

// acceptReport sends one body after the key's report window has passed and
// requires 202 with only the arrival time.
func (h *feedbackHarness) acceptReport(body string) *httptest.ResponseRecorder {
	h.t.Helper()
	h.advance(feedbackSignalWindow)
	response := h.sendReport(h.key.Value, body)
	if response.Code != http.StatusAccepted {
		h.t.Fatalf("report status=%d body=%s, want 202", response.Code, response.Body)
	}
	var answer map[string]json.RawMessage
	decodeEnvironmentBody(h.t, response, &answer)
	var accepted time.Time
	if len(answer) != 1 || json.Unmarshal(answer["accepted_at"], &accepted) != nil || !accepted.Equal(store.EnvironmentTime(h.clock())) {
		h.t.Fatalf("answer=%s, want only accepted_at at %s", response.Body, h.clock())
	}
	return response
}

func (h *feedbackHarness) reports() []store.FeedbackReport {
	h.t.Helper()
	reports, err := h.st.ListEnvironmentFeedbackReports(h.ctx, h.environment.ID)
	if err != nil {
		h.t.Fatal(err)
	}
	return reports
}

func (h *feedbackHarness) reportByKey(key string) store.FeedbackReport {
	h.t.Helper()
	for _, report := range h.reports() {
		if report.Key == key {
			return report
		}
	}
	h.t.Fatalf("no report of key %q", key)
	return store.FeedbackReport{}
}

// reportStored renders everything a refused report request must leave
// unchanged: the signal-side records, every event, and the reports.
func (h *feedbackHarness) reportStored() string {
	h.t.Helper()
	return feedbackJSON(h.t, []any{h.stored(), h.reports()})
}

// requireReportRefusal requires 400 invalid_report naming the field, with no
// submitted value in the answer.
func requireReportRefusal(t *testing.T, response *httptest.ResponseRecorder, field string, submitted ...string) {
	t.Helper()
	var body map[string]string
	decodeEnvironmentBody(t, response, &body)
	if response.Code != http.StatusBadRequest || len(body) != 3 || body["error"] != "invalid_report" || body["field"] != field || body["message"] == "" {
		t.Fatalf("status=%d body=%s, want 400 invalid_report naming %s", response.Code, response.Body, field)
	}
	for _, value := range submitted {
		if value != "" && strings.Contains(response.Body.String(), value) {
			t.Fatalf("refusal %s repeats the submitted value %q", response.Body, value)
		}
	}
}

// TestFeedbackReportAccepted proves the answer and the stored defaults of a
// minimal request (component-feedback-reports invariants 4, 6, and 9;
// req-production-feedback AC-4.1; req-environments AC-2.4).
func TestFeedbackReportAccepted(t *testing.T) {
	t.Parallel()
	h := newFeedbackHarness(t)
	h.acceptReport(feedbackMinimalReport)
	reports := h.reports()
	arrived := store.EnvironmentTime(h.clock())
	if len(reports) != 1 {
		t.Fatalf("reports=%+v", reports)
	}
	got := reports[0]
	if got.Count != 1 || !got.FirstSeen.Equal(arrived) || !got.LastSeen.Equal(arrived) || got.State != store.FeedbackReportOpen || !got.InInbox ||
		got.Origin != (store.FeedbackReportOrigin{Source: store.FeedbackSourceReport, Label: "deploy", Actor: store.SystemActor().ID}) {
		t.Fatalf("report=%+v", got)
	}
	// A later entry under the key adds to the same report.
	h.acceptReport(reportBody(`"kind":"alert","key":"checkout-timeout","summary":"Other text","count":4`))
	if got := h.reportByKey("checkout-timeout"); got.Count != 5 || got.Kind != store.FeedbackReportError || got.Summary != "Checkout timed out" || len(h.reports()) != 1 {
		t.Fatalf("report after a later entry=%+v", got)
	}
}

// TestFeedbackReportBounds covers every field bound of
// component-feedback-reports invariant 3 at the route. A refused request is
// refused whole and records nothing, and its answer repeats no submitted value
// (req-security-boundaries AC-1.5).
func TestFeedbackReportBounds(t *testing.T) {
	t.Parallel()
	h := newFeedbackHarness(t)
	many := func(count int) string {
		entries := make([]string, count)
		for i := range entries {
			entries[i] = reportEntryJSON(fmt.Sprintf("key-%d", i))
		}
		return reportBody(entries...)
	}
	for name, body := range map[string]string{
		"most entries":     many(store.MaxFeedbackReportEntries),
		"each kind":        reportBody(`"kind":"error","key":"a","summary":"s"`, `"kind":"alert","key":"b","summary":"s"`, `"kind":"message","key":"c","summary":"s"`),
		"longest key":      reportBody(`"kind":"error","key":"` + strings.Repeat("é", 200) + `","summary":"s"`),
		"longest summary":  reportBody(`"kind":"error","key":"long-summary","summary":"` + strings.Repeat("é", 1000) + `"`),
		"every field":      reportBody(reportEntryJSON("full", `"service":"`+strings.Repeat("s", 100)+`"`, `"repository":"app"`, `"commit":"`+strings.Repeat("aF", 20)+`"`, `"version":"`+strings.Repeat("v", 200)+`"`, `"count":1000000000`, `"first_seen":"2026-11-25T11:00:00Z"`, `"last_seen":"2026-11-25T12:30:00.5+01:00"`, `"file":"`+strings.Repeat("f", 500)+`"`, `"line":1000000`)),
		"shortest commit":  reportBody(reportEntryJSON("short-commit", `"commit":"abcdef0"`)),
		"least count":      reportBody(reportEntryJSON("one", `"count":1`)),
		"least line":       reportBody(reportEntryJSON("first-line", `"file":"main.go","line":1`)),
		"keys that differ": reportBody(reportEntryJSON("api"), reportEntryJSON("API"), reportEntryJSON("api ")),
		"trailing space":   feedbackMinimalReport + " \n",
		"escaped text":     reportBody(`"kind":"error","key":"🚀","summary":"é\""`),
	} {
		t.Run(name, func(t *testing.T) {
			h.t = t
			h.acceptReport(body)
		})
	}
	h.t = t
	if got := h.reportByKey("full"); got.Count != store.MaxFeedbackReportCount || got.Line != store.MaxFeedbackReportLine || got.First.Repository != "app" {
		t.Fatalf("report of every field=%+v", got)
	}

	h.advance(feedbackSignalWindow)
	before := h.reportStored()
	valid := reportEntryJSON("valid")
	secret := "ghp_0123456789abcdefghij0123456789abcdefghij"
	for _, c := range []struct{ name, field, body, submitted string }{
		{"not an object", "body", `[]`, ""},
		{"two objects", "body", feedbackMinimalReport + feedbackMinimalReport, ""},
		{"invalid UTF-8", "body", `{"reports":[{"kind":"error","key":"` + "\xff" + `","summary":"s"}]}`, ""},
		{"unknown top field", "body", `{"reports":[` + "{" + valid + "}" + `],"workspace":"beta"}`, "beta"},
		{"repeated top field", "body", `{"reports":[],"reports":[` + "{" + valid + "}" + `]}`, ""},
		{"no reports", "reports", `{}`, ""},
		{"null reports", "reports", `{"reports":null}`, ""},
		{"empty reports", "reports", `{"reports":[]}`, ""},
		{"too many reports", "reports", many(store.MaxFeedbackReportEntries + 1), ""},
		{"entry not an object", "reports[1]", `{"reports":[{` + valid + `},"x"]}`, ""},
		{"unknown entry field", "reports[1]", reportBody(valid, reportEntryJSON("other", `"priority":"high"`)), "priority"},
		{"repeated entry field", "reports[1]", reportBody(valid, reportEntryJSON("other", `"key":"again"`)), "again"},
		{"no kind", "reports[1].kind", reportBody(valid, `"key":"other","summary":"s"`), ""},
		{"no key", "reports[1].key", reportBody(valid, `"kind":"error","summary":"s"`), ""},
		{"no summary", "reports[1].summary", reportBody(valid, `"kind":"error","key":"other"`), ""},
		{"null kind", "reports[1].kind", reportBody(valid, `"kind":null,"key":"other","summary":"s"`), ""},
		{"empty key", "reports[1].key", reportBody(valid, `"kind":"error","key":"","summary":"s"`), ""},
		{"unknown kind", "reports[1].kind", reportBody(valid, `"kind":"panic","key":"other","summary":"s"`), "panic"},
		{"long key", "reports[1].key", reportBody(valid, reportEntryJSON(strings.Repeat("é", 201))), strings.Repeat("é", 201)},
		{"repeated key", "reports[1].key", reportBody(valid, valid), ""},
		{"empty summary", "reports[1].summary", reportBody(valid, `"kind":"error","key":"other","summary":""`), ""},
		{"long summary", "reports[1].summary", reportBody(valid, `"kind":"error","key":"other","summary":"`+strings.Repeat("é", 1001)+`"`), strings.Repeat("é", 1001)},
		{"null service", "reports[1].service", reportBody(valid, reportEntryJSON("other", `"service":null`)), ""},
		{"empty service", "reports[1].service", reportBody(valid, reportEntryJSON("other", `"service":""`)), ""},
		{"long service", "reports[1].service", reportBody(valid, reportEntryJSON("other", `"service":"`+strings.Repeat("s", 101)+`"`)), strings.Repeat("s", 101)},
		{"long version", "reports[1].version", reportBody(valid, reportEntryJSON("other", `"version":"`+strings.Repeat("v", 201)+`"`)), strings.Repeat("v", 201)},
		{"short commit", "reports[1].commit", reportBody(valid, reportEntryJSON("other", `"commit":"abcdef"`)), "abcdef"},
		{"long commit", "reports[1].commit", reportBody(valid, reportEntryJSON("other", `"commit":"`+strings.Repeat("a", 41)+`"`)), strings.Repeat("a", 41)},
		{"commit not hexadecimal", "reports[1].commit", reportBody(valid, reportEntryJSON("other", `"commit":"abcdefg"`)), "abcdefg"},
		{"unknown repository", "reports[1].repository", reportBody(valid, reportEntryJSON("other", `"repository":"ghost"`)), "ghost"},
		{"repository in another case", "reports[1].repository", reportBody(valid, reportEntryJSON("other", `"repository":"App"`)), "App"},
		{"repository of another workspace", "reports[1].repository", reportBody(valid, reportEntryJSON("other", `"repository":"elsewhere"`)), "elsewhere"},
		{"zero count", "reports[1].count", reportBody(valid, reportEntryJSON("other", `"count":0`)), ""},
		{"high count", "reports[1].count", reportBody(valid, reportEntryJSON("other", `"count":1000000001`)), "1000000001"},
		{"fractional count", "reports[1].count", reportBody(valid, reportEntryJSON("other", `"count":1.5`)), ""},
		{"text count", "reports[1].count", reportBody(valid, reportEntryJSON("other", `"count":"2"`)), ""},
		{"null count", "reports[1].count", reportBody(valid, reportEntryJSON("other", `"count":null`)), ""},
		{"file without line", "reports[1].file", reportBody(valid, reportEntryJSON("other", `"file":"main.go"`)), "main.go"},
		{"line without file", "reports[1].file", reportBody(valid, reportEntryJSON("other", `"line":3`)), ""},
		{"zero line", "reports[1].line", reportBody(valid, reportEntryJSON("other", `"file":"main.go","line":0`)), "main.go"},
		{"high line", "reports[1].line", reportBody(valid, reportEntryJSON("other", `"file":"main.go","line":1000001`)), "1000001"},
		{"long file", "reports[1].file", reportBody(valid, reportEntryJSON("other", `"file":"`+strings.Repeat("f", 501)+`","line":1`)), strings.Repeat("f", 501)},
		{"time not RFC 3339", "reports[1].first_seen", reportBody(valid, reportEntryJSON("other", `"first_seen":"yesterday"`)), "yesterday"},
		{"time without zone", "reports[1].last_seen", reportBody(valid, reportEntryJSON("other", `"last_seen":"2026-11-25T11:00:00"`)), ""},
		{"null time", "reports[1].first_seen", reportBody(valid, reportEntryJSON("other", `"first_seen":null`)), ""},
		{"first after last", "reports[1].first_seen", reportBody(valid, reportEntryJSON("other", `"first_seen":"2026-11-25T11:00:00Z","last_seen":"2026-11-25T10:00:00Z"`)), ""},
		{"credential key", "reports[1].key", reportBody(valid, reportEntryJSON(secret)), secret},
		{"credential service", "reports[1].service", reportBody(valid, reportEntryJSON("other", `"service":"`+secret+`"`)), secret},
		{"credential version", "reports[1].version", reportBody(valid, reportEntryJSON("other", `"version":"`+secret+`"`)), secret},
		{"credential file", "reports[1].file", reportBody(valid, reportEntryJSON("other", `"file":"`+secret+`","line":1`)), secret},
	} {
		t.Run(c.name, func(t *testing.T) {
			h.t = t
			requireReportRefusal(t, h.sendReport(h.key.Value, c.body), c.field, c.submitted)
		})
	}
	h.t = t
	if after := h.reportStored(); after != before {
		t.Fatalf("a refused request changed a record:\nbefore %s\nafter  %s", before, after)
	}
	// Each refusal started no window, so a valid request at the same time
	// is accepted.
	if response := h.sendReport(h.key.Value, reportBody(valid)); response.Code != http.StatusAccepted {
		t.Fatalf("valid request after refusals: status=%d body=%s", response.Code, response.Body)
	}
}

// TestFeedbackReportTimes covers component-feedback-reports invariant 4: an
// omitted time is the arrival, a later time is taken as the arrival, and the
// arrival is read before the key check.
func TestFeedbackReportTimes(t *testing.T) {
	t.Parallel()
	h := newFeedbackHarness(t)
	arrival := h.clock().Add(feedbackSignalWindow)
	h.acceptReport(reportBody(
		reportEntryJSON("omitted"),
		reportEntryJSON("future", `"first_seen":"2030-01-01T00:00:00Z","last_seen":"2030-01-02T00:00:00Z"`),
		reportEntryJSON("past", `"first_seen":"2026-11-25T10:00:00.000001Z","last_seen":"2026-11-25T13:00:00+02:00"`),
		reportEntryJSON("only-first", `"first_seen":"2026-11-25T10:00:00Z"`),
	))
	at := store.EnvironmentTime(arrival)
	for key, want := range map[string][2]time.Time{
		"omitted":    {at, at},
		"future":     {at, at},
		"past":       {time.Date(2026, 11, 25, 10, 0, 0, 1000, time.UTC), time.Date(2026, 11, 25, 11, 0, 0, 0, time.UTC)},
		"only-first": {time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC), at},
	} {
		got := h.reportByKey(key)
		if !got.FirstSeen.Equal(want[0]) || !got.LastSeen.Equal(want[1]) || !got.FirstEntrySeen.Equal(want[0]) {
			t.Fatalf("%s: first=%v last=%v, want %v", key, got.FirstSeen, got.LastSeen, want)
		}
	}
	// A first-seen later than the last-seen is refused. A future first-seen
	// is taken as the arrival first, so it is refused only against a later
	// last-seen, which is taken as the arrival too.
	h.advance(feedbackSignalWindow)
	requireReportRefusal(t, h.sendReport(h.key.Value, reportBody(reportEntryJSON("order", `"first_seen":"2026-11-25T12:00:05Z","last_seen":"2026-11-25T12:00:01Z"`))), "reports[0].first_seen")
	h.acceptReport(reportBody(reportEntryJSON("both-future", `"first_seen":"2030-01-02T00:00:00Z","last_seen":"2030-01-01T00:00:00Z"`)))
}

// TestFeedbackReportTimesWithinOneMicrosecond proves that the route keeps the
// stated precision of each time until the store has checked it: a reversed
// pair within one microsecond is refused, an ordered pair within one
// microsecond is accepted, and the store receives each time exactly as
// stated, so its reopen check compares the exact last-seen time
// (component-feedback-reports invariants 4 and 7; req-production-feedback
// AC-4.1, AC-4.4).
func TestFeedbackReportTimesWithinOneMicrosecond(t *testing.T) {
	t.Parallel()
	h := newFeedbackHarness(t)
	h.advance(feedbackSignalWindow)
	requireReportRefusal(t, h.sendReport(h.key.Value, reportBody(reportEntryJSON("reversed", `"first_seen":"2026-11-25T11:00:00.0000005Z","last_seen":"2026-11-25T11:00:00.0000001Z"`))), "reports[0].first_seen")
	if reports := h.reports(); len(reports) != 0 {
		t.Fatalf("a refused request stored %+v", reports)
	}
	var received []store.FeedbackReportEntry
	h.use(feedbackReportStore{Backend: h.st, record: func(ctx context.Context, batch store.FeedbackReportBatch) error {
		received = batch.Entries
		return h.st.RecordFeedbackReports(ctx, batch)
	}})
	h.acceptReport(reportBody(reportEntryJSON("ordered", `"first_seen":"2026-11-25T11:00:00.0000001Z","last_seen":"2026-11-25T13:00:00.0000005+02:00"`)))
	first, last := time.Date(2026, 11, 25, 11, 0, 0, 100, time.UTC), time.Date(2026, 11, 25, 11, 0, 0, 500, time.UTC)
	if len(received) != 1 || received[0].FirstSeen == nil || received[0].LastSeen == nil || !received[0].FirstSeen.Equal(first) || !received[0].LastSeen.Equal(last) {
		t.Fatalf("the store received %+v, want first-seen %v and last-seen %v exactly", received, first, last)
	}
	stored := h.reportByKey("ordered")
	at := time.Date(2026, 11, 25, 11, 0, 0, 0, time.UTC)
	if !stored.FirstSeen.Equal(at) || !stored.LastSeen.Equal(at) {
		t.Fatalf("stored first=%v last=%v, want both at the microsecond %v", stored.FirstSeen, stored.LastSeen, at)
	}
}

// TestFeedbackReportReversedPairInArrivalMicrosecond is the reviewer's case:
// the server captures the arrival at T+900 ns, and first-seen T+700 ns and
// last-seen T+200 ns both fall before it. The route clamps and checks them on
// the exact arrival, so it refuses the reversed pair, and it accepts the same
// pair in order (component-feedback-reports invariant 4;
// req-production-feedback AC-4.1).
func TestFeedbackReportReversedPairInArrivalMicrosecond(t *testing.T) {
	t.Parallel()
	h := newFeedbackHarness(t)
	whole := time.Date(2026, 11, 25, 12, 0, 10, 123456000, time.UTC)
	h.mu.Lock()
	h.now = whole.Add(900 * time.Nanosecond)
	h.mu.Unlock()
	reversed := reportBody(reportEntryJSON("reversed", `"first_seen":"2026-11-25T12:00:10.1234567Z","last_seen":"2026-11-25T12:00:10.1234562Z"`))
	requireReportRefusal(t, h.sendReport(h.key.Value, reversed), "reports[0].first_seen")
	if reports := h.reports(); len(reports) != 0 {
		t.Fatalf("a refused request stored %+v", reports)
	}
	ordered := reportBody(reportEntryJSON("ordered", `"first_seen":"2026-11-25T12:00:10.1234562Z","last_seen":"2026-11-25T12:00:10.1234567Z"`))
	response := h.sendReport(h.key.Value, ordered)
	var answer map[string]time.Time
	decodeEnvironmentBody(t, response, &answer)
	if response.Code != http.StatusAccepted || !answer["accepted_at"].Equal(whole) {
		t.Fatalf("ordered pair: status=%d body=%s, want 202 with the arrival at %v", response.Code, response.Body, whole)
	}
	if got := h.reportByKey("ordered"); !got.FirstSeen.Equal(whole) || !got.LastSeen.Equal(whole) || !got.CreatedAt.Equal(whole) {
		t.Fatalf("stored first=%v last=%v created=%v, want each cut to %v", got.FirstSeen, got.LastSeen, got.CreatedAt, whole)
	}
}

// TestFeedbackReportStrictTimes covers the time rules of
// component-feedback-reports v2 invariant 4 at the route. Each refusal is 400
// invalid_report on the field with a fixed message that repeats no submitted
// value, and it refuses the whole request: an RFC 3339 grammar error, a
// second of 60, more than nine fraction digits, and an instant before
// 1970-01-01T00:00:00Z, also through an offset. Both cases of T and Z, nine
// fraction digits, the floor through either offset sign, and a year 9999 time
// whose offset puts it later than the arrival are accepted. No case answers
// 500 (req-production-feedback AC-4.1).
func TestFeedbackReportStrictTimes(t *testing.T) {
	t.Parallel()
	h := newFeedbackHarness(t)
	valid := reportEntryJSON("valid")
	for _, c := range []struct{ name, field, message, members string }{
		{"offset hour 24", "reports[1].first_seen", store.FeedbackReportTimeInvalid, `"first_seen":"2026-11-25T11:00:00+24:00"`},
		{"offset minute 60", "reports[1].last_seen", store.FeedbackReportTimeInvalid, `"last_seen":"2026-11-25T11:00:00+01:60"`},
		{"offset without a colon", "reports[1].first_seen", store.FeedbackReportTimeInvalid, `"first_seen":"2026-11-25T11:00:00+0100"`},
		{"a day the month does not hold", "reports[1].first_seen", store.FeedbackReportTimeInvalid, `"first_seen":"2026-02-30T11:00:00Z"`},
		{"second 60", "reports[1].first_seen", store.FeedbackReportTimeLeapSecond, `"first_seen":"2016-12-31T23:59:60Z"`},
		{"second 60 through an offset", "reports[1].last_seen", store.FeedbackReportTimeLeapSecond, `"last_seen":"2016-12-31T15:59:60-08:00"`},
		{"ten fraction digits", "reports[1].first_seen", store.FeedbackReportTimeTooPrecise, `"first_seen":"2026-11-25T11:00:00.1234567899Z"`},
		{"before the floor", "reports[1].first_seen", store.FeedbackReportTimeBeforeEpoch, `"first_seen":"1969-12-31T23:59:59.999999999Z"`},
		{"before the floor through an offset", "reports[1].last_seen", store.FeedbackReportTimeBeforeEpoch, `"last_seen":"1970-01-01T00:59:59+01:00"`},
		{"reversed one nanosecond apart", "reports[1].first_seen", "must not be later than last_seen", `"first_seen":"2026-11-25T11:00:00.000000002Z","last_seen":"2026-11-25T11:00:00.000000001Z"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			h.t = t
			h.advance(feedbackSignalWindow)
			response := h.sendReport(h.key.Value, reportBody(valid, reportEntryJSON("strict", c.members)))
			requireReportRefusal(t, response, c.field, "2026-11-25", "2016-12-31", "1969-12-31", "1970-01-01T00:59")
			var body map[string]string
			decodeEnvironmentBody(t, response, &body)
			if body["message"] != c.message {
				t.Fatalf("message=%q, want %q", body["message"], c.message)
			}
		})
	}
	h.t = t
	if reports := h.reports(); len(reports) != 0 {
		t.Fatalf("refused requests stored %+v", reports)
	}
	h.acceptReport(reportBody(
		reportEntryJSON("uppercase", `"first_seen":"2026-11-25T11:00:00Z"`),
		reportEntryJSON("lowercase", `"first_seen":"2026-11-25t11:00:00z"`),
		reportEntryJSON("nine-digits", `"first_seen":"2026-11-25T11:00:00.123456789Z","last_seen":"2026-11-25T11:00:00.123456790Z"`),
		reportEntryJSON("offset", `"first_seen":"2026-11-25T13:00:00+02:00","last_seen":"2026-11-25T11:30:00-00:30"`),
		reportEntryJSON("floor", `"first_seen":"1970-01-01T00:00:00Z","last_seen":"1970-01-01T01:00:00+01:00"`),
		reportEntryJSON("floor-negative-offset", `"first_seen":"1969-12-31T23:00:00-01:00"`),
		reportEntryJSON("above", `"first_seen":"9999-12-31T23:30:00-01:00"`),
	))
	arrived := store.EnvironmentTime(h.clock())
	floor := time.Unix(0, 0).UTC()
	for key, want := range map[string][2]time.Time{
		"uppercase":             {time.Date(2026, 11, 25, 11, 0, 0, 0, time.UTC), arrived},
		"lowercase":             {time.Date(2026, 11, 25, 11, 0, 0, 0, time.UTC), arrived},
		"nine-digits":           {time.Date(2026, 11, 25, 11, 0, 0, 123456000, time.UTC), time.Date(2026, 11, 25, 11, 0, 0, 123456000, time.UTC)},
		"offset":                {time.Date(2026, 11, 25, 11, 0, 0, 0, time.UTC), time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)},
		"floor":                 {floor, floor},
		"floor-negative-offset": {floor, arrived},
		"above":                 {arrived, arrived},
	} {
		got := h.reportByKey(key)
		if !got.FirstSeen.Equal(want[0]) || !got.LastSeen.Equal(want[1]) || !got.FirstEntrySeen.Equal(want[0]) {
			t.Fatalf("%s: first=%v last=%v first entry=%v, want %v and %v", key, got.FirstSeen, got.LastSeen, got.FirstEntrySeen, want[0], want[1])
		}
	}
}

// TestFeedbackReportRedaction covers component-feedback-reports invariant 5:
// a summary is redacted before it is stored and its removals are counted by
// class (req-security-boundaries REQ-3, AC-3.1).
func TestFeedbackReportRedaction(t *testing.T) {
	t.Parallel()
	h := newFeedbackHarness(t)
	summary := "deploy failed with token ghp_0123456789abcdefghij0123456789abcdefghij and " + h.key.Value
	clean, stats := redact.New(nil).Redact(summary)
	if stats.Total() == 0 {
		t.Fatal("the fixture secret is not one the redactor removes")
	}
	body := feedbackJSON(t, map[string]any{"reports": []map[string]any{{"kind": "error", "key": "leak", "summary": summary}}})
	h.acceptReport(body)
	got := h.reportByKey("leak")
	if got.Summary != clean || got.SummaryRedactions != stats || strings.Contains(got.Summary, "ghp_") {
		t.Fatalf("stored summary=%q redactions=%+v, want %q with %+v", got.Summary, got.SummaryRedactions, clean, stats)
	}
}

// TestFeedbackReportBodyLimit proves the 256 KiB bound with 413.
func TestFeedbackReportBodyLimit(t *testing.T) {
	t.Parallel()
	h := newFeedbackHarness(t)
	fit := feedbackMinimalReport + strings.Repeat(" ", feedbackReportBodyLimit-len(feedbackMinimalReport))
	over := fit + " "
	send := func(body io.Reader, length int64) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, environmentReportPath, body)
		request.ContentLength = length
		request.Header.Set("Authorization", "Bearer "+h.key.Value)
		return h.serve(request)
	}
	before := h.reportStored()
	for name, length := range map[string]int64{"known length": int64(len(over)), "unknown length": -1} {
		if response := send(io.MultiReader(strings.NewReader(over)), length); response.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("%s over the limit: status=%d body=%s", name, response.Code, response.Body)
		}
	}
	if h.reportStored() != before {
		t.Fatal("a body over the limit changed a record")
	}
	for name, length := range map[string]int64{"known length": int64(len(fit)), "unknown length": -1} {
		if response := send(io.MultiReader(strings.NewReader(fit)), length); response.Code != http.StatusAccepted {
			t.Fatalf("%s at the limit: status=%d body=%s", name, response.Code, response.Body)
		}
		h.advance(feedbackSignalWindow)
	}
}

// TestFeedbackReportCredentials proves component-feedback-reports invariant
// 2 and req-security-boundaries AC-1.5: one live key of an unarchived
// environment is the only credential, and every other one gets one answer
// and records nothing.
func TestFeedbackReportCredentials(t *testing.T) {
	t.Parallel()
	h := newFeedbackHarness(t)
	revoked := h.issue(h.environment.ID)
	h.want(http.StatusNoContent, http.MethodDelete, "/v1/environments/"+h.environment.ID+"/keys/"+revoked.ID, "")
	old := h.create(`{"name":"Old"}`)
	archived := h.issue(old.ID)
	h.want(http.StatusOK, http.MethodPatch, "/v1/environments/"+old.ID, `{"archived":true}`)
	before := h.reportStored()
	for name, headers := range map[string][]string{
		"no credential":             nil,
		"operator token":            {"Bearer " + h.manager()},
		"viewer token":              {"Bearer " + h.tokens[core.WorkspaceRoleViewer]},
		"unknown token":             {"Bearer unknown"},
		"bare prefix":               {"Bearer cv_env_"},
		"unknown key":               {"Bearer cv_env_envkey_unknown_unknown"},
		"altered key":               {"Bearer " + h.key.Value + "x"},
		"key ID":                    {"Bearer " + h.key.ID},
		"revoked key":               {"Bearer " + revoked.Value},
		"key of an archived record": {"Bearer " + archived.Value},
		"another scheme":            {"Basic " + h.key.Value},
		"key beside a user token":   {"Bearer " + h.key.Value, "Bearer " + h.manager()},
		"user token beside a key":   {"Bearer " + h.manager(), "Bearer " + h.key.Value},
		"OAuth token":               {"Bearer cv_oat_0123456789abcdef_0123456789abcdef"},
	} {
		for _, cookie := range []bool{false, true} {
			request := httptest.NewRequest(http.MethodPost, "http://conveyor.example"+environmentReportPath, strings.NewReader(feedbackMinimalReport))
			request.Header["Authorization"] = headers
			request.Header.Set("X-Workspace-ID", "alpha")
			if cookie {
				request.Header.Set("X-Conveyor-CSRF", "1")
				request.Header.Set("Origin", oauthTestIssuer)
				request.AddCookie(&http.Cookie{Name: dashboardSessionCookie, Value: h.session})
			}
			response := h.serve(request)
			if response.Code != http.StatusUnauthorized || strings.TrimSpace(response.Body.String()) != "unauthorized" {
				t.Fatalf("%s cookie=%v: status=%d body=%s, want 401", name, cookie, response.Code, response.Body)
			}
		}
	}
	if after := h.reportStored(); after != before {
		t.Fatalf("a refused credential changed a record:\nbefore %s\nafter  %s", before, after)
	}
	// A restored environment's key is admitted again.
	h.want(http.StatusOK, http.MethodPatch, "/v1/environments/"+old.ID, `{"archived":false}`)
	if response := h.sendReport(archived.Value, feedbackMinimalReport); response.Code != http.StatusAccepted {
		t.Fatalf("restored key: status=%d body=%s", response.Code, response.Body)
	}
	// The route takes POST only.
	if response := h.do(h.manager(), http.MethodGet, environmentReportPath, ""); response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET with a user token: status=%d", response.Code)
	}
}

// TestFeedbackReportKeyStaysOnItsTwoRoutes proves that a key admitted on the
// report route is still refused on every other route and method before
// routing (component-environments invariant 15; component-http-api invariant
// 4; req-security-boundaries AC-1.5).
func TestFeedbackReportKeyStaysOnItsTwoRoutes(t *testing.T) {
	t.Parallel()
	h := newFeedbackHarness(t)
	before := h.reportStored()
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/v1/tasks"}, {http.MethodPost, "/v1/tasks"}, {http.MethodGet, "/v1/environments"}, {http.MethodGet, "/v1/inbox"}, {http.MethodPost, "/mcp"},
		{http.MethodGet, environmentReportPath}, {http.MethodHead, environmentReportPath}, {http.MethodPut, environmentReportPath}, {http.MethodPatch, environmentReportPath},
		{http.MethodDelete, environmentReportPath}, {http.MethodOptions, environmentReportPath}, {"post", environmentReportPath},
		{http.MethodPost, "/v1/feedback/reports/"}, {http.MethodPost, "/v1/feedback/reports/more"}, {http.MethodPost, "/v1/feedback/report"}, {http.MethodPost, "/v1/feedback/%72eports"},
		{http.MethodPost, "/v1/feedback%2Freports"}, {http.MethodPost, "//v1/feedback/reports"}, {http.MethodPost, "/V1/feedback/reports"}, {http.MethodPost, "/v1/feedback-reports"},
	} {
		request := httptest.NewRequest(route.method, "http://conveyor.example"+route.path, strings.NewReader(feedbackMinimalReport))
		request.Header.Set("Authorization", "Bearer "+h.key.Value)
		request.Header.Set("X-Workspace-ID", "alpha")
		if response := h.serve(request); response.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s: status=%d body=%s, want 401", route.method, route.path, response.Code, response.Body)
		}
	}
	if after := h.reportStored(); after != before {
		t.Fatalf("a refused route changed a record:\nbefore %s\nafter  %s", before, after)
	}
}

// TestFeedbackReportScopeIsTheKeys proves that no header, query, or body
// chooses the workspace or the environment.
func TestFeedbackReportScopeIsTheKeys(t *testing.T) {
	t.Parallel()
	h := newFeedbackHarness(t)
	other := h.create(`{"name":"Staging"}`)
	request := httptest.NewRequest(http.MethodPost, environmentReportPath+"?workspace=beta&environment_id="+other.ID, strings.NewReader(feedbackMinimalReport))
	request.Header.Set("Authorization", "Bearer "+h.key.Value)
	for _, name := range []string{"X-Workspace-ID", "X-Conveyor-Workspace", "X-Environment-ID"} {
		request.Header.Set(name, "beta")
	}
	if response := h.serve(request); response.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", response.Code, response.Body)
	}
	if len(h.reports()) != 1 {
		t.Fatal("the key's environment holds no report")
	}
	if reports, err := h.st.ListEnvironmentFeedbackReports(h.ctx, other.ID); err != nil || len(reports) != 0 {
		t.Fatalf("reports of another environment=%+v err=%v", reports, err)
	}
}

// TestFeedbackReportRateRule covers component-feedback-reports invariant 10:
// the 5-second rule holds for the report route with a state of its own, so a
// report request and a signal of one key do not delay each other.
func TestFeedbackReportRateRule(t *testing.T) {
	t.Parallel()
	h := newFeedbackHarness(t)
	h.advance(feedbackSignalWindow)
	if response := h.sendReport(h.key.Value, feedbackMinimalReport); response.Code != http.StatusAccepted {
		t.Fatalf("first report: status=%d body=%s", response.Code, response.Body)
	}
	// A signal of the same key at the same time is not held by the report.
	if response := h.signal(h.key.Value, feedbackMinimalSignal); response.Code != http.StatusAccepted {
		t.Fatalf("signal beside a report: status=%d body=%s", response.Code, response.Body)
	}
	h.advance(2 * time.Second)
	response := h.sendReport(h.key.Value, feedbackMinimalReport)
	retry, err := strconv.Atoi(response.Header().Get("Retry-After"))
	if response.Code != http.StatusTooManyRequests || err != nil || retry < 1 || retry > 5 {
		t.Fatalf("second report within the window: status=%d Retry-After=%q, want 429 with 1 to 5", response.Code, response.Header().Get("Retry-After"))
	}
	// The signal window is its own too: a second signal inside it is
	// refused, which leaves the report state as it is.
	if response := h.signal(h.key.Value, feedbackMinimalSignal); response.Code != http.StatusTooManyRequests {
		t.Fatalf("second signal within its window: status=%d", response.Code)
	}
	// Another key has its own window.
	second := h.issue(h.environment.ID)
	if response := h.sendReport(second.Value, reportBody(reportEntryJSON("second-key"))); response.Code != http.StatusAccepted {
		t.Fatalf("report of another key: status=%d body=%s", response.Code, response.Body)
	}
	h.advance(3 * time.Second)
	if response := h.sendReport(h.key.Value, feedbackMinimalReport); response.Code != http.StatusAccepted {
		t.Fatalf("report after the window: status=%d body=%s", response.Code, response.Body)
	}
	if got := h.reportByKey("checkout-timeout"); got.Count != 2 {
		t.Fatalf("count=%d, want only the two accepted requests", got.Count)
	}
}

// TestFeedbackReportLimiterFull answers 503 when the report rule can hold no
// further key, and the signal rule keeps its own room.
func TestFeedbackReportLimiterFull(t *testing.T) {
	t.Parallel()
	h := newFeedbackHarness(t)
	h.server.feedbackReportLimiter = newFeedbackLimiter(1)
	second := h.issue(h.environment.ID)
	h.acceptReport(feedbackMinimalReport)
	before := h.reportStored()
	response := h.sendReport(second.Value, feedbackMinimalReport)
	if response.Code != http.StatusServiceUnavailable || strings.TrimSpace(response.Body.String()) != "temporarily unavailable" || h.reportStored() != before {
		t.Fatalf("full limiter: status=%d body=%s", response.Code, response.Body)
	}
	if response := h.signal(second.Value, feedbackMinimalSignal); response.Code != http.StatusAccepted {
		t.Fatalf("signal of the second key: status=%d body=%s", response.Code, response.Body)
	}
	h.advance(feedbackSignalWindow)
	if response := h.sendReport(second.Value, feedbackMinimalReport); response.Code != http.StatusAccepted {
		t.Fatalf("after the window: status=%d body=%s", response.Code, response.Body)
	}
}

// TestFeedbackReportOpenLimit answers 409 open_report_limit for a request
// that would store a new report beyond 200 open ones, records nothing, and
// starts no window (component-feedback-reports invariant 8).
func TestFeedbackReportOpenLimit(t *testing.T) {
	t.Parallel()
	h := newFeedbackHarness(t)
	for batch := range store.MaxOpenFeedbackReports / store.MaxFeedbackReportEntries {
		entries := make([]string, store.MaxFeedbackReportEntries)
		for i := range entries {
			entries[i] = reportEntryJSON(fmt.Sprintf("fill-%d-%d", batch, i))
		}
		h.acceptReport(reportBody(entries...))
	}
	h.advance(feedbackSignalWindow)
	before := h.reportStored()
	response := h.sendReport(h.key.Value, reportBody(reportEntryJSON("fill-0-0"), reportEntryJSON("one-too-many")))
	var body map[string]string
	decodeEnvironmentBody(t, response, &body)
	if response.Code != http.StatusConflict || body["error"] != "open_report_limit" || !strings.Contains(body["message"], "beyond the limit of 200 open reports") || len(body) != 2 || strings.Contains(response.Body.String(), "one-too-many") {
		t.Fatalf("status=%d body=%s, want 409 open_report_limit", response.Code, response.Body)
	}
	if after := h.reportStored(); after != before {
		t.Fatalf("a refused request changed a record:\nbefore %s\nafter  %s", before, after)
	}
	// The refusal started no window, and a request of updates alone is
	// accepted at the limit.
	if response := h.sendReport(h.key.Value, reportBody(reportEntryJSON("fill-0-0"))); response.Code != http.StatusAccepted {
		t.Fatalf("an update at the limit: status=%d body=%s", response.Code, response.Body)
	}
}

// feedbackReportStore replaces the report write when it is set.
type feedbackReportStore struct {
	store.Backend
	record func(context.Context, store.FeedbackReportBatch) error
}

func (f feedbackReportStore) RecordFeedbackReports(ctx context.Context, batch store.FeedbackReportBatch) error {
	if f.record == nil {
		return f.Backend.RecordFeedbackReports(ctx, batch)
	}
	return f.record(ctx, batch)
}

// TestFeedbackReportStoreFailures covers the retryable 503, the generic 500,
// and a key that loses its validity after the check. None records anything,
// starts a window, or repeats store text (component-feedback-reports,
// Refusals; DEC-105(1)).
func TestFeedbackReportStoreFailures(t *testing.T) {
	h := newFeedbackHarness(t)
	logs := captureStandardLog(t)
	detail := "driver detail " + h.key.Value
	var failure error
	h.use(feedbackReportStore{Backend: h.st, record: func(context.Context, store.FeedbackReportBatch) error { return failure }})
	before := h.reportStored()
	failure = fmt.Errorf("%s: %w", detail, store.ErrRetryable)
	response := h.sendReport(h.key.Value, feedbackMinimalReport)
	var body map[string]string
	decodeEnvironmentBody(t, response, &body)
	if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") != "1" || !maps.Equal(body, map[string]string{"error": "retryable", "message": storeRetryableMessage}) {
		t.Fatalf("retryable: status=%d headers=%v body=%s", response.Code, response.Header(), response.Body)
	}
	failure = errors.New(detail)
	response = h.sendReport(h.key.Value, feedbackMinimalReport)
	if response.Code != http.StatusInternalServerError || strings.TrimSpace(response.Body.String()) != "internal server error" {
		t.Fatalf("store failure: status=%d body=%s, want the generic 500", response.Code, response.Body)
	}
	failure = store.ErrInvalidEnvironmentKey
	h.requireUnauthorized(h.sendReport(h.key.Value, feedbackMinimalReport), "a key revoked during the request")
	if strings.Contains(logs.String(), h.key.Value) {
		t.Fatal("the log holds the key value")
	}
	if after := h.reportStored(); after != before {
		t.Fatalf("a failed request changed a record:\nbefore %s\nafter  %s", before, after)
	}
	// No failure opened a window, so the request at the same time is
	// accepted.
	h.use(h.st)
	if response := h.sendReport(h.key.Value, feedbackMinimalReport); response.Code != http.StatusAccepted {
		t.Fatalf("resend: status=%d body=%s", response.Code, response.Body)
	}
}

// TestFeedbackReportObservesOnly covers component-feedback-reports invariant
// 1: a report changes no task list, work-order list, next-action list, task
// event, or signal state (req-production-feedback AC-11.1; DEC-149(1)).
func TestFeedbackReportObservesOnly(t *testing.T) {
	t.Parallel()
	h := newFeedbackHarness(t)
	if err := h.st.CreateTask(h.ctx, core.Task{ID: "report-task", Workspace: "alpha", Repo: "app", Title: "Task", State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	h.accept(feedbackService(`"name":"api","repository":"app","versions":[{"commit":"abcdef0"}]`))
	observe := func() string {
		t.Helper()
		var parts []string
		for _, path := range []string{"/v1/tasks", "/v1/tasks/report-task", "/v1/tasks/report-task/events", "/v1/work-orders", "/v1/attention/tasks", "/v1/next-actions"} {
			response := h.want(http.StatusOK, http.MethodGet, path, "")
			parts = append(parts, path+" "+response.Body.String())
		}
		signals, err := h.st.ListFeedbackSignals(h.ctx, []string{h.environment.ID})
		if err != nil {
			t.Fatal(err)
		}
		return feedbackJSON(t, []any{parts, signals, h.deploys()})
	}
	before := observe()
	h.acceptReport(reportBody(reportEntryJSON("one", `"service":"api","repository":"app","commit":"abcdef0","file":"main.go","line":3`)))
	h.acceptReport(reportBody(reportEntryJSON("one"), reportEntryJSON("two")))
	if len(h.reports()) != 2 {
		t.Fatal("the requests recorded no report")
	}
	if after := observe(); after != before {
		t.Fatalf("a report changed an observed read:\nbefore %s\nafter  %s", before, after)
	}
}

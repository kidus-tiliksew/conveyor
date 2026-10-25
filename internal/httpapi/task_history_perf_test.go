package httpapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// Server request durations for the bounded detail view and task history pages
// (task 261015-3721c6; component-http-api "Bounded detail view", "Task history
// pages"; req-operator-next-action AC-6.10, AC-6.11). The boundary is the
// real router and handlers over the memory store behind an httptest server:
// each duration runs from the request's start to the last body byte read,
// with identity encoding, and excludes browser rendering. Opt-in only.

const perfReportEnv = "CONVEYOR_PERF_REPORT"

const (
	perfWarmups = 1
	perfRuns    = 5
)

type perfSample struct {
	DurationMS []float64 `json:"duration_ms"`
	Bytes      []int     `json:"bytes"`
	MedianMS   float64   `json:"median_ms"`
	MinMS      float64   `json:"min_ms"`
	MaxMS      float64   `json:"max_ms"`
	Path       string    `json:"path"`
}

type perfUnavailable struct {
	Measured bool   `json:"measured"`
	Reason   string `json:"reason"`
}

func perfStats(sample *perfSample) {
	sorted := slices.Clone(sample.DurationMS)
	slices.Sort(sorted)
	sample.MinMS, sample.MaxMS = sorted[0], sorted[len(sorted)-1]
	sample.MedianMS = sorted[len(sorted)/2]
}

// perfGet reads path through client and returns the elapsed wall time from
// the request's start to the full body read, and the body.
func perfGet(t *testing.T, client *http.Client, base, path string) (time.Duration, []byte) {
	t.Helper()
	separator := "?"
	if strings.Contains(path, "?") {
		separator = "&"
	}
	request, err := http.NewRequest(http.MethodGet, base+path+separator+"workspace_id="+historyWorkspace, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+historyTokenA)
	runtime.GC()
	start := time.Now()
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	elapsed := time.Since(start)
	_ = response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Encoding") != "" {
		t.Fatalf("GET %s: %d encoding=%q", path, response.StatusCode, response.Header.Get("Content-Encoding"))
	}
	return elapsed, body
}

func TestTaskHistoryServerRequestDurations(t *testing.T) {
	if os.Getenv(perfReportEnv) != "1" {
		t.Skipf("set %s=1 to measure server request durations", perfReportEnv)
	}
	fixtures := []struct {
		taskID                      string
		total, reconciled, bigEvery int
	}{
		{"incident-task", 20_219, 20_127, 9},
		{"large-task", 100_000, 99_400, 50},
	}
	for _, fixture := range fixtures {
		st := store.NewMemory()
		seedHistoryFixture(t, st, fixture.taskID, fixture.total, fixture.reconciled, fixture.bigEvery)
		h := newHistoryHarness(t, st)
		server := httptest.NewServer(h.handler)
		client := &http.Client{Transport: &http.Transport{DisableCompression: true}}

		_, body := perfGet(t, client, server.URL, "/v1/tasks/"+fixture.taskID+"/activity?view=bounded")
		var detail boundedDetailJSON
		if err := json.Unmarshal(body, &detail); err != nil {
			t.Fatal(err)
		}
		paths := []struct{ name, path string }{
			{"complete_activity", "/v1/tasks/" + fixture.taskID + "/activity"},
			{"bounded_activity", "/v1/tasks/" + fixture.taskID + "/activity?view=bounded"},
			{"first_technical_history_page", "/v1/tasks/" + fixture.taskID + "/history"},
			{"first_human_history_page", "/v1/tasks/" + fixture.taskID + "/history?selection=human"},
		}
		report := map[string]any{
			"boundary":         "in-process router and handlers over the memory store via httptest loopback server; request start to full body read; identity encoding; excludes browser rendering",
			"fixture_events":   fixture.total,
			"reconciled":       fixture.reconciled,
			"warmups":          perfWarmups,
			"runs":             perfRuns,
			"gc_before_each":   true,
			"go_version":       runtime.Version(),
			"gomaxprocs":       runtime.GOMAXPROCS(0),
			"bounded_returned": detail.EventPage.Returned,
			"bounded_older":    detail.EventPage.HasOlder,
		}
		if detail.EventPage.OlderCursor != "" {
			paths = append(paths, struct{ name, path string }{"older_human_page_from_bounded_cursor",
				"/v1/tasks/" + fixture.taskID + "/history?selection=human&cursor=" + url.QueryEscape(detail.EventPage.OlderCursor)})
		} else {
			report["older_human_page_from_bounded_cursor"] = perfUnavailable{Reason: fmt.Sprintf("bounded view has_older=false: all %d human events fit its page, so it carries no older_cursor", detail.EventPage.Returned)}
		}
		samples := make([]*perfSample, len(paths))
		for i, entry := range paths {
			samples[i] = &perfSample{Path: entry.path}
			for range perfWarmups {
				perfGet(t, client, server.URL, entry.path)
			}
		}
		for range perfRuns {
			for i, entry := range paths {
				elapsed, body := perfGet(t, client, server.URL, entry.path)
				samples[i].DurationMS = append(samples[i].DurationMS, float64(elapsed.Microseconds())/1000)
				samples[i].Bytes = append(samples[i].Bytes, len(body))
			}
		}
		for i, entry := range paths {
			perfStats(samples[i])
			report[entry.name] = samples[i]
		}
		server.Close()
		line, err := json.Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Printf("PERF-SERVER: %s\n", line)
	}
}

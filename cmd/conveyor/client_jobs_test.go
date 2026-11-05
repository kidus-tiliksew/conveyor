package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// serveJobPages answers GET /v1/tasks/{id}/jobs one page at a time with the
// server's paging headers (DEC-105(4)). stateTotal false models a server that
// serves every job in one response and states no total.
func serveJobPages(t *testing.T, count int, stateTotal bool) (*client, *[]string) {
	t.Helper()
	jobs := make([]core.Job, count)
	for i := range jobs {
		jobs[i] = core.Job{ID: fmt.Sprintf("job-%03d", i)}
	}
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/tasks/task-1/jobs" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		requests = append(requests, r.URL.RawQuery)
		page := jobs
		if stateTotal {
			limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
			offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
			start := min(offset, len(jobs))
			page = jobs[start:min(start+limit, len(jobs))]
			w.Header().Set("X-Conveyor-Total", strconv.Itoa(len(jobs)))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(page)
	}))
	t.Cleanup(server.Close)
	return &client{base: server.URL, token: "t"}, &requests
}

// TestListJobsReadsEveryPage keeps `conveyor task show` listing a task's whole
// job history when it outgrows one page.
func TestListJobsReadsEveryPage(t *testing.T) {
	c, requests := serveJobPages(t, 450, true)
	jobs, err := c.listJobs("task-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 450 || jobs[0].ID != "job-000" || jobs[449].ID != "job-449" {
		t.Fatalf("jobs = %d", len(jobs))
	}
	want := []string{"limit=200&offset=0", "limit=200&offset=200", "limit=200&offset=400"}
	if fmt.Sprint(*requests) != fmt.Sprint(want) {
		t.Fatalf("requests = %v, want %v", *requests, want)
	}
}

func TestListJobsStopsOnAServerThatStatesNoTotal(t *testing.T) {
	c, requests := serveJobPages(t, 3, false)
	jobs, err := c.listJobs("task-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 3 || len(*requests) != 1 {
		t.Fatalf("jobs = %d requests = %v", len(jobs), *requests)
	}
}

func TestListJobsEmpty(t *testing.T) {
	c, requests := serveJobPages(t, 0, true)
	jobs, err := c.listJobs("task-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 || len(*requests) != 1 {
		t.Fatalf("jobs = %d requests = %v", len(jobs), *requests)
	}
}

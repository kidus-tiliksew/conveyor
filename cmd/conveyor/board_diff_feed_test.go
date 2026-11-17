package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func TestBoardDiffActivityPaging(t *testing.T) {
	for _, total := range []int{0, 1, 200, 201, 1000, 1001} {
		t.Run(strconv.Itoa(total), func(t *testing.T) {
			calls := 0
			position := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				q := r.URL.Query()
				if r.URL.Path != "/v1/activity" || q.Get("updated_from") != position.Add(-2*time.Minute).Format(time.RFC3339) || q.Get("limit") != "200" || q.Has("since") || q.Get("offset") != strconv.Itoa(calls*200) {
					t.Errorf("unexpected request %s", r.URL.String())
				}
				calls++
				w.Header().Set("Date", "Fri, 02 Jan 2026 03:05:00 GMT")
				w.Header().Set("X-Conveyor-Total", strconv.Itoa(total))
				n := total - (calls-1)*200
				if n > 200 {
					n = 200
				}
				if n < 0 {
					n = 0
				}
				rows := make([]map[string]any, n)
				for i := range rows {
					rows[i] = map[string]any{"task": map[string]any{"id": fmt.Sprint(i)}}
				}
				_ = json.NewEncoder(w).Encode(rows)
			}))
			defer srv.Close()
			got, err := (&client{base: srv.URL}).readBoardDiffFeed(position)
			if err != nil {
				t.Fatal(err)
			}
			expectedCalls := (total + 199) / 200
			if expectedCalls == 0 {
				expectedCalls = 1
			}
			if total > 1000 {
				expectedCalls = 1
			}
			if calls != expectedCalls || (total > 1000) != (got.ResetReason != "") {
				t.Fatalf("calls %d reason %q", calls, got.ResetReason)
			}
			if total <= 1000 && len(got.Items) != total {
				t.Fatalf("got %d items", len(got.Items))
			}
			if !got.Position.Equal(time.Date(2026, 1, 2, 3, 5, 0, 0, time.UTC)) {
				t.Fatalf("position %s", got.Position)
			}
		})
	}
}

package singlestore

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/httpapi"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// SingleStore reports query time and count, never acquisition latency.
var singleStoreServerTiming = regexp.MustCompile(`^db;dur=\d+\.\d{3}, db_queries;desc="(\d+)"$`)

func TestServerTimingHTTPIntegration(t *testing.T) {
	st := integrationStore(t)
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "server-timing")
	if _, err := st.BootstrapIdentity(ctx, config.FirstOperatorIdentity{OrganizationName: "Timing", Email: "owner@example.test", DisplayName: "Owner"}, "server-timing-token"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: "server-timing"}); err != nil {
		t.Fatal(err)
	}
	server := httpapi.NewServer(st)
	server.Workspaces = st
	server.Workspace = "server-timing"
	handler := server.Handler()
	call := func(token string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
		request.Header.Set("Authorization", "Bearer "+token)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}
	queries := func(recorder *httptest.ResponseRecorder) uint64 {
		t.Helper()
		values := recorder.Result().Header.Values("Server-Timing")
		if len(values) != 1 {
			t.Fatalf("Server-Timing values = %q", values)
		}
		match := singleStoreServerTiming.FindStringSubmatch(values[0])
		if match == nil || strings.Contains(values[0], "db_acquire") {
			t.Fatalf("Server-Timing = %q", values[0])
		}
		count, err := strconv.ParseUint(match[1], 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		return count
	}

	recorder := call("server-timing-token")
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "owner@example.test") {
		t.Fatalf("GET /v1/me = %d %s", recorder.Code, recorder.Body)
	}
	if count := queries(recorder); count == 0 {
		t.Fatal("store-backed request reported no queries")
	}
	// The unchanged route's refusal for an invalid token.
	recorder = call("wrong-token")
	if recorder.Code != http.StatusUnauthorized || recorder.Header().Get("WWW-Authenticate") != "Bearer" || strings.TrimSpace(recorder.Body.String()) != "unauthorized" {
		t.Fatalf("invalid token = %d %q", recorder.Code, recorder.Body)
	}
	queries(recorder)
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if values := recorder.Result().Header.Values("Server-Timing"); recorder.Code != http.StatusOK || len(values) != 0 {
		t.Fatalf("/healthz = %d Server-Timing %q", recorder.Code, values)
	}
}

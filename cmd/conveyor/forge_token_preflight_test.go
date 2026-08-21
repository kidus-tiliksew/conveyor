package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestForgeTokenPreflightUsesStatusOnlyAndNamesRemedy(t *testing.T) {
	configured := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/forge-token" || r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer invoking-user" {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		if !configured {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"configured":true,"forge_login":"owner"}`))
	}))
	defer server.Close()
	c := &client{base: server.URL, workspace: "demo"}
	if err := c.fetchForgeTokenPreflight(t.Context(), "invoking-user"); !errors.Is(err, store.ErrForgeTokenRequired) || err.Error() != store.ForgeTokenRequiredMessage {
		t.Fatalf("missing-token preflight=%v", err)
	}
	configured = true
	if err := c.fetchForgeTokenPreflight(t.Context(), "invoking-user"); err != nil {
		t.Fatalf("configured preflight=%v", err)
	}
}

func TestRunPreflightStopsBeforeOrderLookup(t *testing.T) {
	called := false
	c := &client{token: "invoking-user", forgeTokenPreflight: func(context.Context, string) error {
		called = true
		return store.ErrForgeTokenRequired
	}}
	err := runTaskWithPresentationAndSetup(t.Context(), c, "task", "unused", "", bytes.NewBuffer(nil), &bytes.Buffer{}, true, false, false, false)
	if !called || !errors.Is(err, store.ErrForgeTokenRequired) {
		t.Fatalf("called=%t err=%v", called, err)
	}
}

func TestWorkerPreflightStopsBeforeEnrollment(t *testing.T) {
	t.Setenv("CONVEYOR_CONFIG", writeWorkerLocalExecutionConfig(t, []string{"true", "{prompt}", "{mcp_config}"}, []string{"true"}))
	t.Setenv("CONVEYOR_WORKER_TOKEN", "")
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests++
	}))
	defer server.Close()
	c := &client{
		base:      server.URL,
		token:     "invoking-user",
		workspace: "demo",
		forgeTokenPreflight: func(context.Context, string) error {
			return store.ErrForgeTokenRequired
		},
	}
	err := runWorkerWithPolicy(t.Context(), c, "pairing-code", "worker", true, defaultWorkerReconnectPolicy)
	if !errors.Is(err, store.ErrForgeTokenRequired) || requests != 0 {
		t.Fatalf("worker preflight err=%v requests=%d", err, requests)
	}
}

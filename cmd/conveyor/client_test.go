package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func TestClientSendsBearerTokenOnCreate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/tasks" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret-token" {
			t.Fatalf("Authorization = %q", got)
		}
		_ = json.NewEncoder(w).Encode(core.Task{ID: "task-1"})
	}))
	defer srv.Close()

	c := &client{base: srv.URL, token: "secret-token"}
	if _, err := c.createTask("fix it", "", "api", "main"); err != nil {
		t.Fatal(err)
	}
}

func TestClientRefusesCreateWithoutToken(t *testing.T) {
	c := &client{base: "http://unused"}
	if _, err := c.createTask("fix it", "", "api", "main"); err == nil {
		t.Fatal("expected missing-token error")
	}
}

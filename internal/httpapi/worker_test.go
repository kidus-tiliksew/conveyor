package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

func TestWorkerEnrollmentHeartbeatHealthAndRevocationHTTP(t *testing.T) {
	st := store.NewMemory()
	cfg := &config.Config{Workspace: "demo", Harnesses: []config.Harness{{Name: "codex"}}, Routing: config.Routing{Stages: map[string]config.StageRoute{"implement": {Harness: "codex"}, "review": {Harness: "codex", Execution: config.ExecutionMCP}}}}
	provider := func(context.Context) (*config.Config, error) { return cfg, nil }
	orders := &workorder.Service{Store: st, ConfigProvider: provider}
	workers := &workerservice.Service{Store: st, WorkOrders: orders, ConfigProvider: provider}
	server := NewServer(st)
	server.Workspace = "demo"
	server.BearerToken = "operator"
	server.ConfigProvider = provider
	server.WorkOrders = orders
	server.Workers = workers
	handler := server.Handler()
	call := func(method, path, body, token string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	pairResponse := call(http.MethodPost, "/v1/workers/pairings", `{"ttl_seconds":60}`, "operator")
	if pairResponse.Code != http.StatusCreated {
		t.Fatalf("pair status=%d body=%s", pairResponse.Code, pairResponse.Body.String())
	}
	var pairing struct {
		PairingToken string `json:"pairing_token"`
	}
	_ = json.Unmarshal(pairResponse.Body.Bytes(), &pairing)
	enrollResponse := call(http.MethodPost, "/v1/worker/enroll", `{"pairing_token":"`+pairing.PairingToken+`","name":"laptop"}`, "")
	if enrollResponse.Code != http.StatusCreated {
		t.Fatalf("enroll status=%d body=%s", enrollResponse.Code, enrollResponse.Body.String())
	}
	var enrollment workerservice.Enrollment
	_ = json.Unmarshal(enrollResponse.Body.Bytes(), &enrollment)
	heartbeat := call(http.MethodPost, "/v1/worker/heartbeat", `{"probes":[{"harness":"codex","healthy":true,"checked_at":"`+time.Now().UTC().Format(time.RFC3339)+`"}]}`, enrollment.Credential)
	if heartbeat.Code != http.StatusOK {
		t.Fatalf("heartbeat status=%d body=%s", heartbeat.Code, heartbeat.Body.String())
	}
	list := call(http.MethodGet, "/v1/workers", "", "operator")
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `"auto_available":true`) {
		t.Fatalf("list status=%d body=%s", list.Code, list.Body.String())
	}
	revoke := call(http.MethodDelete, "/v1/workers/"+enrollment.Worker.ID, "", "operator")
	if revoke.Code != http.StatusNoContent {
		t.Fatalf("revoke status=%d body=%s", revoke.Code, revoke.Body.String())
	}
	if response := call(http.MethodPost, "/v1/worker/heartbeat", `{"probes":[]}`, enrollment.Credential); response.Code != http.StatusUnauthorized {
		t.Fatalf("revoked heartbeat status=%d", response.Code)
	}
}

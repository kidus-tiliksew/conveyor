package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestArtifactRepairCLI(t *testing.T) {
	t.Setenv("CONVEYOR_WORKSPACE", "demo")
	t.Setenv("CONVEYOR_API_TOKEN", "repair-token")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "POST" || r.URL.Path != "/v1/artifacts/hash/metadata-repair" || r.Header.Get("X-Workspace-ID") != "demo" || r.Header.Get("Authorization") != "Bearer repair-token" {
			t.Errorf("request: %s %s headers=%v", r.Method, r.URL.Path, r.Header)
		}
		var req store.ArtifactRepairRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if !req.DryRun || req.ExpectedOldContentType != "image/png" || req.NewContentType != "image/jpeg" || req.RequestID != "r1" {
			t.Errorf("payload: %+v", req)
		}
		_ = json.NewEncoder(w).Encode(store.ArtifactRepairResult{ArtifactID: "hash", ValidationStatus: "valid", RequestID: "r1", WouldChange: true})
	}))
	defer srv.Close()
	t.Setenv("CONVEYOR_ADDR", srv.URL)
	oldWorkspace, oldServer := workspaceFlag, serverFlag
	workspaceFlag, serverFlag = "", ""
	t.Cleanup(func() { workspaceFlag, serverFlag = oldWorkspace, oldServer })
	cmd := artifactCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"repair", "hash", "--expected-old-content-type", "image/png", "--content-type", "image/jpeg", "--request-id", "r1", "--dry-run"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || !strings.Contains(out.String(), `"validation_status":"valid"`) {
		t.Fatalf("calls=%d output=%s", calls, &out)
	}
}

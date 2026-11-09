package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
)

// TestConfigImportSendsMergeApprovalForEitherPlanKeySpelling proves config
// import sends the merge_approval its YAML states, false or true, with either
// plan-state key spelling, and the stated plan key, to PUT
// /v1/workspace/config, which resolves both (DEC-122(1); component-runtime
// invariant 39).
func TestConfigImportSendsMergeApprovalForEitherPlanKeySpelling(t *testing.T) {
	for _, planKey := range []string{"default_plan_state: needed", "default_plan_state: not_required", "spec_approval: true", "spec_approval: false"} {
		for _, merge := range []bool{false, true} {
			var sent map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodGet:
					_ = json.NewEncoder(w).Encode(config.VersionedDocument{Document: cliContextualWorkspaceDocument(), Version: 3})
				case http.MethodPut:
					var request struct {
						Document map[string]any `json:"document"`
					}
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Fatal(err)
					}
					sent, _ = request.Document["execution"].(map[string]any)
					_ = json.NewEncoder(w).Encode(config.UpdateReceipt{VersionedDocument: config.VersionedDocument{Version: 4}, EventID: 5})
				}
			}))
			isolateConveyorEnv(t)
			t.Setenv("CONVEYOR_ADDR", srv.URL)
			t.Setenv("CONVEYOR_API_TOKEN", "secret-token")
			previousWorkspace := workspaceFlag
			workspaceFlag = "demo"
			body := "workspace: demo\nmax_bounces: 2\nexecution:\n  " + planKey + "\n  merge_approval: " + map[bool]string{false: "false", true: "true"}[merge] + "\n"
			command := configCmd()
			command.SetArgs([]string{"import", "-"})
			command.SetIn(strings.NewReader(body))
			command.SetOut(&bytes.Buffer{})
			err := command.Execute()
			workspaceFlag = previousWorkspace
			srv.Close()
			if err != nil {
				t.Fatalf("%s merge %t: %v", planKey, merge, err)
			}
			key, value, _ := strings.Cut(planKey, ": ")
			if sent["merge_approval"] != merge || sent[key] == nil || strings.Trim(strings.ToLower(jsonText(sent[key])), `"`) != value {
				t.Fatalf("%s merge %t: sent execution %+v", planKey, merge, sent)
			}
		}
	}
}

func jsonText(value any) string {
	data, _ := json.Marshal(value)
	return string(data)
}

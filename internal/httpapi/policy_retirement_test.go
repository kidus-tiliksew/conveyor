package httpapi

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTaskIntakeRejectsRetiredExecutionFieldsByName(t *testing.T) {
	for _, field := range []string{"setup", "execution_settings", "routing", "harness", "model", "effort", "argv"} {
		var request createTaskReq
		err := json.Unmarshal([]byte(`{"body":"work","repo":"repo","`+field+`":"retired"}`), &request)
		if err == nil || !strings.Contains(err.Error(), field) {
			t.Fatalf("field %s err=%v", field, err)
		}
	}
	var request createTaskReq
	if err := json.Unmarshal([]byte(`{"body":"work","repo":"repo"}`), &request); err != nil {
		t.Fatalf("policy-only intake: %v", err)
	}
}

func TestWorkspacePolicyRejectsNestedExecutionDetailByName(t *testing.T) {
	var payload any
	if err := json.Unmarshal([]byte(`{"document":{"review":{"seats":[{"model":"server-owned"}]}}}`), &payload); err != nil {
		t.Fatal(err)
	}
	if got := forbiddenExecutionField(payload); got != "model" {
		t.Fatalf("forbidden field=%q", got)
	}
}

func TestMCPCreateTaskSchemaHasNoSetupSelector(t *testing.T) {
	tools := mcpTools()
	for _, tool := range tools {
		if tool["name"] != "create_task" {
			continue
		}
		encoded, _ := json.Marshal(tool["inputSchema"])
		if strings.Contains(string(encoded), `"setup"`) || strings.Contains(string(encoded), `"model"`) || strings.Contains(string(encoded), `"harness"`) {
			t.Fatalf("create_task schema carries execution detail: %s", encoded)
		}
		return
	}
	t.Fatal("create_task tool missing")
}

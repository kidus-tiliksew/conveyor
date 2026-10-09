package store

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func TestTaskCreatedPayloadAddsProvenanceOnlyForAgentCredentials(t *testing.T) {
	task := core.Task{ID: "task-1", Workspace: "alpha", Source: "mcp", Title: "Title", Body: "Body", Repo: "api", SpecApproval: true, CreatedAt: time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC),
		Context: core.TaskContext{Requirements: []core.TaskRequirementContext{{ID: "req-a"}}}}
	plain := core.JSONPayload(task)

	for name, ctx := range map[string]func() []byte{
		"no credential": func() []byte { return TaskCreatedPayload(t.Context(), task) },
		"user credential": func() []byte {
			return TaskCreatedPayload(WithCredential(t.Context(), core.AuthenticatedCredential{ID: "pat", OwnerUserID: "usr", Kind: core.CredentialUser}), task)
		},
		"ownerless agent": func() []byte {
			return TaskCreatedPayload(WithCredential(t.Context(), core.AuthenticatedCredential{ID: "agt", Kind: core.CredentialAgent}), task)
		},
		"agent actor without credential": func() []byte {
			return TaskCreatedPayload(WithActor(t.Context(), Actor{ID: AgentActorID("agt"), Role: core.ActorAgent}), task)
		},
	} {
		if got := ctx(); !bytes.Equal(got, plain) {
			t.Fatalf("%s payload changed:\n got %s\nwant %s", name, got, plain)
		}
		if _, ok, err := TaskCreatedProvenance(ctx()); err != nil || ok {
			t.Fatalf("%s provenance ok=%t err=%v", name, ok, err)
		}
	}

	agent := core.AuthenticatedCredential{ID: "agt_1", OwnerUserID: "usr_owner", Kind: core.CredentialAgent, Scope: core.CredentialScopeUser, RunWorkspaceID: "alpha", RunWorkOrderID: "order", RunSessionID: "session"}
	payload := TaskCreatedPayload(WithCredential(t.Context(), agent), task)
	provenance, ok, err := TaskCreatedProvenance(payload)
	if err != nil || !ok || provenance != (TaskTriggerProvenance{AgentCredentialID: "agt_1", OwnerUserID: "usr_owner"}) {
		t.Fatalf("provenance=%+v ok=%t err=%v", provenance, ok, err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatal(err)
	}
	var plainFields map[string]json.RawMessage
	if err := json.Unmarshal(plain, &plainFields); err != nil {
		t.Fatal(err)
	}
	// Every task field stays at the top level with its plain value; the only
	// addition is trigger_provenance, which carries exactly the two IDs.
	if len(fields) != len(plainFields)+1 {
		t.Fatalf("fields=%d plain=%d", len(fields), len(plainFields))
	}
	for key, value := range plainFields {
		if !bytes.Equal(fields[key], value) {
			t.Fatalf("field %s = %s, want %s", key, fields[key], value)
		}
	}
	if string(fields["trigger_provenance"]) != `{"agent_credential_id":"agt_1","owner_user_id":"usr_owner"}` {
		t.Fatalf("trigger_provenance = %s", fields["trigger_provenance"])
	}
	var decoded core.Task
	if err := json.Unmarshal(payload, &decoded); err != nil || decoded.ID != task.ID || decoded.Context.Requirements[0].ID != "req-a" {
		t.Fatalf("decoded=%+v err=%v", decoded, err)
	}
	if _, _, err := TaskCreatedProvenance(json.RawMessage(`not json`)); err == nil {
		t.Fatal("malformed payload decoded")
	}
}

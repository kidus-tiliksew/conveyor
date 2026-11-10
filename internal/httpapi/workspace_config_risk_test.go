package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

type riskConfigFixture struct {
	backend *fakeWorkspaceConfigStore
	members *membershipFixture
	handler http.Handler
}

func newRiskConfigFixture(t *testing.T) *riskConfigFixture {
	t.Helper()
	document := contextualWorkspaceDocument()
	document.Workspace = "alpha"
	backend := &fakeWorkspaceConfigStore{record: config.VersionedDocument{Document: document, Version: 3}}
	members := &membershipFixture{
		workspaces: []core.Workspace{{ID: "alpha"}},
		roles: map[string]map[string]core.WorkspaceRole{
			"viewer": {"alpha": core.WorkspaceRoleViewer}, "contributor": {"alpha": core.WorkspaceRoleContributor},
			"maintainer": {"alpha": core.WorkspaceRoleMaintainer}, "operator": {"alpha": core.WorkspaceRoleOperator},
		},
		// Two people hold operate_gates: the maintainer and the operator. The
		// operator's second listing row is the same person and counts once.
		members: []core.WorkspaceMembership{
			{WorkspaceID: "alpha", UserID: "viewer", Role: core.WorkspaceRoleViewer},
			{WorkspaceID: "alpha", UserID: "contributor", Role: core.WorkspaceRoleContributor},
			{WorkspaceID: "alpha", UserID: "maintainer", Role: core.WorkspaceRoleMaintainer},
			{WorkspaceID: "alpha", UserID: "operator", Role: core.WorkspaceRoleOperator},
			{WorkspaceID: "alpha", UserID: "operator", Role: core.WorkspaceRoleOperator},
		},
	}
	server := NewServer(store.NewMemory())
	server.Workspaces, server.Memberships = members, members
	verifier := staticCredentialVerifier{}
	for user := range members.roles {
		verifier[user+"-token"] = core.AuthenticatedCredential{ID: "pat_" + user, OwnerUserID: user, Kind: core.CredentialUser, Scope: core.CredentialScopeUser}
	}
	server.Credentials = verifier
	server.Deployment = &config.Config{Workspace: "alpha", Database: config.Database{Backend: "memory"}}
	server.ConfigStore = backend
	return &riskConfigFixture{backend: backend, members: members, handler: server.Handler()}
}

func (f *riskConfigFixture) call(t *testing.T, user, method, body, ifMatch string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, "/v1/workspace/config?workspace_id=alpha", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+user+"-token")
	if ifMatch == "" {
		ifMatch = fmt.Sprintf("%d", f.backend.record.Version)
	}
	request.Header.Set("If-Match", ifMatch)
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, request)
	return response
}

// riskBody replaces the document's risk_policy member with raw JSON, so a
// case can send values the Go types cannot hold.
func (f *riskConfigFixture) riskBody(t *testing.T, raw string) string {
	t.Helper()
	data, err := json.Marshal(map[string]any{"document": f.backend.record.Document})
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]map[string]json.RawMessage
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatal(err)
	}
	delete(envelope["document"], "risk_policy")
	if raw != "" {
		envelope["document"]["risk_policy"] = json.RawMessage(raw)
	}
	data, err = json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

type riskConfigResponse struct {
	config.VersionedDocument
	Warnings []configWarning `json:"warnings"`
}

func decodeRiskConfig(t *testing.T, response *httptest.ResponseRecorder) riskConfigResponse {
	t.Helper()
	var decoded riskConfigResponse
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode %s: %v", response.Body, err)
	}
	return decoded
}

// req-risk-based-review REQ-1, AC-1.2, AC-1.3, AC-1.4: only manage_workspace
// saves a risk policy; an invalid table is refused with nothing saved; an
// omitted table saves the defaults; a table needing more human approvals
// than people holding operate_gates saves with a warning naming both numbers.
func TestWorkspaceConfigRiskPolicy(t *testing.T) {
	f := newRiskConfigFixture(t)
	enforce := f.riskBody(t, `{"mode":"enforce"}`)
	for _, user := range []string{"viewer", "contributor", "maintainer"} {
		if write := f.call(t, user, http.MethodPut, enforce, ""); write.Code != http.StatusNotFound || f.backend.updates != 0 {
			t.Fatalf("%s write status=%d updates=%d body=%s", user, write.Code, f.backend.updates, write.Body)
		}
	}
	read := f.call(t, "viewer", http.MethodGet, "", "")
	if read.Code != http.StatusOK || strings.Contains(read.Body.String(), "risk_policy") || strings.Contains(read.Body.String(), "warnings") {
		t.Fatalf("read without a policy status=%d body=%s", read.Code, read.Body)
	}

	for name, raw := range map[string]string{
		"unknown mode":    `{"mode":"strict"}`,
		"missing mode":    `{}`,
		"incomplete":      `{"mode":"record","tiers":{"low":{"agent_reviews":1,"human_approvals":0}}}`,
		"missing value":   `{"mode":"record","tiers":{"low":{"agent_reviews":1},"medium":{"agent_reviews":1,"human_approvals":1},"high":{"agent_reviews":2,"human_approvals":2}}}`,
		"zero agents":     `{"mode":"record","tiers":{"low":{"agent_reviews":0,"human_approvals":0},"medium":{"agent_reviews":1,"human_approvals":1},"high":{"agent_reviews":2,"human_approvals":2}}}`,
		"negative humans": `{"mode":"record","tiers":{"low":{"agent_reviews":1,"human_approvals":-1},"medium":{"agent_reviews":1,"human_approvals":1},"high":{"agent_reviews":2,"human_approvals":2}}}`,
		"decreasing":      `{"mode":"record","tiers":{"low":{"agent_reviews":1,"human_approvals":0},"medium":{"agent_reviews":3,"human_approvals":1},"high":{"agent_reviews":2,"human_approvals":2}}}`,
		"fraction":        `{"mode":"record","tiers":{"low":{"agent_reviews":1.5,"human_approvals":0},"medium":{"agent_reviews":2,"human_approvals":1},"high":{"agent_reviews":2,"human_approvals":2}}}`,
		"text":            `{"mode":"record","tiers":{"low":{"agent_reviews":"1","human_approvals":0},"medium":{"agent_reviews":2,"human_approvals":1},"high":{"agent_reviews":2,"human_approvals":2}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			write := f.call(t, "operator", http.MethodPut, f.riskBody(t, raw), "")
			if write.Code != http.StatusUnprocessableEntity || f.backend.updates != 0 || !strings.Contains(write.Body.String(), `"field":"risk_policy"`) {
				t.Fatalf("status=%d updates=%d body=%s", write.Code, f.backend.updates, write.Body)
			}
		})
	}

	// The default table needs two human approvals at high, and two people
	// hold operate_gates: no warning.
	saved := f.call(t, "operator", http.MethodPut, enforce, "")
	if saved.Code != http.StatusOK || f.backend.updates != 1 {
		t.Fatalf("default save status=%d body=%s", saved.Code, saved.Body)
	}
	receipt := decodeRiskConfig(t, saved)
	policy, err := receipt.Document.RiskPolicy.Resolve()
	if err != nil || policy != (riskpolicy.Policy{Mode: riskpolicy.ModeEnforce, Tiers: riskpolicy.DefaultTable()}) || len(receipt.Warnings) != 0 {
		t.Fatalf("default save policy=%+v err=%v warnings=%+v", policy, err, receipt.Warnings)
	}
	if !strings.Contains(saved.Body.String(), `"risk_policy":{"mode":"enforce","tiers":{"low":{"agent_reviews":1,"human_approvals":0},"medium":{"agent_reviews":1,"human_approvals":1},"high":{"agent_reviews":2,"human_approvals":2}}}`) {
		t.Fatalf("saved document does not state the default table: %s", saved.Body)
	}

	// A stale version saves nothing.
	if stale := f.call(t, "operator", http.MethodPut, enforce, "3"); stale.Code != http.StatusPreconditionFailed || f.backend.updates != 1 {
		t.Fatalf("stale status=%d updates=%d", stale.Code, f.backend.updates)
	}

	demanding := f.riskBody(t, `{"mode":"record","tiers":{"low":{"agent_reviews":1,"human_approvals":0},"medium":{"agent_reviews":2,"human_approvals":1},"high":{"agent_reviews":3,"human_approvals":3}}}`)
	warned := f.call(t, "operator", http.MethodPut, demanding, "")
	if warned.Code != http.StatusOK || f.backend.updates != 2 {
		t.Fatalf("warned save status=%d updates=%d body=%s", warned.Code, f.backend.updates, warned.Body)
	}
	want := configWarning{Code: "risk_policy_human_approvals_unavailable", Field: "risk_policy", Required: 3, Available: 2,
		Message: "The risk policy can require 3 human approvals. People in this workspace who hold operate_gates: 2."}
	if got := decodeRiskConfig(t, warned); len(got.Warnings) != 1 || got.Warnings[0] != want {
		t.Fatalf("save warnings=%+v", got.Warnings)
	}
	if got := decodeRiskConfig(t, f.call(t, "viewer", http.MethodGet, "", "")); len(got.Warnings) != 1 || got.Warnings[0] != want || got.Document.RiskPolicy == nil {
		t.Fatalf("read warnings=%+v policy=%+v", got.Warnings, got.Document.RiskPolicy)
	}

	// Mode off rates nothing and warns nothing.
	off := f.call(t, "operator", http.MethodPut, f.riskBody(t, `{"mode":"off","tiers":{"low":{"agent_reviews":1,"human_approvals":0},"medium":{"agent_reviews":2,"human_approvals":1},"high":{"agent_reviews":3,"human_approvals":3}}}`), "")
	if off.Code != http.StatusOK || len(decodeRiskConfig(t, off).Warnings) != 0 {
		t.Fatalf("off save status=%d body=%s", off.Code, off.Body)
	}

	// A failed membership read saves nothing.
	f.members.membersErr = errors.New("membership store down")
	before := f.backend.updates
	if failed := f.call(t, "operator", http.MethodPut, demanding, ""); failed.Code != http.StatusInternalServerError || f.backend.updates != before || strings.Contains(failed.Body.String(), "membership store down") {
		t.Fatalf("failed count status=%d updates=%d body=%s", failed.Code, f.backend.updates, failed.Body)
	}
}

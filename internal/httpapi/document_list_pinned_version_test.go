package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/insight"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// confirmAfterRowReadStore is the hook between a list route's row read and
// its summary read: it returns the listed rows, then confirms the named
// pending version through the real store before the handler reads the
// summaries. It records the requirement summaries the handler received.
type confirmAfterRowReadStore struct {
	store.Store
	ctx                  context.Context
	requirementID        string
	requirementVersion   int
	designID             string
	designVersion        int
	requirementSummaries map[string][]core.RequirementVersion
}

func (s *confirmAfterRowReadStore) ListRequirements(ctx context.Context, includeArchived bool) ([]core.Requirement, error) {
	rows, err := s.Store.ListRequirements(ctx, includeArchived)
	if err == nil && s.requirementID != "" {
		_, _, err = s.Store.ConfirmRequirementVersion(s.ctx, s.requirementID, s.requirementVersion)
		s.requirementID = ""
	}
	return rows, err
}

func (s *confirmAfterRowReadStore) ListSystemDesigns(ctx context.Context, includeArchived bool) ([]core.SystemDesign, error) {
	rows, err := s.Store.ListSystemDesigns(ctx, includeArchived)
	if err == nil && s.designID != "" {
		_, _, err = s.Store.ConfirmSystemDesignVersion(s.ctx, s.designID, s.designVersion)
		s.designID = ""
	}
	return rows, err
}

func (s *confirmAfterRowReadStore) ListRequirementVersionSummaries(ctx context.Context, selections []store.DocumentVersionSelection) (map[string][]core.RequirementVersion, error) {
	summaries, err := s.Store.ListRequirementVersionSummaries(ctx, selections)
	s.requirementSummaries = summaries
	return summaries, err
}

func pinnedVersionContext(t *testing.T) context.Context {
	t.Helper()
	return store.WithWorkspace(store.WithActor(t.Context(), store.Actor{ID: "pinned-version-operator", Role: core.ActorHuman}), "demo")
}

// seedPinnedRequirement creates a requirement with confirmed v1 and pending
// v2 through store commands, and returns v1.
func seedPinnedRequirement(t *testing.T, ctx context.Context, st store.Store, id string) (core.RequirementVersion, core.RequirementVersion) {
	t.Helper()
	version := func(n string) core.RequirementVersion {
		return core.RequirementVersion{
			Content:    "# " + id + "\n\nVersion " + n + " of the pinned requirement.",
			Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "The list keeps the listed version " + n + "."}},
			Origin:     core.RequirementOriginChat, OriginSessionID: "session-pinned-" + n,
		}
	}
	_, first, err := st.CreateRequirement(ctx, core.Requirement{ID: id, Title: id}, version("1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, id, first.Version); err != nil {
		t.Fatal(err)
	}
	next, err := st.ProposeRequirementVersion(ctx, core.RequirementVersion{RequirementID: id, Content: version("2").Content, Statements: version("2").Statements, Origin: core.RequirementOriginChat, OriginSessionID: "session-pinned-2"})
	if err != nil {
		t.Fatal(err)
	}
	listed, err := st.GetRequirementVersion(ctx, id, first.Version)
	if err != nil {
		t.Fatal(err)
	}
	return listed, next
}

// seedPinnedDesign creates a System Design with confirmed v1 and pending v2
// through store commands. Both versions name requirementID.
func seedPinnedDesign(t *testing.T, ctx context.Context, st store.Store, id, requirementID string) (core.SystemDesignVersion, core.SystemDesignVersion) {
	t.Helper()
	content := func(n string) string {
		return "# " + id + " v" + n + "\n\nServes " + requirementID + ".\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/**\n```"
	}
	_, first, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: id, Title: id, Category: "Component design"}, core.SystemDesignVersion{Content: content("1"), Origin: core.SystemDesignOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmSystemDesignVersion(ctx, id, first.Version); err != nil {
		t.Fatal(err)
	}
	next, err := st.ProposeSystemDesignVersion(ctx, core.SystemDesignVersion{DocumentID: id, Content: content("2"), Origin: core.SystemDesignOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	listed, err := st.GetSystemDesignVersion(ctx, id, first.Version)
	if err != nil {
		t.Fatal(err)
	}
	return listed, next
}

func pinnedVersionGet(t *testing.T, st store.Store, path string) []byte {
	t.Helper()
	server := NewServer(st)
	server.Workspace = "demo"
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, authenticatedMemoryRead(server, httptest.NewRequest(http.MethodGet, path, nil)))
	if response.Code != http.StatusOK {
		t.Fatalf("GET %s status=%d body=%s", path, response.Code, response.Body.String())
	}
	return response.Body.Bytes()
}

// A confirmation between the requirement list's row read and its summary
// read keeps the listed current version and its content
// (component-document-corpus).
func TestRequirementListKeepsListedVersionWhenConfirmedBetweenReads(t *testing.T) {
	ctx := pinnedVersionContext(t)
	base := store.NewMemory()
	listed, next := seedPinnedRequirement(t, ctx, base, "req-pinned-list")
	hook := &confirmAfterRowReadStore{Store: base, ctx: ctx, requirementID: listed.RequirementID, requirementVersion: next.Version}

	var summaries []requirementSummary
	if err := json.Unmarshal(pinnedVersionGet(t, hook, "/v1/requirements"), &summaries); err != nil {
		t.Fatal(err)
	}
	if hook.requirementID != "" {
		t.Fatal("the hook did not confirm the next version between the reads")
	}
	if len(summaries) != 1 || summaries[0].CurrentVersion == nil || summaries[0].CurrentVersion.Version != listed.Version || !summaries[0].CurrentVersion.Confirmed {
		t.Fatalf("requirement list = %+v, want the listed current version v%d", summaries, listed.Version)
	}
	var current *core.RequirementVersion
	for i, version := range hook.requirementSummaries[listed.RequirementID] {
		if version.Version == listed.Version {
			current = &hook.requirementSummaries[listed.RequirementID][i]
		}
	}
	if current == nil || current.Content != listed.Content {
		t.Fatalf("summary of the listed current version = %+v, want its content %q", current, listed.Content)
	}

	// The next list read picks up the version confirmed between the reads.
	if err := json.Unmarshal(pinnedVersionGet(t, hook, "/v1/requirements"), &summaries); err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 || summaries[0].CurrentVersion == nil || summaries[0].CurrentVersion.Version != next.Version {
		t.Fatalf("next requirement list = %+v, want current version v%d", summaries, next.Version)
	}
	for _, version := range hook.requirementSummaries[listed.RequirementID] {
		if version.Version == next.Version && version.Content != next.Content {
			t.Fatalf("next summary of v%d = %+v, want its content %q", next.Version, version, next.Content)
		}
	}
}

// A confirmation between the System Design list's row read and its summary
// read keeps the listed current_version (component-document-corpus).
func TestSystemDesignListKeepsListedVersionWhenConfirmedBetweenReads(t *testing.T) {
	ctx := pinnedVersionContext(t)
	base := store.NewMemory()
	listed, next := seedPinnedDesign(t, ctx, base, "design-pinned-list", "req-unrelated")
	hook := &confirmAfterRowReadStore{Store: base, ctx: ctx, designID: listed.DocumentID, designVersion: next.Version}

	var views []systemDesignSummary
	if err := json.Unmarshal(pinnedVersionGet(t, hook, "/v1/system-designs"), &views); err != nil {
		t.Fatal(err)
	}
	if hook.designID != "" {
		t.Fatal("the hook did not confirm the next version between the reads")
	}
	if len(views) != 1 || views[0].Document.CurrentVersion != listed.Version || views[0].CurrentVersion == nil || views[0].CurrentVersion.Version != listed.Version || !views[0].CurrentVersion.Confirmed {
		t.Fatalf("System Design list = %+v, want current_version v%d", views, listed.Version)
	}

	// The next list read picks up the version confirmed between the reads.
	if err := json.Unmarshal(pinnedVersionGet(t, hook, "/v1/system-designs"), &views); err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].CurrentVersion == nil || views[0].CurrentVersion.Version != next.Version {
		t.Fatalf("next System Design list = %+v, want current_version v%d", views, next.Version)
	}
}

// A confirmation between the requirement insight documents read's design rows
// and its summary read keeps the listed design's current content, so the
// design still names the requirement (component-document-corpus).
func TestRequirementInsightDocumentsKeepListedVersionWhenConfirmedBetweenReads(t *testing.T) {
	ctx := pinnedVersionContext(t)
	base := store.NewMemory()
	requirement, _ := seedPinnedRequirement(t, ctx, base, "req-pinned-insight")
	listed, next := seedPinnedDesign(t, ctx, base, "design-pinned-insight", requirement.RequirementID)
	hook := &confirmAfterRowReadStore{Store: base, ctx: ctx, designID: listed.DocumentID, designVersion: next.Version}

	var result insight.AssociationsResult
	if err := json.Unmarshal(pinnedVersionGet(t, hook, "/v1/requirements/"+requirement.RequirementID+"/insights/documents"), &result); err != nil {
		t.Fatal(err)
	}
	if hook.designID != "" {
		t.Fatal("the hook did not confirm the next version between the reads")
	}
	if len(result.Designs) != 1 || result.Designs[0].ID != listed.DocumentID || result.Designs[0].CurrentVersion != listed.Version || !result.Designs[0].NamesRequirement {
		t.Fatalf("insight designs = %+v, want %s v%d naming the requirement from its listed content", result.Designs, listed.DocumentID, listed.Version)
	}

	// The next read picks up the version confirmed between the reads.
	if err := json.Unmarshal(pinnedVersionGet(t, hook, "/v1/requirements/"+requirement.RequirementID+"/insights/documents"), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Designs) != 1 || result.Designs[0].CurrentVersion != next.Version {
		t.Fatalf("next insight designs = %+v, want %s v%d", result.Designs, listed.DocumentID, next.Version)
	}
}

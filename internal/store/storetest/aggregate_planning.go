package storetest

import (
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func runPlanningReads(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	doc, version, err := st.CreateReferenceDocument(ctx, core.ReferenceDocument{ID: "reference", Name: "Fixture reference"}, core.ReferenceDocumentVersion{Filename: "reference.md", ContentType: "text/markdown", Content: "# Fixture"})
	requireOK(t, err)
	read, err := st.GetReferenceDocument(ctx, doc.ID)
	requireOK(t, err)
	if read.Name != doc.Name {
		t.Fatal("reference document name differs")
	}
	readVersion, err := st.GetReferenceDocumentVersion(ctx, doc.ID, version.Version)
	requireOK(t, err)
	if readVersion.Content != version.Content {
		t.Fatal("reference version bytes differ")
	}
	versions, err := st.ListReferenceDocumentVersions(ctx, doc.ID)
	requireOK(t, err)
	if len(versions) != 1 || versions[0].Version != version.Version {
		t.Fatal("reference version history differs")
	}
	session, err := st.CreatePlanningSession(ctx, core.PlanningSession{ID: "session", Goal: core.PlanningGoalBundle})
	requireOK(t, err)
	artifact, err := st.CreateArtifact(ctx, core.Artifact{Name: "context.txt", ContentType: "text/plain", Role: core.ArtifactRoleTaskContext, PlanningSessionID: session.ID}, []byte("context"))
	requireOK(t, err)
	_, content, err := st.GetArtifactForPlanningSession(ctx, artifact.ID, session.ID)
	requireOK(t, err)
	if string(content) != "context" {
		t.Fatal("session artifact content differs")
	}
	if _, _, err := st.GetArtifactForPlanningSession(ctx, artifact.ID, "foreign"); err == nil {
		t.Fatal("another session read artifact")
	}
	req, revision, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-bundle", Title: "Bundle intent"}, core.RequirementVersion{Content: "Fixture intent.", Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Retain rejected bundle history."}}, Origin: core.RequirementOriginOperator})
	requireOK(t, err)
	bundle, err := st.CreatePlanningBundle(ctx, core.PlanningBundle{ID: "bundle", SessionID: session.ID, Title: "Fixture bundle", Documents: []core.PlanningBundleDocument{{Kind: core.PlanningBundleRequirement, ID: req.ID, Version: revision.Version}}, Tasks: []core.PlanningBundleTask{{MemberID: "one", Title: "One", Body: "Fixture", Repo: "conveyor"}}})
	requireOK(t, err)
	bundle, err = st.RejectPlanningBundle(ctx, bundle.ID)
	requireOK(t, err)
	if bundle.Status != core.PlanningBundleRejected {
		t.Fatal("bundle was not rejected")
	}
	_, err = st.RejectPlanningBundle(ctx, bundle.ID)
	requireOK(t, err)
	if _, err := st.ApprovePlanningBundle(ctx, bundle.ID); err == nil {
		t.Fatal("rejected bundle was approved")
	}
	tasks, err := st.ListTasks(ctx)
	requireOK(t, err)
	if len(tasks) != 0 {
		t.Fatal("rejected bundle materialized tasks")
	}
}

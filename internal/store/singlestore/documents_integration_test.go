package singlestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestSingleStoreDocumentsIntegration(t *testing.T) {
	st := integrationStore(t)
	ctx := store.WithActor(store.WithWorkspace(t.Context(), "documents"), store.Actor{ID: "operator", Role: core.ActorHuman})
	if _, err := st.CreateWorkspace(ctx, "documents", "Documents", &config.Config{Workspace: "documents"}); err != nil {
		t.Fatal(err)
	}
	create := func(id, text string) core.Requirement {
		t.Helper()
		r, v, err := st.CreateRequirement(ctx, core.Requirement{ID: id, Title: id}, core.RequirementVersion{Content: "# " + id + "\n\n" + text, Origin: core.RequirementOriginOperator, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: text}}})
		if err != nil {
			t.Fatal(err)
		}
		got, err := st.GetRequirementVersion(ctx, id, 1)
		if err != nil || got.Content != v.Content || !reflect.DeepEqual(got.Statements, v.Statements) {
			t.Fatalf("version round trip: %v", err)
		}
		if _, _, err = st.ConfirmRequirementVersion(ctx, id, 1); err != nil {
			t.Fatal(err)
		}
		return r
	}
	r := create("req-large", strings.Repeat("Keep the recorded document history. ", 2000))
	a, b := create("req-a", "Keep A."), create("req-b", "Keep B.")
	if err := st.ArchiveRequirement(ctx, r.ID, "operator", []string{b.ID, a.ID, b.ID}); err != nil {
		t.Fatal(err)
	}
	archived, err := st.GetRequirement(ctx, r.ID)
	if err != nil || !reflect.DeepEqual(archived.SupersededBy, []string{b.ID, a.ID}) {
		t.Fatalf("archive order: %+v %v", archived, err)
	}
	if err = st.RestoreRequirement(ctx, r.ID, "operator"); err != nil {
		t.Fatal(err)
	}
	restored, err := st.GetRequirement(ctx, r.ID)
	if err != nil || restored.Archived || len(restored.SupersededBy) != 0 {
		t.Fatalf("restore: %+v %v", restored, err)
	}

	t.Run("large design and immutable versions", func(t *testing.T) {
		content := strings.Repeat("Detailed storage behavior and acceptance evidence.\n", 2000) + "\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/store/**\n```"
		d, v, e := st.CreateSystemDesign(ctx, core.SystemDesign{ID: "design-large", Title: "Large design", Category: "component"}, core.SystemDesignVersion{Content: content, Origin: core.SystemDesignOriginOperator})
		if e != nil {
			t.Fatal(e)
		}
		got, e := st.GetSystemDesignVersion(ctx, d.ID, v.Version)
		if e != nil || got.Content != content {
			t.Fatalf("large design round trip: %v", e)
		}
		if _, _, e = st.ConfirmSystemDesignVersion(ctx, d.ID, v.Version); e != nil {
			t.Fatal(e)
		}
		if _, e = st.ProposeSystemDesignVersion(ctx, core.SystemDesignVersion{DocumentID: d.ID, Content: "A later design.\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/store/**\n```", Origin: core.SystemDesignOriginOperator}); e != nil {
			t.Fatal(e)
		}
		got, e = st.GetSystemDesignVersion(ctx, d.ID, v.Version)
		if e != nil || got.Content != content || !got.Confirmed {
			t.Fatalf("immutable design changed: %v", e)
		}
	})
	t.Run("reference name concurrency", func(t *testing.T) {
		var wg sync.WaitGroup
		results := make(chan error, 2)
		for i, name := range []string{"Operator Notes", "operator notes"} {
			wg.Add(1)
			go func(i int, name string) {
				defer wg.Done()
				_, _, e := st.CreateReferenceDocument(ctx, core.ReferenceDocument{ID: []string{"ref-a", "ref-b"}[i], Name: name}, core.ReferenceDocumentVersion{Content: "notes", Filename: "notes.md", ContentType: "text/markdown"})
				results <- e
			}(i, name)
		}
		wg.Wait()
		close(results)
		success, conflict := 0, 0
		for e := range results {
			if e == nil {
				success++
			} else if errors.Is(e, store.ErrReferenceDocumentNameConflict) {
				conflict++
			} else {
				t.Fatal(e)
			}
		}
		if success != 1 || conflict != 1 {
			t.Fatalf("success=%d conflict=%d", success, conflict)
		}
	})
	t.Run("planning messages and finalization", func(t *testing.T) {
		session, e := st.CreatePlanningSession(ctx, core.PlanningSession{ID: "session-docs", Title: "Document planning", RequirementContextID: r.ID})
		if e != nil {
			t.Fatal(e)
		}
		parts := json.RawMessage(`[{"type":"text","text":"recorded planning"}]`)
		for i := 1; i <= 2; i++ {
			m, e := st.AppendPlanningMessage(ctx, core.PlanningMessage{SessionID: session.ID, Role: "user", Content: "recorded planning", Parts: parts})
			if e != nil || m.Seq != i {
				t.Fatalf("message sequence: %+v %v", m, e)
			}
		}
		messages, e := st.ListPlanningMessages(ctx, session.ID)
		if e != nil || len(messages) != 2 {
			t.Fatalf("messages: %+v %v", messages, e)
		}
		var got, want any
		_ = json.Unmarshal(messages[0].Parts, &got)
		_ = json.Unmarshal(parts, &want)
		if !reflect.DeepEqual(got, want) {
			t.Fatal("planning parts changed")
		}
		request := store.PlanningFinalizeRequest{SessionID: session.ID, RequirementID: r.ID}
		if _, e = st.FinalizePlanningSession(ctx, request); e != nil {
			t.Fatal(e)
		}
		if _, e = st.FinalizePlanningSession(ctx, request); e != nil {
			t.Fatalf("finalize retry: %v", e)
		}
		if _, e = st.AppendPlanningMessage(ctx, core.PlanningMessage{SessionID: session.ID, Role: "user", Content: "late"}); e == nil {
			t.Fatal("terminal session accepted message")
		}
		if _, e = st.ListPlanningSessions(ctx); e != nil {
			t.Fatal(e)
		}
		if _, e = st.ListPendingProposals(ctx); e != nil {
			t.Fatal(e)
		}
	})

	t.Run("event rollback", func(t *testing.T) {
		err := st.documentTx(ctx, func(tx *sql.Tx) error {
			if _, e := tx.ExecContext(ctx, `UPDATE requirements SET title='must roll back' WHERE workspace_id=? AND id=?`, "documents", r.ID); e != nil {
				return e
			}
			return insertWorkspaceEvent(ctx, tx, core.Event{Kind: "requirement.invalid", Payload: json.RawMessage(`{`)})
		})
		if err == nil {
			t.Fatal("invalid audit event committed")
		}
		got, e := st.GetRequirement(ctx, r.ID)
		if e != nil || got.Title != r.Title {
			t.Fatalf("projection escaped rollback: %+v %v", got, e)
		}
	})
	t.Run("pagination and workspace isolation", func(t *testing.T) {
		first, e := st.ListDocumentEventPage(ctx, core.LineageRequirement, r.ID, store.DocumentEventQuery{Limit: 2})
		if e != nil || first.Total < 4 || len(first.Events) != 2 {
			t.Fatalf("page: %+v %v", first, e)
		}
		if _, e = st.ProposeRequirementVersion(ctx, core.RequirementVersion{RequirementID: r.ID, Content: "# Keep a later revision.", Origin: core.RequirementOriginOperator, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Keep a later revision."}}}); e != nil {
			t.Fatal(e)
		}
		pinned, e := st.ListDocumentEventPage(ctx, core.LineageRequirement, r.ID, store.DocumentEventQuery{Limit: 2, Offset: 2, SnapshotID: first.SnapshotID})
		if e != nil || pinned.Total != first.Total {
			t.Fatalf("snapshot changed: %+v %v", pinned, e)
		}
		for _, event := range pinned.Events {
			if event.ID > first.SnapshotID {
				t.Fatal("new event leaked into snapshot")
			}
		}
		other := store.WithWorkspace(ctx, "another-workspace")
		if _, e = st.GetRequirement(other, r.ID); !errors.Is(e, store.ErrNotFound) {
			t.Fatal(e)
		}
		empty, e := st.ListDocumentEventPage(other, core.LineageRequirement, r.ID, store.DocumentEventQuery{Limit: 2})
		if e != nil || empty.Total != 0 {
			t.Fatalf("workspace leak: %+v %v", empty, e)
		}
		if _, e = st.CreatePlanningSession(ctx, core.PlanningSession{ID: "bad-parent", RequirementContextID: "missing"}); !errors.Is(e, store.ErrNotFound) {
			t.Fatalf("missing parent: %v", e)
		}
	})
	t.Run("lineage rebuild and request identity", func(t *testing.T) {
		// Durable historical confirmation cannot prove its predecessor. Two
		// identical reference events deliberately make one ambiguous edge.
		e := st.documentTx(ctx, func(tx *sql.Tx) error {
			for _, event := range []core.Event{
				{Kind: "requirement.version_confirmed", Payload: core.JSONPayload(map[string]any{"requirement_id": "historical", "version": 2})},
				{Kind: "reference_document.created", Payload: core.JSONPayload(map[string]any{"document_id": "historical-reference", "version": 1})},
				{Kind: "reference_document.created", Payload: core.JSONPayload(map[string]any{"document_id": "historical-reference", "version": 1})},
				{Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]any{"number": 99})},
			} {
				if err := insertWorkspaceEvent(ctx, tx, event); err != nil {
					return err
				}
			}
			return nil
		})
		if e != nil {
			t.Fatal(e)
		}
		request := core.LineageRebuildRequest{RequestID: "repeatable", Reason: "integration test"}
		first, e := st.RebuildLineage(ctx, request)
		if e != nil {
			t.Fatal(e)
		}
		if first.PreservedUnregenerable != 1 || first.Ambiguous != 1 || first.Unsupported != 1 {
			t.Fatalf("rebuild counts: %+v", first)
		}
		second, e := st.RebuildLineage(ctx, request)
		if e != nil || !reflect.DeepEqual(first, second) {
			t.Fatalf("rebuild retry: %+v %+v %v", first, second, e)
		}
		request.Reason = "different"
		if _, e = st.RebuildLineage(ctx, request); !errors.Is(e, store.ErrLineageRebuildConflict) {
			t.Fatal(e)
		}
		links, e := st.ListLineageNeighborhood(ctx, []core.LineageNode{{Type: core.LineageReferenceDocument, ID: "historical-reference"}}, core.LineageTraversalBudget{MaxDepth: 3, MaxNodes: 20, MaxLinks: 100})
		if e != nil || len(links) == 0 {
			t.Fatalf("neighborhood: %d %v", len(links), e)
		}
	})
	t.Run("batched lineage records and shard plan", func(t *testing.T) {
		nodes := []core.LineageNode{{Type: core.LineageRequirement, ID: r.ID}, {Type: core.LineageSystemDesign, ID: "design-large"}}
		records, e := st.ListLineageNodeRecords(ctx, nodes)
		if e != nil || records[nodes[0]].Title != r.Title || records[nodes[1]].Title != "Large design" {
			t.Fatalf("labels: %+v %v", records, e)
		}
		contexts, e := st.ListLineageContextRecords(ctx, nodes)
		if e != nil || contexts.Requirements[r.ID].Version != 1 {
			t.Fatalf("contexts: %+v %v", contexts, e)
		}
		artifacts, e := st.ListArtifactsForLineage(ctx, nodes)
		if e != nil || len(artifacts) != 0 {
			t.Fatalf("artifacts: %+v %v", artifacts, e)
		}
		rows, e := st.db.QueryContext(ctx, `EXPLAIN SELECT src_type,src_id,dst_type,dst_id,kind FROM links WHERE workspace_id='documents'`)
		if e != nil {
			t.Fatal(e)
		}
		defer rows.Close()
		var plan []string
		for rows.Next() {
			var line string
			if e = rows.Scan(&line); e != nil {
				t.Fatal(e)
			}
			plan = append(plan, line)
		}
		if e = rows.Err(); e != nil {
			t.Fatal(e)
		}
		t.Logf("workspace lineage plan:\n%s", strings.Join(plan, "\n"))
		if len(plan) == 0 {
			t.Fatal("missing query plan")
		}
	})

	t.Run("nonwaiting planning run", func(t *testing.T) {
		entered, release := make(chan struct{}), make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- st.WithPlanningSessionRun(ctx, "run-lock", func(context.Context) error { close(entered); <-release; return nil })
		}()
		select {
		case <-entered:
		case e := <-done:
			t.Fatal(e)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		err := st.WithPlanningSessionRun(ctx, "run-lock", func(context.Context) error { t.Error("competing callback ran"); return nil })
		close(release)
		if !errors.Is(err, store.ErrPlanningSessionRunConflict) {
			t.Fatalf("competing run=%v", err)
		}
		if err = <-done; err != nil {
			t.Fatal(err)
		}
		func() {
			defer func() {
				if recover() == nil {
					t.Error("callback panic swallowed")
				}
			}()
			_ = st.WithPlanningSessionRun(ctx, "run-lock", func(context.Context) error { panic("test release") })
		}()
		if err = st.WithPlanningSessionRun(ctx, "run-lock", func(context.Context) error { return nil }); err != nil {
			t.Fatalf("lock retained after panic: %v", err)
		}
	})
	t.Run("bundle approval and initial task activity", func(t *testing.T) {
		session, e := st.CreatePlanningSession(ctx, core.PlanningSession{ID: "bundle-session", Title: "Bundle session"})
		if e != nil {
			t.Fatal(e)
		}
		bundle, e := st.CreatePlanningBundle(ctx, core.PlanningBundle{ID: "bundle-docs", SessionID: session.ID, Title: "Document bundle", Documents: []core.PlanningBundleDocument{{Kind: core.PlanningBundleRequirement, ID: r.ID, Version: 2}}, Tasks: []core.PlanningBundleTask{{MemberID: "one", Title: "Implement document", Body: "Implement the documented change.", Repo: "conveyor"}}})
		if e != nil {
			t.Fatal(e)
		}
		approved, e := st.ApprovePlanningBundle(ctx, bundle.ID)
		if e != nil || approved.Status != core.PlanningBundleApproved {
			t.Fatalf("approve: %+v %v", approved, e)
		}
		if _, e = st.ApprovePlanningBundle(ctx, bundle.ID); e != nil {
			t.Fatalf("approve retry: %v", e)
		}
		var count int
		if e = st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks WHERE workspace_id=? AND id=?`, "documents", bundle.Tasks[0].CreatedTaskID).Scan(&count); e != nil || count != 1 {
			t.Fatalf("task identity: %d %v", count, e)
		}
		markers, e := st.ListActivityMarkers(ctx)
		if e != nil || len(markers) != 1 || markers[0].TaskID != bundle.Tasks[0].CreatedTaskID || markers[0].LastEventID == 0 {
			t.Fatalf("activity: %+v %v", markers, e)
		}
		if _, e = st.PendingProposalsProjection(ctx); e != nil {
			t.Fatal(e)
		}
	})

}

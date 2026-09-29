package singlestore

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestDocumentDismissalArchivalMigrationIntegration(t *testing.T) {
	st := integrationStore(t)
	ctx := t.Context()
	var log bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&log, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	for _, ws := range []string{"dismissal-a", "dismissal-b"} {
		scoped := store.WithWorkspace(ctx, ws)
		if _, err := st.BootstrapWorkspaceConfig(scoped, &config.Config{Workspace: ws}); err != nil {
			t.Fatal(err)
		}
		for _, tier := range []string{"requirement", "system_design"} {
			table, versions, key, flag, actor, at := "requirements", "requirement_versions", "requirement_id", "retired", "retired_by", "retired_at"
			if tier == "system_design" {
				table, versions, key, flag, actor, at = "system_designs", "system_design_versions", "document_id", "dismissed", "dismissed_by", "dismissed_at"
			}
			for _, state := range []string{"orphan", "pending", "confirmed", "operator", "empty"} {
				id := tier + "-" + state
				rv := core.RequirementVersion{RequirementID: id, Content: "# Historical proposal", Origin: core.RequirementOriginOperator, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Keep history."}}}
				dv := core.SystemDesignVersion{DocumentID: id, Content: "# Historical proposal\n\n```conveyor:governs\n- repo: conveyor\n  paths: [internal/**]\n```", Origin: core.SystemDesignOriginOperator}
				var err error
				if tier == "requirement" {
					_, _, err = st.CreateRequirement(scoped, core.Requirement{ID: id, Title: id}, rv)
				} else {
					_, _, err = st.CreateSystemDesign(scoped, core.SystemDesign{ID: id, Title: id, Category: "Architecture"}, dv)
				}
				if err != nil {
					t.Fatal(err)
				}
				if state == "empty" {
					if _, err = st.db.ExecContext(ctx, "DELETE FROM "+versions+" WHERE workspace_id=? AND "+key+"=?", ws, id); err != nil {
						t.Fatal(err)
					}
					continue
				}
				if state == "pending" || state == "confirmed" {
					rv.Content = "# Second proposal"
					dv.Content = strings.Replace(dv.Content, "Historical", "Second", 1)
					if tier == "requirement" {
						_, err = st.ProposeRequirementVersion(scoped, rv)
					} else {
						_, err = st.ProposeSystemDesignVersion(scoped, dv)
					}
					if err != nil {
						t.Fatal(err)
					}
				}
				dismissed := 1
				if state == "confirmed" {
					if _, err = st.db.ExecContext(ctx, "UPDATE "+table+" SET current_version=1 WHERE workspace_id=? AND id=?", ws, id); err != nil {
						t.Fatal(err)
					}
					if _, err = st.db.ExecContext(ctx, "UPDATE "+versions+" SET confirmed=true,confirmed_by='user:seed',confirmed_at=now() WHERE workspace_id=? AND "+key+"=? AND version=1", ws, id); err != nil {
						t.Fatal(err)
					}
					dismissed = 2
				}
				if _, err = st.db.ExecContext(ctx, "UPDATE "+versions+" SET "+flag+"=true,"+actor+"='user:seed',"+at+"=now(),dismissal_note='Historical note' WHERE workspace_id=? AND "+key+"=? AND version=?", ws, id, dismissed); err != nil {
					t.Fatal(err)
				}
				if state == "operator" {
					if _, err = st.db.ExecContext(ctx, "UPDATE "+table+" SET archived_at=now(),archived_by='user:seed' WHERE workspace_id=? AND id=?", ws, id); err != nil {
						t.Fatal(err)
					}
				}
			}
		}
	}
	// Simulate a restart after only some DDL committed. The current test DB
	// belongs to this fixture; no historical published migration is rewritten.
	if _, err := st.db.ExecContext(ctx, "DELETE FROM conveyor_singlestore_migrations WHERE version=16"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, "ALTER TABLE system_designs DROP COLUMN archive_reason"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, "ALTER TABLE requirements DROP COLUMN archive_note"); err != nil {
		t.Fatal(err)
	}
	if err := st.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash after repair committed, before its ledger write.
	if _, err := st.db.ExecContext(ctx, "DELETE FROM conveyor_singlestore_migrations WHERE version=16"); err != nil {
		t.Fatal(err)
	}

	if err := st.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for _, ws := range []string{"dismissal-a", "dismissal-b"} {
		scoped := store.WithWorkspace(ctx, ws)
		for _, tier := range []string{"requirement", "system_design"} {
			for _, state := range []string{"orphan", "pending", "confirmed", "operator", "empty"} {
				id := tier + "-" + state
				var archived bool
				var reason, note, actor string
				var events []core.Event
				var err error
				if tier == "requirement" {
					d, e := st.GetRequirement(scoped, id)
					err = e
					archived, reason, note, actor = d.Archived, d.ArchiveReason, d.ArchiveNote, d.ArchivedBy
					if err == nil {
						events, err = st.ListRequirementEvents(scoped, id)
					}
				} else {
					d, e := st.GetSystemDesign(scoped, id)
					err = e
					archived, reason, note, actor = d.Archived, d.ArchiveReason, d.ArchiveNote, d.ArchivedBy
					if err == nil {
						events, err = st.ListSystemDesignEvents(scoped, id)
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				if archived != (state == "orphan" || state == "operator") {
					t.Fatalf("%s/%s archived=%v", ws, id, archived)
				}
				count := 0
				for _, e := range events {
					if e.Kind != tier+".archived" {
						continue
					}
					count++
					var payload map[string]any
					if err = json.Unmarshal(e.Payload, &payload); err != nil {
						t.Fatal(err)
					}
					if e.ActorID != "system" || e.ActorRole != core.ActorSystem || payload["reason"] != core.ArchiveReasonOnlyProposalDismissed || payload["note"] != "Historical note" {
						t.Fatalf("event=%+v", e)
					}
				}
				if state == "orphan" {
					if count != 1 || reason != core.ArchiveReasonOnlyProposalDismissed || note != "Historical note" || actor != "system" {
						t.Fatalf("%s/%s count=%d reason=%q note=%q actor=%q", ws, id, count, reason, note, actor)
					}
					if !strings.Contains(log.String(), ws) || !strings.Contains(log.String(), tier+":"+id) {
						t.Fatalf("missing migration log: %s", log.String())
					}
				} else if count != 0 || reason != "" || note != "" {
					t.Fatalf("untouched %s/%s changed", ws, id)
				}
				// All original content and identifiers survive repair.
				if state != "empty" {
					if tier == "requirement" {
						v, e := st.GetRequirementVersion(scoped, id, 1)
						if e != nil || !strings.Contains(v.Content, "Historical proposal") || len(v.Statements) != 1 || v.Statements[0].ID != "REQ-1" {
							t.Fatalf("history=%+v err=%v", v, e)
						}
					} else {
						v, e := st.GetSystemDesignVersion(scoped, id, 1)
						if e != nil || !strings.Contains(v.Content, "Historical proposal") {
							t.Fatalf("history=%+v err=%v", v, e)
						}
					}
				}
			}
		}
	}
}

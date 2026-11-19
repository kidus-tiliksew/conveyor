package singlestore

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

// The baseline statements below are the read methods at
// storetest.ReadEgressInlineBaselineRevision, copied inline; the candidate
// variants call the public methods of the checked-out tree.
const (
	legacySystemDesignEventsSQL = `SELECT id,COALESCE(task_id,''),COALESCE(job_id,''),kind,actor_id,actor_role,payload_json,at,workspace_id FROM events_columnstore WHERE workspace_id=? AND kind LIKE 'system_design.%' AND JSON_EXTRACT_STRING(payload_json,'document_id')=? ORDER BY at,id`
	legacySystemDesignFamilySQL = `SELECT id,COALESCE(task_id,''),COALESCE(job_id,''),kind,actor_id,actor_role,payload_json,at,workspace_id FROM events_columnstore WHERE workspace_id=? AND kind LIKE 'system_design.%' ORDER BY at,id`
	legacyPlanningSessionSQL    = `SELECT id,COALESCE(task_id,''),COALESCE(job_id,''),kind,actor_id,actor_role,payload_json,at
		FROM events_columnstore WHERE workspace_id=? AND JSON_EXTRACT_STRING(payload_json,'session_id')=? ORDER BY at,id`
	legacyGovernanceSQL = `SELECT d.id,d.title,d.category,v.version,v.content,v.governs
		FROM system_designs d JOIN system_design_versions v
		  ON v.workspace_id=d.workspace_id AND v.document_id=d.id AND v.version=d.current_version
		WHERE d.workspace_id=? AND d.archived_at IS NULL ORDER BY d.id`
	// legacyDecisionCitationsSQL is the pre-change source read: each live
	// requirement and System Design current confirmed version's whole body.
	legacyDecisionCitationsSQL = `SELECT 'requirement',r.id,r.title,r.current_version,v.content
		FROM requirements r JOIN requirement_versions v
		  ON v.workspace_id=r.workspace_id AND v.requirement_id=r.id AND v.version=r.current_version
		WHERE r.workspace_id=? AND r.archived_at IS NULL AND v.confirmed
		UNION ALL
		SELECT 'system_design',d.id,d.title,d.current_version,v.content
		FROM system_designs d JOIN system_design_versions v
		  ON v.workspace_id=d.workspace_id AND v.document_id=d.id AND v.version=d.current_version
		WHERE d.workspace_id=? AND d.archived_at IS NULL AND v.confirmed`
)

func init() {
	readEgressCases.Register("selected-system-design-events", buildSystemDesignEvents)
	readEgressCases.Register("system-design-events-by-document", buildSystemDesignEventsByDocument)
	readEgressCases.Register("planning-session-events", buildPlanningSessionEvents)
	readEgressCases.Register("governance-designs", buildGovernanceDesigns)
	readEgressCases.Register("decision-citations", buildDecisionCitations)
	readEgressCases.Register("activity-markers", buildActivityMarkers)
	readEgressCases.Register("activity-markers-v0.72.0", buildActivityMarkersTag)
}

func legacyEventRows(ctx context.Context, st *Store, label, sql string, args ...any) ([]core.Event, error) {
	ctx = dbmetrics.WithReadLabel(ctx, label)
	rows, err := documentRows(ctx, st.db, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []core.Event{}
	for rows.Next() {
		var row core.Event
		var eventWorkspace string
		if err = rows.Scan(&row.ID, &row.TaskID, &row.JobID, &row.Kind, &row.ActorID, &row.ActorRole, &row.Payload, &row.At, &eventWorkspace); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func buildSystemDesignEvents(t testing.TB, ctx context.Context, st *Store) storetest.ReadEgressCase {
	t.Helper()
	noise, pad, err := storetest.ReadEgressSizes()
	if err != nil {
		t.Fatal(err)
	}
	selected, fixture := storetest.SeedSystemDesignEventEgress(t, ctx, st, noise, pad)
	return storetest.ReadEgressCase{Name: "selected-system-design-events", Fixture: fixture,
		Baseline: func(ctx context.Context) (storetest.ReadEgressOutcome, error) {
			events, err := legacyEventRows(ctx, st, "LegacySystemDesignEvents", legacySystemDesignEventsSQL, documentWorkspace(ctx), selected)
			if err != nil {
				return storetest.ReadEgressOutcome{}, err
			}
			return readEgressOutcome(events)
		},
		Candidate: func(ctx context.Context) (storetest.ReadEgressOutcome, error) {
			events, err := st.ListSystemDesignEvents(ctx, selected)
			if err != nil {
				return storetest.ReadEgressOutcome{}, err
			}
			return readEgressOutcome(events)
		}}
}

// buildSystemDesignEventsByDocument compares the whole-family scan grouped in
// Go, which the removed by-document read ran, with the scoped
// ListSystemDesignEvents read that replaced it. Unselected documents' events
// are the egress noise (component-document-corpus).
func buildSystemDesignEventsByDocument(t testing.TB, ctx context.Context, st *Store) storetest.ReadEgressCase {
	t.Helper()
	noise, pad, err := storetest.ReadEgressSizes()
	if err != nil {
		t.Fatal(err)
	}
	selected, fixture := storetest.SeedSystemDesignEventEgress(t, ctx, st, noise, pad)
	return storetest.ReadEgressCase{Name: "system-design-events-by-document", Fixture: fixture,
		Baseline: func(ctx context.Context) (storetest.ReadEgressOutcome, error) {
			events, err := legacyEventRows(ctx, st, "LegacySystemDesignEventsByDocument", legacySystemDesignFamilySQL, documentWorkspace(ctx))
			if err != nil {
				return storetest.ReadEgressOutcome{}, err
			}
			grouped := []core.Event{}
			for _, event := range events {
				var payload map[string]any
				if json.Unmarshal(event.Payload, &payload) == nil && payload["document_id"] == selected {
					grouped = append(grouped, event)
				}
			}
			return readEgressOutcome(grouped)
		},
		Candidate: func(ctx context.Context) (storetest.ReadEgressOutcome, error) {
			events, err := st.ListSystemDesignEvents(ctx, selected)
			if err != nil {
				return storetest.ReadEgressOutcome{}, err
			}
			return readEgressOutcome(events)
		}}
}

func buildPlanningSessionEvents(t testing.TB, ctx context.Context, st *Store) storetest.ReadEgressCase {
	t.Helper()
	noise, pad, err := storetest.ReadEgressSizes()
	if err != nil {
		t.Fatal(err)
	}
	session, fixture := storetest.SeedPlanningSessionEventEgress(t, ctx, st, noise, pad)
	return storetest.ReadEgressCase{Name: "planning-session-events", Fixture: fixture,
		Baseline: func(ctx context.Context) (storetest.ReadEgressOutcome, error) {
			ctx = dbmetrics.WithReadLabel(ctx, "LegacyPlanningSessionEvents")
			rows, err := documentRows(ctx, st.db, legacyPlanningSessionSQL, documentWorkspace(ctx), session)
			if err != nil {
				return storetest.ReadEgressOutcome{}, err
			}
			defer rows.Close()
			var out []core.Event
			for rows.Next() {
				var event core.Event
				if err := rows.Scan(&event.ID, &event.TaskID, &event.JobID, &event.Kind, &event.ActorID, &event.ActorRole, &event.Payload, &event.At); err != nil {
					return storetest.ReadEgressOutcome{}, err
				}
				out = append(out, event)
			}
			if err := rows.Err(); err != nil {
				return storetest.ReadEgressOutcome{}, err
			}
			return readEgressOutcome(out)
		},
		Candidate: func(ctx context.Context) (storetest.ReadEgressOutcome, error) {
			events, err := st.ListPlanningSessionEvents(ctx, session)
			if err != nil {
				return storetest.ReadEgressOutcome{}, err
			}
			return readEgressOutcome(events)
		}}
}

// buildGovernanceDesigns compares the inline governance statement (every
// current design read, filtered by repository in Go) with the public
// ListGovernanceDesigns for one repository, among designs governing other
// repositories whose content is large. Design counts come from
// CONVEYOR_READ_EGRESS_GOVERNANCE_DESIGNS (default 40).
func buildGovernanceDesigns(t testing.TB, ctx context.Context, st *Store) storetest.ReadEgressCase {
	t.Helper()
	_, pad, err := storetest.ReadEgressSizes()
	if err != nil {
		t.Fatal(err)
	}
	unmatched, err := storetest.ReadEgressCount("CONVEYOR_READ_EGRESS_GOVERNANCE_DESIGNS", 40)
	if err != nil {
		t.Fatal(err)
	}
	const repository = "egress-governed"
	fixture := storetest.SeedGovernanceEgress(t, ctx, st, repository, 3, unmatched, pad)
	return storetest.ReadEgressCase{Name: "governance-designs", Fixture: fixture,
		Candidate: func(ctx context.Context) (storetest.ReadEgressOutcome, error) {
			designs, err := st.ListGovernanceDesigns(ctx, repository)
			if err != nil {
				return storetest.ReadEgressOutcome{}, err
			}
			digest, err := storetest.ReadEgressDigest(designs)
			return storetest.ReadEgressOutcome{Digest: digest, Successes: len(designs)}, err
		},
		Baseline: func(ctx context.Context) (storetest.ReadEgressOutcome, error) {
			ctx = dbmetrics.WithReadLabel(ctx, "LegacyGovernanceDesigns")
			rows, err := documentRows(ctx, st.db, legacyGovernanceSQL, documentWorkspace(ctx))
			if err != nil {
				return storetest.ReadEgressOutcome{}, err
			}
			defer rows.Close()
			out := make([]core.GovernanceDesignContext, 0)
			for rows.Next() {
				var item core.GovernanceDesignContext
				var governs []byte
				if err = rows.Scan(&item.ID, &item.Title, &item.Category, &item.Version, &item.Content, &governs); err != nil {
					return storetest.ReadEgressOutcome{}, err
				}
				if err = json.Unmarshal(governs, &item.Governs); err != nil {
					return storetest.ReadEgressOutcome{}, err
				}
				if core.GovernsRepositoryPaths(item.Governs, repository) {
					out = append(out, item)
				}
			}
			if err := rows.Err(); err != nil {
				return storetest.ReadEgressOutcome{}, err
			}
			digest, err := storetest.ReadEgressDigest(out)
			return storetest.ReadEgressOutcome{Digest: digest, Successes: len(out)}, err
		}}
}

// buildDecisionCitations compares the inline two-statement citation read,
// which loads each citing document's body, with the public
// ListDecisionCitations, which returns only citation metadata among
// requirements and designs whose content carries the noise payload.
func buildDecisionCitations(t testing.TB, ctx context.Context, st *Store) storetest.ReadEgressCase {
	t.Helper()
	noise, pad, err := storetest.ReadEgressSizes()
	if err != nil {
		t.Fatal(err)
	}
	fixture := storetest.SeedDecisionCitationEgress(t, ctx, st, noise, pad)
	return storetest.ReadEgressCase{Name: "decision-citations", Fixture: fixture,
		Baseline: func(ctx context.Context) (storetest.ReadEgressOutcome, error) {
			return legacyDecisionCitations(ctx, st, "LegacyDecisionCitations")
		},
		Candidate: func(ctx context.Context) (storetest.ReadEgressOutcome, error) {
			citations, err := st.ListDecisionCitations(ctx)
			if err != nil {
				return storetest.ReadEgressOutcome{}, err
			}
			digest, err := storetest.ReadEgressDigest(citations)
			return storetest.ReadEgressOutcome{Digest: digest, Successes: len(citations)}, err
		}}
}

// legacyDecisionCitations reproduces the inline two-statement citation read:
// the workspace's decision IDs, then each source's body, derived with
// core.DeriveDecisionCitations in Go.
func legacyDecisionCitations(ctx context.Context, st *Store, label string) (storetest.ReadEgressOutcome, error) {
	ctx = dbmetrics.WithReadLabel(ctx, label)
	rows, err := documentRows(ctx, st.db, `SELECT id FROM decisions WHERE workspace_id=?`, documentWorkspace(ctx))
	if err != nil {
		return storetest.ReadEgressOutcome{}, err
	}
	decisionIDs := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return storetest.ReadEgressOutcome{}, err
		}
		decisionIDs = append(decisionIDs, id)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return storetest.ReadEgressOutcome{}, err
	}
	rows, err = documentRows(ctx, st.db, legacyDecisionCitationsSQL, documentWorkspace(ctx), documentWorkspace(ctx))
	if err != nil {
		return storetest.ReadEgressOutcome{}, err
	}
	defer rows.Close()
	sources := []core.DecisionCitationSource{}
	for rows.Next() {
		var source core.DecisionCitationSource
		if err = rows.Scan(&source.Tier, &source.DocumentID, &source.Title, &source.Version, &source.Content); err != nil {
			return storetest.ReadEgressOutcome{}, err
		}
		sources = append(sources, source)
	}
	if err = rows.Err(); err != nil {
		return storetest.ReadEgressOutcome{}, err
	}
	citations := core.DeriveDecisionCitations(decisionIDs, sources)
	digest, err := storetest.ReadEgressDigest(citations)
	return storetest.ReadEgressOutcome{Digest: digest, Successes: len(citations)}, err
}

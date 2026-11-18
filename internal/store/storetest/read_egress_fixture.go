package storetest

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// ReadEgressInlineBaselineRevision is the commit whose read methods the
// inline baseline statements in the backend measurement cases reproduce.
// Baseline variants run those embedded statements, not code at the candidate
// revision, so metadata names this commit rather than the candidate.
const ReadEgressInlineBaselineRevision = "4b37d6fbd2dfc4806b74390b120b6d9cc6c868a3"

// ReadEgressTask creates the task that owns a case's fixture events. The
// context must carry the workspace and an actor, with the repository
// "conveyor" bootstrapped as the other store fixtures do.
func ReadEgressTask(t testing.TB, ctx context.Context, st store.Store) string {
	t.Helper()
	ws, _ := store.WorkspaceFromContext(ctx)
	id := "egress-" + core.NewTaskID()
	if err := st.CreateTask(ctx, core.Task{ID: id, Workspace: ws, Title: id, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + id,
		State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	return id
}

// AppendReadEgressEvents appends each of kind on taskID, "each" per value,
// through the public AppendEvent API (never SQL). Every payload carries
// {key: value}; payloadBytes > 0 adds a padding string of that size. Event
// times start at base and advance one second per event; the next free time is
// returned. It returns the number of events appended.
func AppendReadEgressEvents(t testing.TB, ctx context.Context, st store.Store, taskID, kind, key string, values []string, each, payloadBytes int, base time.Time) (time.Time, int) {
	t.Helper()
	padding := strings.Repeat("x", payloadBytes)
	n := 0
	for _, value := range values {
		for range each {
			payload := map[string]any{key: value}
			if payloadBytes > 0 {
				payload["padding"] = padding
			}
			if err := st.AppendEvent(ctx, core.Event{TaskID: taskID, Kind: kind, At: base.Add(time.Duration(n) * time.Second), Payload: core.JSONPayload(payload)}); err != nil {
				t.Fatal(err)
			}
			n++
		}
	}
	return base.Add(time.Duration(n) * time.Second), n
}

// ReadEgressNames returns count names "prefix-0" … "prefix-(count-1)".
func ReadEgressNames(prefix string, count int) []string {
	names := make([]string, count)
	for i := range names {
		names[i] = fmt.Sprintf("%s-%d", prefix, i)
	}
	return names
}

// ReadEgressGovernedContent is System Design content that governs paths in
// one repository, followed by padBytes of unrelated body text. The H1 is title.
func ReadEgressGovernedContent(title, repository string, padBytes int) string {
	return fmt.Sprintf("# %s\n\n```conveyor:governs\n- repo: %s\n  paths:\n    - internal/store/**\n```\n\n%s\n", title, repository, strings.Repeat("unrelated governance prose. ", padBytes/28+1))
}

func readEgressTitleH1(title, body string) string {
	if _, rest, ok := strings.Cut(body, "\n"); ok {
		return "# " + title + "\n" + rest
	}
	return "# " + title
}

// ReadEgressSizes reads the shared fixture sizes: unrelated noise items
// (CONVEYOR_READ_EGRESS_NOISE_EVENTS, default 300) and the byte size of each
// noise payload (CONVEYOR_READ_EGRESS_PAYLOAD_BYTES, default 16 KiB).
func ReadEgressSizes() (noise, payloadBytes int, err error) {
	if noise, err = ReadEgressCount("CONVEYOR_READ_EGRESS_NOISE_EVENTS", 300); err != nil {
		return 0, 0, err
	}
	payloadBytes, err = ReadEgressCount("CONVEYOR_READ_EGRESS_PAYLOAD_BYTES", 16<<10)
	return noise, payloadBytes, err
}

// readEgressNoiseDocuments is how many unrelated documents or sessions share
// a case's noise events.
const readEgressNoiseDocuments = 25

// readEgressSelectedEvents is how many small events the selected subject owns
// beyond those its lifecycle emits.
const readEgressSelectedEvents = 3

// SeedSystemDesignEventEgress creates one selected System Design through the
// public API, gives it a few small consultation events, and adds noise events
// of the same family (system_design.consulted) for other document ids, each
// with a padBytes payload. It returns the selected document id and fixture
// counts. Fixture rows are written only through public store methods.
func SeedSystemDesignEventEgress(t testing.TB, ctx context.Context, st store.Store, noise, padBytes int) (string, map[string]int) {
	t.Helper()
	id := "design-egress-" + core.NewTaskID()
	title := "Egress " + id
	if _, _, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: id, Title: title, Category: "Component design"},
		core.SystemDesignVersion{Content: ReadEgressGovernedContent(title, "egress-events", 0), Origin: core.SystemDesignOriginOperator}); err != nil {
		t.Fatal(err)
	}
	task := ReadEgressTask(t, ctx, st)
	at, _ := AppendReadEgressEvents(t, ctx, st, task, "system_design.consulted", "document_id", []string{id}, readEgressSelectedEvents, 0, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	per := max(1, noise/readEgressNoiseDocuments)
	_, appended := AppendReadEgressEvents(t, ctx, st, task, "system_design.consulted", "document_id", ReadEgressNames("design-egress-unrelated", readEgressNoiseDocuments), per, padBytes, at)
	return id, map[string]int{"selected_consultations": readEgressSelectedEvents, "noise_events": appended, "noise_documents": readEgressNoiseDocuments, "noise_payload_bytes": padBytes}
}

// SeedPlanningSessionEventEgress adds small events for one selected planning
// session and large-payload events for other sessions
// (planning_session.message_appended), through the public event API. It
// returns the selected session id and fixture counts.
func SeedPlanningSessionEventEgress(t testing.TB, ctx context.Context, st store.Store, noise, padBytes int) (string, map[string]int) {
	t.Helper()
	session := "session-egress-" + core.NewTaskID()
	task := ReadEgressTask(t, ctx, st)
	at, _ := AppendReadEgressEvents(t, ctx, st, task, "planning_session.message_appended", "session_id", []string{session}, 5, 0, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	per := max(1, noise/readEgressNoiseDocuments)
	_, appended := AppendReadEgressEvents(t, ctx, st, task, "planning_session.message_appended", "session_id", ReadEgressNames("session-egress-unrelated", readEgressNoiseDocuments), per, padBytes, at)
	return session, map[string]int{"selected_events": 5, "noise_events": appended, "noise_sessions": readEgressNoiseDocuments, "noise_payload_bytes": padBytes}
}

// SeedGovernanceEgress creates governedCount designs governing repository and
// unmatched designs governing other repositories, each unmatched design
// carrying padBytes of content. It returns fixture counts.
func SeedGovernanceEgress(t testing.TB, ctx context.Context, st store.Store, repository string, governedCount, unmatched, padBytes int) map[string]int {
	t.Helper()
	create := func(repo string, pad int) {
		id := "design-gov-" + core.NewTaskID()
		title := "Governance " + id
		design, version, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: id, Title: title, Category: "Component design"},
			core.SystemDesignVersion{Content: ReadEgressGovernedContent(title, repo, pad), Origin: core.SystemDesignOriginOperator})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := st.ConfirmSystemDesignVersion(ctx, design.ID, version.Version); err != nil {
			t.Fatal(err)
		}
	}
	for range governedCount {
		create(repository, 0)
	}
	for i := range unmatched {
		create(fmt.Sprintf("egress-other-%d", i), padBytes)
	}
	return map[string]int{"governed_designs": governedCount, "unmatched_designs": unmatched, "unmatched_content_bytes": padBytes}
}

// SeedDecisionCitationEgress creates three decisions named by two confirmed
// requirements and one confirmed System Design, then noise unrelated
// requirements, each carrying padBytes of content and naming no decision.
// The citing documents' content is small and the noise documents' content
// is large, so a citation read that returned document bodies would carry
// the noise. It returns fixture counts.
func SeedDecisionCitationEgress(t testing.TB, ctx context.Context, st store.Store, noise, padBytes int) map[string]int {
	t.Helper()
	decisions := []string{}
	for i := range 3 {
		id := fmt.Sprintf("DEC-%d", i+1)
		if _, err := st.ProposeDecision(ctx, core.Decision{ID: id, Statement: "Egress " + id + ".", Context: "Citation egress fixture.", AlternativesRejected: "None.", Origin: core.DecisionOriginOperator}); err != nil {
			t.Fatal(err)
		}
		decisions = append(decisions, id)
	}
	cites := "Cites " + strings.Join(decisions, ", ") + "."
	statements := []core.RequirementStatement{{ID: "REQ-1", Statement: "Cite decisions."}}
	suffix := core.NewTaskID()[:6]
	citing := 0
	for i := range 2 {
		id := fmt.Sprintf("egress-cite-%d-%s", i, suffix)
		title := "Egress citation " + id
		_, version, err := st.CreateRequirement(ctx, core.Requirement{ID: id, Title: title}, core.RequirementVersion{Content: "# " + title + "\n\n" + cites, Statements: statements, Origin: core.RequirementOriginOperator})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = st.ConfirmRequirementVersion(ctx, id, version.Version); err != nil {
			t.Fatal(err)
		}
		citing++
	}
	design := "egress-cite-design-" + suffix
	_, version, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: design, Title: "Egress citation design", Category: "Component design"},
		core.SystemDesignVersion{Content: "# Egress citation design\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/store/**\n```\n\n" + cites + "\n", Origin: core.SystemDesignOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmSystemDesignVersion(ctx, design, version.Version); err != nil {
		t.Fatal(err)
	}
	citing++
	pad := strings.Repeat("unrelated requirement prose. ", padBytes/28+1)
	for i := range noise {
		id := fmt.Sprintf("egress-noise-%d-%s", i, suffix)
		title := "Egress noise " + id
		if _, _, err := st.CreateRequirement(ctx, core.Requirement{ID: id, Title: title}, core.RequirementVersion{Content: "# " + title + "\n\n" + pad, Statements: statements, Origin: core.RequirementOriginOperator}); err != nil {
			t.Fatal(err)
		}
	}
	return map[string]int{"decisions": len(decisions), "citing_documents": citing, "citations": citing * len(decisions), "noise_documents": noise, "noise_payload_bytes": padBytes}
}

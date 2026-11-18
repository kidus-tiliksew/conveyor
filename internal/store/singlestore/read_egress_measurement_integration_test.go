package singlestore

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

// readEgressCases are the hot-read egress cases measured on SingleStore.
// Add a case with readEgressCases.Register in an init function; the
// measurement test builds each on one owned workspace and runs it.
var readEgressCases = storetest.NewReadEgressRegistry[*Store]()

func init() {
	readEgressCases.Register("selected-reference-document-events", buildSelectedReferenceDocumentEvents)
}

// TestHotReadEgressMeasurementIntegration measures every registered case
// with the store's driver read statistics. It runs only when
// CONVEYOR_READ_EGRESS_MEASUREMENT=1 (make measure-hot-read-egress-singlestore)
// and then requires the database URL instead of skipping.
func TestHotReadEgressMeasurementIntegration(t *testing.T) {
	if os.Getenv("CONVEYOR_READ_EGRESS_MEASUREMENT") != "1" {
		t.Skip("CONVEYOR_READ_EGRESS_MEASUREMENT=1 selects the hot-read egress measurement")
	}
	if strings.TrimSpace(os.Getenv("CONVEYOR_TEST_SINGLESTORE_URL")) == "" {
		t.Fatal("CONVEYOR_TEST_SINGLESTORE_URL is required for the hot-read egress measurement")
	}
	st := integrationStore(t)
	var memsqlVersion string
	if err := st.db.QueryRowContext(t.Context(), `SELECT @@memsql_version`).Scan(&memsqlVersion); err != nil {
		t.Fatal(err)
	}
	t.Log(memsqlVersion)
	ctx := documentPlanWorkspace(t, st, "read-egress-"+core.NewTaskID())
	env, err := storetest.ReadEgressEnvFromProcess("singlestore", st.ReadStats)
	if err != nil {
		t.Fatal(err)
	}
	readEgressCases.Run(t, ctx, st, env)
}

// legacyReferenceEventsSQL is the pre-change statement: the workspace's whole
// reference-document event family, filtered by document in Go.
const legacyReferenceEventsSQL = `SELECT id,COALESCE(task_id,''),COALESCE(job_id,''),kind,actor_id,actor_role,payload_json,at,workspace_id
		FROM events_columnstore WHERE workspace_id=? AND kind LIKE 'reference_document.%' ORDER BY at,id`

// buildSelectedReferenceDocumentEvents seeds one selected reference document
// among unrelated documents whose events carry large payloads, and compares
// the legacy family query plus Go filter with the public
// ListReferenceDocumentEvents. Fixture sizes come from
// CONVEYOR_READ_EGRESS_NOISE_EVENTS and CONVEYOR_READ_EGRESS_PAYLOAD_BYTES.
func buildSelectedReferenceDocumentEvents(t testing.TB, ctx context.Context, st *Store) storetest.ReadEgressCase {
	t.Helper()
	noise, err := storetest.ReadEgressCount("CONVEYOR_READ_EGRESS_NOISE_EVENTS", 300)
	if err != nil {
		t.Fatal(err)
	}
	payloadBytes, err := storetest.ReadEgressCount("CONVEYOR_READ_EGRESS_PAYLOAD_BYTES", 16<<10)
	if err != nil {
		t.Fatal(err)
	}
	selected := "ref-selected-" + core.NewTaskID()
	if _, _, err := st.CreateReferenceDocument(ctx, core.ReferenceDocument{ID: selected, Name: "Selected " + selected},
		core.ReferenceDocumentVersion{Filename: "selected.md", ContentType: "text/markdown", Content: "# Selected"}); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		if _, err := st.SupersedeReferenceDocument(ctx, selected, core.ReferenceDocumentVersion{Filename: "selected.md", ContentType: "text/markdown", Content: fmt.Sprintf("# Selected %d", i+2)}); err != nil {
			t.Fatal(err)
		}
	}
	taskID := documentPlanTask(t, st, ctx, "ref-egress")
	// Public append API only; no fixture row is written with SQL. The selected
	// document's own consultations are small; unrelated documents' are large.
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	padding := strings.Repeat("x", payloadBytes)
	const selectedConsultations = 3
	for i := range selectedConsultations + noise {
		payload := map[string]any{"document_id": selected, "version": 1}
		if i >= selectedConsultations {
			payload = map[string]any{"document_id": fmt.Sprintf("ref-unrelated-%d", i%25), "padding": padding}
		}
		if err := st.AppendEvent(ctx, core.Event{TaskID: taskID, Kind: "reference_document.consulted", At: base.Add(time.Duration(i) * time.Second), Payload: core.JSONPayload(payload)}); err != nil {
			t.Fatal(err)
		}
	}
	ws := documentWorkspace(ctx)
	var family int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events_columnstore WHERE workspace_id=? AND kind LIKE 'reference_document.%'`, ws).Scan(&family); err != nil {
		t.Fatal(err)
	}
	baseline := func(ctx context.Context) (storetest.ReadEgressOutcome, error) {
		ctx = dbmetrics.WithReadLabel(ctx, "LegacyReferenceDocumentEventsFamilyScan")
		rows, err := documentRows(ctx, st.db, legacyReferenceEventsSQL, ws)
		if err != nil {
			return storetest.ReadEgressOutcome{}, err
		}
		defer rows.Close()
		served := []core.Event{}
		for rows.Next() {
			var event core.Event
			var eventWorkspace string
			if err = rows.Scan(&event.ID, &event.TaskID, &event.JobID, &event.Kind, &event.ActorID, &event.ActorRole, &event.Payload, &event.At, &eventWorkspace); err != nil {
				return storetest.ReadEgressOutcome{}, err
			}
			var payload map[string]any
			if json.Unmarshal(event.Payload, &payload) == nil && payload["document_id"] == selected {
				served = append(served, event)
			}
		}
		if err := rows.Err(); err != nil {
			return storetest.ReadEgressOutcome{}, err
		}
		return readEgressOutcome(served)
	}
	candidate := func(ctx context.Context) (storetest.ReadEgressOutcome, error) {
		served, err := st.ListReferenceDocumentEvents(ctx, selected)
		if err != nil {
			return storetest.ReadEgressOutcome{}, err
		}
		return readEgressOutcome(served)
	}
	return storetest.ReadEgressCase{Name: "selected-reference-document-events", Baseline: baseline, Candidate: candidate,
		Fixture: map[string]int{"family_events": family, "selected_revisions": 3, "selected_consultations": selectedConsultations, "noise_events": noise, "noise_payload_bytes": payloadBytes}}
}

func readEgressOutcome(events []core.Event) (storetest.ReadEgressOutcome, error) {
	digest, err := storetest.ReadEgressDigest(events)
	return storetest.ReadEgressOutcome{Digest: digest, Successes: len(events)}, err
}

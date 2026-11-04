package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/monitor"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

type consultationRecord struct {
	Version      int    `json:"version"`
	SessionID    string `json:"session_id"`
	WorkOrderID  string `json:"work_order_id"`
	Consultation string `json:"consultation"`
}

// RunSystemDesignConsultationConformance holds every backend to one read-key
// rule: workspace, session, work order, document, and version record one
// consultation, across repeats and concurrent calls. Planning-session records
// and delivery_no_revision observations stay outside that rule, and events
// already written stay unchanged (req-design-delivery REQ-5).
func RunSystemDesignConsultationConformance(t *testing.T, x Fixture) {
	var st store.Store = x.Backend
	ctx, workspace := x.Context, x.Workspace
	design := func(t *testing.T, id string) string {
		t.Helper()
		document := createConfirmedDesign(t, st, ctx, id, "internal/consultation/**")
		return document.ID
	}
	records := func(t *testing.T, documentID string) []consultationRecord {
		t.Helper()
		events, err := st.ListSystemDesignEvents(ctx, documentID)
		if err != nil {
			t.Fatal(err)
		}
		out := []consultationRecord{}
		for _, event := range events {
			if event.Kind != "system_design.consulted" {
				continue
			}
			var record consultationRecord
			if err := json.Unmarshal(event.Payload, &record); err != nil {
				t.Fatal(err)
			}
			out = append(out, record)
		}
		return out
	}

	t.Run("repeat read of one key records one event", func(t *testing.T) {
		documentID := design(t, "design-consultation-repeat")
		orderID := seedConsultationOrder(t, st, ctx, workspace)
		for range 3 {
			if err := st.RecordSystemDesignConsulted(ctx, documentID, 1, "session-a", orderID); err != nil {
				t.Fatal(err)
			}
		}
		got := records(t, documentID)
		if len(got) != 1 || got[0].Version != 1 || got[0].SessionID != "session-a" || got[0].WorkOrderID != orderID {
			t.Fatalf("records=%+v", got)
		}
	})

	t.Run("each part of the key records independently", func(t *testing.T) {
		documentID := design(t, "design-consultation-key")
		other := design(t, "design-consultation-key-other")
		if _, err := st.ProposeSystemDesignVersion(ctx, core.SystemDesignVersion{DocumentID: documentID, Content: "# Second\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/consultation/**\n```", Origin: core.SystemDesignOriginOperator}); err != nil {
			t.Fatal(err)
		}
		firstOrder, secondOrder := seedConsultationOrder(t, st, ctx, workspace), seedConsultationOrder(t, st, ctx, workspace)
		for _, key := range []struct {
			version          int
			session, orderID string
		}{
			{1, "session-a", firstOrder},
			{2, "session-a", firstOrder},
			{1, "session-b", firstOrder},
			{1, "session-a", secondOrder},
		} {
			for range 2 {
				if err := st.RecordSystemDesignConsulted(ctx, documentID, key.version, key.session, key.orderID); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := st.RecordSystemDesignConsulted(ctx, other, 1, "session-a", firstOrder); err != nil {
			t.Fatal(err)
		}
		if got := records(t, documentID); len(got) != 4 {
			t.Fatalf("records=%+v, want one per key", got)
		}
		if got := records(t, other); len(got) != 1 {
			t.Fatalf("other document records=%+v, want one", got)
		}
	})

	t.Run("concurrent reads of one key record one event", func(t *testing.T) {
		documentID := design(t, "design-consultation-concurrent")
		orderID := seedConsultationOrder(t, st, ctx, workspace)
		const readers = 8
		var wait sync.WaitGroup
		failures := make(chan error, readers)
		for range readers {
			wait.Add(1)
			go func() {
				defer wait.Done()
				// A backend conflict is the retryable outcome a later read repairs.
				var err error
				for range 20 {
					if err = st.RecordSystemDesignConsulted(ctx, documentID, 1, "session-a", orderID); !errors.Is(err, store.ErrRetryable) {
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
				if err != nil {
					failures <- err
				}
			}()
		}
		wait.Wait()
		close(failures)
		for err := range failures {
			t.Fatal(err)
		}
		if got := records(t, documentID); len(got) != 1 {
			t.Fatalf("records=%+v, want one", got)
		}
	})

	t.Run("planning sessions and delivery records stay outside the read key", func(t *testing.T) {
		documentID := design(t, "design-consultation-planning")
		session, err := st.CreatePlanningSession(ctx, core.PlanningSession{ID: "consultation-" + core.NewTaskID(), Title: "Consultation"})
		if err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if err := st.RecordSystemDesignConsulted(ctx, documentID, 1, session.ID, ""); err != nil {
				t.Fatal(err)
			}
		}
		deliveryTask := createDriftTask(t, st, ctx, workspace, "consultation-delivery")
		if err := st.AppendEvent(ctx, core.Event{TaskID: deliveryTask.ID, Kind: store.TaskContextDesignAdded, Payload: core.JSONPayload(map[string]any{"id": documentID, "version": 1})}); err != nil {
			t.Fatal(err)
		}
		if err := st.AppendEvent(ctx, core.Event{TaskID: deliveryTask.ID, Kind: "merge.confirmed", Payload: core.JSONPayload(map[string]any{"repository": "kidus-tiliksew/conveyor", "head_sha": "consultation-head"})}); err != nil {
			t.Fatal(err)
		}
		judgment, err := st.(monitor.Store).ResolveCausalSystemDesignMerge(ctx, documentID, "conveyor", "consultation-head", latestEventID(t, st, ctx, deliveryTask.ID, "merge.confirmed"), "consultation-drift", []string{"internal/consultation/file.go"}, true)
		if err != nil || !judgment.Consulted {
			t.Fatalf("delivery judgment=%+v err=%v", judgment, err)
		}
		orderID := seedConsultationOrder(t, st, ctx, workspace)
		for range 2 {
			if err := st.RecordSystemDesignConsulted(ctx, documentID, 1, session.ID, orderID); err != nil {
				t.Fatal(err)
			}
		}
		planning, delivery, read := 0, 0, 0
		for _, record := range records(t, documentID) {
			switch {
			case record.Consultation == "delivery_no_revision":
				delivery++
			case record.WorkOrderID == orderID:
				read++
			default:
				planning++
			}
		}
		if planning != 2 || delivery != 1 || read != 1 {
			t.Fatalf("planning=%d delivery=%d read=%d, want 2, 1, 1", planning, delivery, read)
		}
	})

	t.Run("events written before read keys stay unchanged and still count", func(t *testing.T) {
		if x.SeedEvents == nil {
			t.Skip("this fixture cannot seed workspace events; TestMemoryConformance runs this case")
		}
		documentID := design(t, "design-consultation-history")
		orderID := seedConsultationOrder(t, st, ctx, workspace)
		payload := core.JSONPayload(map[string]any{"workspace_id": workspace, "document_id": documentID, "version": 1, "session_id": "session-a", "work_order_id": orderID})
		at := time.Now().UTC()
		x.SeedEvents(t, ctx, 0, []core.Event{{ID: 1, Kind: "system_design.consulted", At: at, Payload: payload}, {ID: 2, Kind: "system_design.consulted", At: at, Payload: payload}})
		if err := st.RecordSystemDesignConsulted(ctx, documentID, 1, "session-a", orderID); err != nil {
			t.Fatal(err)
		}
		if got := records(t, documentID); len(got) != 2 {
			t.Fatalf("records=%+v, want the two historical events and no new one", got)
		}
	})

	t.Run("missing targets record nothing", func(t *testing.T) {
		documentID := design(t, "design-consultation-missing")
		orderID := seedConsultationOrder(t, st, ctx, workspace)
		if err := st.RecordSystemDesignConsulted(ctx, documentID, 99, "session-a", orderID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("missing version err=%v", err)
		}
		if err := st.RecordSystemDesignConsulted(ctx, documentID, 1, "session-a", "order-missing"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("missing order err=%v", err)
		}
		if err := st.RecordSystemDesignConsulted(store.WithWorkspace(ctx, workspace+"-sibling"), documentID, 1, "session-a", orderID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("sibling workspace err=%v", err)
		}
		if got := records(t, documentID); len(got) != 0 {
			t.Fatalf("records=%+v, want none", got)
		}
	})

	t.Run("document history reads with no consultation", func(t *testing.T) {
		documentID := design(t, "design-consultation-empty")
		page, err := st.ListDocumentEventPage(ctx, core.LineageSystemDesign, documentID, store.DocumentEventQuery{Limit: 25})
		if err != nil {
			t.Fatal(err)
		}
		if page.Total == 0 || len(page.Events) != page.Total {
			t.Fatalf("history page=%+v, want the creation events", page)
		}
		for _, event := range page.Events {
			if event.Kind == "system_design.consulted" {
				t.Fatalf("history holds a consultation: %+v", event)
			}
		}
	})
}

// seedConsultationOrder creates one queued implementation order on a new task.
func seedConsultationOrder(t *testing.T, st store.Store, ctx context.Context, workspace string) string {
	t.Helper()
	now := time.Now().UTC()
	task := createDriftTask(t, st, ctx, workspace, "consultation")
	orderID := task.ID + "-implement"
	if err := st.CreateJob(ctx, core.Job{ID: orderID, TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}); err != nil {
		t.Fatal(err)
	}
	if err := CreateWorkOrder(ctx, st, core.WorkOrder{ID: orderID, TaskID: task.ID, JobID: orderID, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	return orderID
}

package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/monitor"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

type SystemDesignDriftFactory func(t *testing.T) (store.Store, context.Context, string)

func RunSystemDesignDriftConformance(t *testing.T, factory SystemDesignDriftFactory) {
	t.Helper()
	for _, test := range []struct {
		name             string
		attachBefore     bool
		submissionAttach bool
		attachAfter      bool
		proposal         bool
		wantDrift        bool
		wantConsultation bool
	}{
		{name: "submission-attached lineaged merge records consultation", attachBefore: true, submissionAttach: true, wantConsultation: true},
		{name: "unattached lineaged merge records drift", wantDrift: true},
		{name: "attachment after merge does not rewrite causal context", attachAfter: true, wantDrift: true},
		{name: "same task proposal wins over attached consultation", attachBefore: true, proposal: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			st, ctx, workspace := factory(t)
			now := time.Date(2026, 8, 13, 8, 0, 0, 0, time.UTC)
			service := driftService(t, st, ctx, workspace, now)
			delivery := createDriftTask(t, st, ctx, workspace, "lineaged-delivery")
			document := createConfirmedDesign(t, st, ctx, "DESIGN-lineaged", "internal/dispatch/**")
			attach := func() {
				if test.submissionAttach {
					attached, attachErr := st.AttachSubmissionGovernance(ctx, delivery.ID, "conveyor", []string{"internal/dispatch/dispatch.go"}, store.SubmissionGovernanceAttribution{WorkOrderID: delivery.ID + "-implement", SessionID: "worker-session"})
					if attachErr != nil || len(attached) != 1 || attached[0].ID != document.ID {
						t.Fatalf("submission attachment=%+v err=%v", attached, attachErr)
					}
					return
				}
				if err := st.AppendEvent(ctx, core.Event{TaskID: delivery.ID, Kind: store.TaskContextDesignAdded, Payload: core.JSONPayload(map[string]any{"id": document.ID, "version": 1})}); err != nil {
					t.Fatal(err)
				}
			}
			if test.attachBefore {
				attach()
			}
			if test.proposal {
				proposeDesignRevision(t, st, ctx, document.ID, delivery.ID)
			}
			if err := st.AppendEvent(ctx, core.Event{TaskID: delivery.ID, Kind: "merge.confirmed", Payload: core.JSONPayload(map[string]any{"repository": "kidus-tiliksew/conveyor", "head_sha": "lineaged-head"})}); err != nil {
				t.Fatal(err)
			}
			mergeEventID := latestEventID(t, st, ctx, delivery.ID, "merge.confirmed")
			if test.attachAfter {
				attach()
			}
			observation := monitor.Observation{Repository: "conveyor", Kind: monitor.LineagedMerge, OccurrenceID: "pr:520", SourceURL: "https://example.test/pull/520", CommitSHA: "lineaged-head", ChangedPaths: []string{"internal/dispatch/dispatch.go"}, CausalEventID: mergeEventID}
			if _, err := service.ProcessDesignMerge(ctx, observation, delivery.ID); err != nil {
				t.Fatal(err)
			}
			// Re-evaluation is expected after recovery and must not duplicate the
			// append-only delivery judgment.
			if _, err := service.ProcessDesignMerge(ctx, observation, delivery.ID); err != nil {
				t.Fatal(err)
			}
			status, err := service.Status(ctx)
			if err != nil {
				t.Fatal(err)
			}
			driftFound := false
			for _, drift := range status.Drift {
				driftFound = driftFound || drift.SystemDesignID == document.ID
			}
			if driftFound != test.wantDrift {
				t.Fatalf("design drift found=%t want=%t: %+v", driftFound, test.wantDrift, status.Drift)
			}
			events, err := st.ListSystemDesignEvents(ctx, document.ID)
			if err != nil {
				t.Fatal(err)
			}
			consultations := 0
			for _, event := range events {
				if event.Kind != "system_design.consulted" {
					continue
				}
				var payload struct {
					Version        int      `json:"version"`
					DeliveryTaskID string   `json:"delivery_task_id"`
					MergeEventID   int64    `json:"merge_event_id"`
					MergeHeadSHA   string   `json:"merge_head_sha"`
					MatchingPaths  []string `json:"matching_paths"`
					Consultation   string   `json:"consultation"`
				}
				if json.Unmarshal(event.Payload, &payload) == nil && payload.Consultation == "delivery_no_revision" {
					consultations++
					if payload.Version != 1 || payload.DeliveryTaskID != delivery.ID || payload.MergeEventID != mergeEventID || payload.MergeHeadSHA != "lineaged-head" || !slices.Equal(payload.MatchingPaths, []string{"internal/dispatch/dispatch.go"}) {
						t.Fatalf("consulted payload=%+v", payload)
					}
				}
			}
			if got := consultations == 1; got != test.wantConsultation {
				t.Fatalf("consultations=%d want_one=%t events=%+v", consultations, test.wantConsultation, events)
			}
			if test.wantConsultation {
				sibling := store.WithWorkspace(ctx, workspace+"-sibling")
				judgment, err := st.(monitor.Store).ResolveCausalSystemDesignMerge(sibling, document.ID, "conveyor", "lineaged-head", mergeEventID, "sibling-drift", []string{"internal/dispatch/dispatch.go"}, true)
				if err != nil || judgment.CausalEventValid || judgment.Consulted {
					t.Fatalf("workspace-isolated judgment=%+v err=%v", judgment, err)
				}
			}
		})
	}
	for _, kind := range []monitor.SignalKind{monitor.DirectPush, monitor.ExternalPRMerge, monitor.Revert} {
		t.Run(string(kind)+" keeps drift behavior for attached designs", func(t *testing.T) {
			st, ctx, workspace := factory(t)
			now := time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC)
			service := driftService(t, st, ctx, workspace, now)
			delivery := createDriftTask(t, st, ctx, workspace, "non-lineaged-delivery")
			document := createConfirmedDesign(t, st, ctx, "DESIGN-"+string(kind), "internal/dispatch/**")
			if err := st.AppendEvent(ctx, core.Event{TaskID: delivery.ID, Kind: store.TaskContextDesignAdded, Payload: core.JSONPayload(map[string]any{"id": document.ID, "version": 1})}); err != nil {
				t.Fatal(err)
			}
			if err := st.AppendEvent(ctx, core.Event{TaskID: delivery.ID, Kind: "merge.reconciled", Payload: core.JSONPayload(map[string]any{"head_sha": "non-lineaged-head"})}); err != nil {
				t.Fatal(err)
			}
			mergeEventID := latestEventID(t, st, ctx, delivery.ID, "merge.reconciled")
			if _, err := service.Process(ctx, monitor.Observation{Repository: "conveyor", Kind: kind, OccurrenceID: string(kind) + "-attached", SourceURL: "https://example.test/change", CommitSHA: "non-lineaged-head", ChangedPaths: []string{"internal/dispatch/dispatch.go"}, CausalEventID: mergeEventID}); err != nil {
				t.Fatal(err)
			}
			status, err := service.Status(ctx)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, drift := range status.Drift {
				found = found || drift.SystemDesignID == document.ID
			}
			if !found {
				t.Fatalf("%s design drift missing: %+v", kind, status.Drift)
			}
		})
	}
	for _, test := range []struct {
		name               string
		proposal           string
		wantDrift          bool
		wantCausalEvidence bool
	}{
		{name: "matching causal proposal suppresses", proposal: "matching", wantDrift: false},
		{name: "unrelated task proposal does not suppress", proposal: "unrelated", wantDrift: true, wantCausalEvidence: true},
		{name: "proposal after merge does not suppress", proposal: "after", wantDrift: true, wantCausalEvidence: true},
		{name: "different document proposal does not suppress", proposal: "different", wantDrift: true, wantCausalEvidence: true},
		{name: "non causal merge reference does not suppress", proposal: "invalid-causal", wantDrift: true, wantCausalEvidence: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			st, ctx, workspace := factory(t)
			now := time.Date(2026, 8, 5, 8, 0, 0, 0, time.UTC)
			service := &monitor.Service{Store: st.(monitor.Store), WorkspaceID: workspace, Enabled: true, Repositories: map[string]struct{}{"conveyor": {}}, Now: func() time.Time { return now }}
			service.Intake = func(ctx context.Context, request monitor.TaskRequest) (monitor.IntakeResult, error) {
				id := core.NewTaskID()
				task := core.Task{ID: id, Workspace: workspace, Repo: request.Repository, BaseBranch: "main", Branch: "conveyor/task-" + id, Source: request.Source, IntakeKey: request.IntakeKey, State: core.TaskQueued, NextStage: core.StageTriage, CreatedAt: now}
				return monitor.IntakeResult{Task: task, Created: true}, st.CreateTask(ctx, task)
			}

			delivery := createDriftTask(t, st, ctx, workspace, "delivery")
			other := createDriftTask(t, st, ctx, workspace, "other")
			document := createConfirmedDesign(t, st, ctx, "DESIGN-main", "internal/dispatch/**")
			if test.proposal == "matching" {
				proposeDesignRevision(t, st, ctx, document.ID, delivery.ID)
			}
			if test.proposal == "unrelated" {
				proposeDesignRevision(t, st, ctx, document.ID, other.ID)
			}
			if test.proposal == "different" {
				different := createConfirmedDesign(t, st, ctx, "DESIGN-other", "internal/httpapi/**")
				proposeDesignRevision(t, st, ctx, different.ID, delivery.ID)
			}

			if err := st.AppendEvent(ctx, core.Event{TaskID: delivery.ID, Kind: "merge.confirmed", Payload: core.JSONPayload(map[string]any{"repository": "kidus-tiliksew/conveyor", "base_sha": "base", "head_sha": "head"})}); err != nil {
				t.Fatal(err)
			}
			mergeEventID := latestEventID(t, st, ctx, delivery.ID, "merge.confirmed")
			if test.proposal == "after" {
				proposeDesignRevision(t, st, ctx, document.ID, delivery.ID)
			}
			causalEventID := mergeEventID
			if test.proposal == "invalid-causal" {
				causalEventID = latestEventID(t, st, ctx, other.ID, "task.created")
			}

			if _, err := service.Process(ctx, monitor.Observation{Repository: "conveyor", Kind: monitor.ExternalPRMerge, OccurrenceID: fmt.Sprintf("merge-%s", test.proposal), SourceURL: "https://example.test/merge/head", CommitSHA: "head", ChangedPaths: []string{"internal/dispatch/service.go"}, CausalEventID: causalEventID}); err != nil {
				t.Fatal(err)
			}
			status, err := service.Status(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var designDrift *monitor.Drift
			for i := range status.Drift {
				if status.Drift[i].SystemDesignID == document.ID {
					designDrift = &status.Drift[i]
				}
			}
			if (designDrift != nil) != test.wantDrift {
				t.Fatalf("design drift=%+v status=%+v", designDrift, status)
			}
			if designDrift != nil {
				if got := designDrift.CausalEventID != 0; got != test.wantCausalEvidence {
					t.Fatalf("causal evidence id=%d want_present=%t", designDrift.CausalEventID, test.wantCausalEvidence)
				}
				if test.wantCausalEvidence && designDrift.CausalEventID != mergeEventID {
					t.Fatalf("causal evidence id=%d want=%d", designDrift.CausalEventID, mergeEventID)
				}
			}
			if test.proposal == "matching" {
				activityFound := false
				for _, activity := range status.Activity {
					if activity.Kind == "system_design.drift_suppressed" && fmt.Sprint(activity.Payload["proposal_status"]) == "pending" {
						activityFound = true
					}
				}
				if !activityFound {
					t.Fatalf("suppression audit missing: %+v", status.Activity)
				}
				if err := st.AppendEvent(ctx, core.Event{TaskID: delivery.ID, Kind: "merge.confirmed", Payload: core.JSONPayload(map[string]any{"repository": "kidus-tiliksew/conveyor", "base_sha": "base-2", "head_sha": "head-2"})}); err != nil {
					t.Fatal(err)
				}
				secondMergeEventID := latestEventID(t, st, ctx, delivery.ID, "merge.confirmed")
				if _, err := service.Process(ctx, monitor.Observation{Repository: "conveyor", Kind: monitor.ExternalPRMerge, OccurrenceID: "merge-matching-second", SourceURL: "https://example.test/merge/head-2", CommitSHA: "head-2", ChangedPaths: []string{"internal/dispatch/second.go"}, CausalEventID: secondMergeEventID}); err != nil {
					t.Fatal(err)
				}
				status, err = service.Status(ctx)
				if err != nil {
					t.Fatal(err)
				}
				secondDrift := false
				for _, candidate := range status.Drift {
					secondDrift = secondDrift || (candidate.SystemDesignID == document.ID && candidate.CommitSHA == "head-2")
				}
				if !secondDrift {
					t.Fatalf("single-use proposal suppressed a later merge: %+v", status.Drift)
				}
			}
		})
	}

	t.Run("deduplicated enriched observation evaluates and persists design drift", func(t *testing.T) {
		st, ctx, workspace := factory(t)
		now := time.Date(2026, 8, 5, 9, 0, 0, 0, time.UTC)
		service := driftService(t, st, ctx, workspace, now)
		delivery := createDriftTask(t, st, ctx, workspace, "dedup-delivery")
		document := createConfirmedDesign(t, st, ctx, "DESIGN-dedup", "internal/dispatch/**")
		if err := st.AppendEvent(ctx, core.Event{TaskID: delivery.ID, Kind: "merge.confirmed", Payload: core.JSONPayload(map[string]any{"repository": "kidus-tiliksew/conveyor", "head_sha": "dedup-head"})}); err != nil {
			t.Fatal(err)
		}
		mergeEventID := latestEventID(t, st, ctx, delivery.ID, "merge.confirmed")
		base := monitor.Observation{Repository: "conveyor", Kind: monitor.ExternalPRMerge, OccurrenceID: "pr:77", SourceURL: "https://example.test/pull/77", CommitSHA: "dedup-head"}
		first, err := service.Process(ctx, base)
		if err != nil {
			t.Fatal(err)
		}
		base.ChangedPaths, base.CausalEventID = []string{"internal/dispatch/service.go"}, mergeEventID
		second, err := service.Process(ctx, base)
		if err != nil {
			t.Fatal(err)
		}
		if second.TaskID != first.TaskID || second.CausalEventID != mergeEventID || len(second.ChangedPaths) != 1 {
			t.Fatalf("enriched observation not retained: first=%+v second=%+v", first, second)
		}
		status, err := service.Status(ctx)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, drift := range status.Drift {
			found = found || drift.SystemDesignID == document.ID
		}
		if !found {
			t.Fatalf("enriched dedup did not evaluate design drift: %+v", status.Drift)
		}
	})

	t.Run("design resolution requires confirmed same-document replacement and emits events", func(t *testing.T) {
		st, ctx, workspace := factory(t)
		now := time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC)
		service := driftService(t, st, ctx, workspace, now)
		delivery := createDriftTask(t, st, ctx, workspace, "resolution-delivery")
		document := createConfirmedDesign(t, st, ctx, "DESIGN-resolution", "internal/dispatch/**")
		createConfirmedDesign(t, st, ctx, "DESIGN-unrelated-resolution", "cmd/**")
		plainID := "plain-resolution-drift"
		if _, _, err := st.(monitor.Store).RecordDrift(ctx, monitor.Drift{ID: plainID, WorkspaceID: workspace, Repository: "conveyor", Kind: monitor.DirectPush, SourceURL: "https://example.test/commit/plain", TaskID: delivery.ID, DetectedAt: now}); err != nil {
			t.Fatal(err)
		}
		if _, err := service.Resolve(ctx, plainID, "design_document_updated", ""); err == nil {
			t.Fatal("design-less drift resolved as design_document_updated")
		}
		if err := st.AppendEvent(ctx, core.Event{TaskID: delivery.ID, Kind: "merge.confirmed", Payload: core.JSONPayload(map[string]any{"repository": "kidus-tiliksew/conveyor", "head_sha": "resolution-head"})}); err != nil {
			t.Fatal(err)
		}
		mergeEventID := latestEventID(t, st, ctx, delivery.ID, "merge.confirmed")
		if _, err := service.ProcessDesignMerge(ctx, monitor.Observation{Repository: "conveyor", Kind: monitor.LineagedMerge, OccurrenceID: "pr:88", SourceURL: "https://example.test/pull/88", CommitSHA: "resolution-head", ChangedPaths: []string{"internal/dispatch/service.go"}, CausalEventID: mergeEventID}, delivery.ID); err != nil {
			t.Fatal(err)
		}
		status, _ := service.Status(ctx)
		var driftID string
		for _, drift := range status.Drift {
			if drift.SystemDesignID == document.ID {
				driftID = drift.ID
			}
		}
		if driftID == "" {
			t.Fatalf("design drift missing: %+v", status.Drift)
		}
		if _, err := service.Resolve(ctx, driftID, "design_document_updated", ""); err == nil {
			t.Fatal("design drift resolved without a replacement version")
		}
		content := "# Replacement\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/dispatch/**\n```"
		replacement, err := st.ProposeSystemDesignVersion(ctx, core.SystemDesignVersion{DocumentID: document.ID, Content: content, Origin: core.SystemDesignOriginOperator})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = service.Resolve(ctx, driftID, "design_document_updated", ""); err == nil {
			t.Fatal("design drift resolved against an unconfirmed replacement")
		}
		if _, _, err = st.ConfirmSystemDesignVersion(ctx, document.ID, replacement.Version); err != nil {
			t.Fatal(err)
		}
		status, err = service.Status(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, drift := range status.Drift {
			if drift.ID == driftID {
				t.Fatalf("confirmed replacement did not reconcile drift: %+v", drift)
			}
		}
		if _, err = service.Resolve(ctx, driftID, "design_document_updated", ""); err != nil {
			t.Fatal(err)
		}
		events, err := st.ListSystemDesignEvents(ctx, document.ID)
		if err != nil {
			t.Fatal(err)
		}
		detected, resolved, namedVersion := false, false, false
		for _, event := range events {
			detected = detected || event.Kind == "system_design.drift_detected"
			resolved = resolved || event.Kind == "system_design.drift_resolved"
			var payload map[string]any
			if event.Kind == "system_design.drift_resolved" && json.Unmarshal(event.Payload, &payload) == nil {
				namedVersion = namedVersion || fmt.Sprint(payload["confirmed_version"]) == fmt.Sprint(replacement.Version)
			}
		}
		if !detected || !resolved || !namedVersion {
			t.Fatalf("drift lifecycle events missing: %+v", events)
		}
	})

	t.Run("unresolved task drift saturates at the shared bound", func(t *testing.T) {
		st, ctx, workspace := factory(t)
		task := createDriftTask(t, st, ctx, workspace, "saturated-drift-task")
		monitorStore := st.(monitor.Store)
		detectedAt := time.Now().UTC()
		for index := 0; index < monitor.MaxUnresolvedDriftPerTask; index++ {
			drift := monitor.Drift{
				ID: fmt.Sprintf("saturation-%d", index), WorkspaceID: workspace, Repository: "conveyor",
				Kind: monitor.DirectPush, SourceURL: fmt.Sprintf("https://example.test/commit/%d", index),
				TaskID: task.ID, DetectedAt: detectedAt.Add(time.Duration(index) * time.Second),
			}
			if _, fresh, err := monitorStore.RecordDrift(ctx, drift); err != nil || !fresh {
				t.Fatalf("record drift %d fresh=%t err=%v", index, fresh, err)
			}
			if index == 0 {
				if _, duplicateFresh, duplicateErr := monitorStore.RecordDrift(ctx, drift); duplicateErr != nil || duplicateFresh {
					t.Fatalf("duplicate drift fresh=%t err=%v", duplicateFresh, duplicateErr)
				}
			}
		}
		_, _, err := monitorStore.RecordDrift(ctx, monitor.Drift{
			ID: "saturation-refused", WorkspaceID: workspace, Repository: "conveyor", Kind: monitor.Revert,
			SourceURL: "https://example.test/commit/refused", TaskID: task.ID, DetectedAt: detectedAt.Add(time.Hour),
		})
		if !errors.Is(err, monitor.ErrTaskDriftSaturated) || !strings.Contains(err.Error(), task.ID) {
			t.Fatalf("saturation error=%v", err)
		}
		status, err := monitorStore.MonitorStatus(ctx, true, detectedAt.Add(2*time.Hour))
		if err != nil || status.DriftCount != monitor.MaxUnresolvedDriftPerTask {
			t.Fatalf("drift count=%d err=%v", status.DriftCount, err)
		}
		observation := monitor.Observation{
			WorkspaceID: workspace, Repository: "conveyor", Kind: monitor.LineagedMerge,
			OccurrenceID: "saturated-link", SourceURL: "https://example.test/pull/saturated", ObservedAt: detectedAt.Add(3 * time.Hour),
		}
		if _, _, err = monitorStore.Observe(ctx, observation); err != nil {
			t.Fatal(err)
		}
		if _, err = monitorStore.LinkTask(ctx, observation.Identity(), task.ID, "reused"); !errors.Is(err, monitor.ErrTaskDriftSaturated) || !strings.Contains(err.Error(), task.ID) {
			t.Fatalf("link saturation error=%v", err)
		}
	})

	t.Run("confirmation leaves ineligible drift unresolved", func(t *testing.T) {
		st, ctx, workspace := factory(t)
		task := createDriftTask(t, st, ctx, workspace, "ineligible-reconciliation")
		document := createConfirmedDesign(t, st, ctx, "DESIGN-ineligible", "internal/store/**")
		other := createConfirmedDesign(t, st, ctx, "DESIGN-other", "cmd/**")
		content := "# Replacement\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/store/**\n```"
		replacement, err := st.ProposeSystemDesignVersion(ctx, core.SystemDesignVersion{DocumentID: document.ID, Content: content, Origin: core.SystemDesignOriginOperator})
		if err != nil {
			t.Fatal(err)
		}
		drifts := []monitor.Drift{
			{ID: "proposal-before-detection", WorkspaceID: workspace, Repository: "conveyor", Kind: monitor.LineagedMerge, SourceURL: "https://example.test/pull/late", SystemDesignID: document.ID, SystemDesignVersion: 1, TaskID: task.ID, DetectedAt: replacement.CreatedAt.Add(time.Second)},
			{ID: "non-lineaged-kind", WorkspaceID: workspace, Repository: "conveyor", Kind: monitor.DirectPush, SourceURL: "https://example.test/commit/direct", SystemDesignID: document.ID, SystemDesignVersion: 1, TaskID: task.ID, DetectedAt: replacement.CreatedAt.Add(-time.Second)},
			{ID: "other-document", WorkspaceID: workspace, Repository: "conveyor", Kind: monitor.LineagedMerge, SourceURL: "https://example.test/pull/other", SystemDesignID: document.ID, SystemDesignVersion: 1, TaskID: task.ID, DetectedAt: replacement.CreatedAt.Add(-time.Second)},
		}
		for _, drift := range drifts {
			if _, _, err = st.(monitor.Store).RecordDrift(ctx, drift); err != nil {
				t.Fatal(err)
			}
		}
		otherRevision, err := st.ProposeSystemDesignVersion(ctx, core.SystemDesignVersion{DocumentID: other.ID, Content: "# Other replacement\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - cmd/**\n```", Origin: core.SystemDesignOriginOperator})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = st.ConfirmSystemDesignVersion(ctx, other.ID, otherRevision.Version); err != nil {
			t.Fatal(err)
		}
		status, err := st.(monitor.Store).MonitorStatus(ctx, true, time.Now().UTC())
		if err != nil || status.DriftCount != len(drifts) {
			t.Fatalf("unrelated confirmation drift count=%d err=%v", status.DriftCount, err)
		}
		if _, _, err = st.ConfirmSystemDesignVersion(ctx, document.ID, replacement.Version); err != nil {
			t.Fatal(err)
		}
		status, err = st.(monitor.Store).MonitorStatus(ctx, true, time.Now().UTC())
		if err != nil || status.DriftCount != 2 {
			t.Fatalf("ineligible drift count=%d err=%v drift=%+v", status.DriftCount, err, status.Drift)
		}
	})

	t.Run("concurrent fresh drift cannot exceed the task bound", func(t *testing.T) {
		st, ctx, workspace := factory(t)
		task := createDriftTask(t, st, ctx, workspace, "concurrent-saturation")
		monitorStore := st.(monitor.Store)
		errorsByCall := make(chan error, monitor.MaxUnresolvedDriftPerTask+1)
		var ready sync.WaitGroup
		start := make(chan struct{})
		for index := 0; index <= monitor.MaxUnresolvedDriftPerTask; index++ {
			ready.Add(1)
			go func(index int) {
				ready.Done()
				<-start
				_, _, err := monitorStore.RecordDrift(ctx, monitor.Drift{
					ID: fmt.Sprintf("concurrent-saturation-%d", index), WorkspaceID: workspace,
					Repository: "conveyor", Kind: monitor.LineagedMerge,
					SourceURL: fmt.Sprintf("https://example.test/pull/concurrent-%d", index),
					TaskID:    task.ID, DetectedAt: time.Now().UTC(),
				})
				errorsByCall <- err
			}(index)
		}
		ready.Wait()
		close(start)
		saturated := 0
		for index := 0; index <= monitor.MaxUnresolvedDriftPerTask; index++ {
			err := <-errorsByCall
			if errors.Is(err, monitor.ErrTaskDriftSaturated) {
				saturated++
			} else if err != nil {
				t.Fatal(err)
			}
		}
		status, err := monitorStore.MonitorStatus(ctx, true, time.Now().UTC())
		if err != nil || saturated != 1 || status.DriftCount != monitor.MaxUnresolvedDriftPerTask {
			t.Fatalf("saturated=%d drift_count=%d err=%v", saturated, status.DriftCount, err)
		}
	})
}

func driftService(t *testing.T, st store.Store, ctx context.Context, workspace string, now time.Time) *monitor.Service {
	t.Helper()
	service := &monitor.Service{Store: st.(monitor.Store), WorkspaceID: workspace, Enabled: true, Repositories: map[string]struct{}{"conveyor": {}}, Now: func() time.Time { return now }}
	service.Intake = func(ctx context.Context, request monitor.TaskRequest) (monitor.IntakeResult, error) {
		id := core.NewTaskID()
		task := core.Task{ID: id, Workspace: workspace, Repo: request.Repository, BaseBranch: "main", Branch: "conveyor/task-" + id, Source: request.Source, IntakeKey: request.IntakeKey, State: core.TaskQueued, NextStage: core.StageTriage, CreatedAt: now}
		return monitor.IntakeResult{Task: task, Created: true}, st.CreateTask(ctx, task)
	}
	return service
}

func createDriftTask(t *testing.T, st store.Store, ctx context.Context, workspace, suffix string) core.Task {
	t.Helper()
	id := core.NewTaskID()
	task := core.Task{ID: id, Workspace: workspace, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + id, Title: suffix, State: core.TaskQueued, NextStage: core.StageTriage, CreatedAt: time.Now().UTC()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	return task
}

func createConfirmedDesign(t *testing.T, st store.Store, ctx context.Context, id, path string) core.SystemDesign {
	t.Helper()
	content := fmt.Sprintf("# Design\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - %s\n```", path)
	document, version, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: id, Title: strings.ReplaceAll(id, "-", " "), Category: "Architecture"}, core.SystemDesignVersion{Content: content, Origin: core.SystemDesignOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmSystemDesignVersion(ctx, document.ID, version.Version, 0); err != nil {
		t.Fatal(err)
	}
	return document
}

func proposeDesignRevision(t *testing.T, st store.Store, ctx context.Context, documentID, taskID string) {
	t.Helper()
	content := "# Revised design\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/dispatch/**\n```"
	if _, err := st.ProposeSystemDesignVersion(ctx, core.SystemDesignVersion{DocumentID: documentID, Content: content, Origin: core.SystemDesignOriginImplementation, OriginTaskID: taskID}); err != nil {
		t.Fatal(err)
	}
}

func latestEventID(t *testing.T, st store.Store, ctx context.Context, taskID, kind string) int64 {
	t.Helper()
	events, err := st.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind == kind {
			return events[i].ID
		}
	}
	t.Fatalf("task %s has no %s event", taskID, kind)
	return 0
}

package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// RunReferenceDocumentConformance verifies the shared reference-document
// contract against both memory and Postgres adapters.
func RunReferenceDocumentConformance(t *testing.T, st store.Store, ctx context.Context) {
	t.Helper()
	documentID := "ref-" + core.NewTaskID()
	document, first, err := st.CreateReferenceDocument(ctx,
		core.ReferenceDocument{ID: documentID, Name: "Product Overview " + documentID},
		core.ReferenceDocumentVersion{Filename: "overview.md", ContentType: "text/markdown", Content: "# Product\n\nInitial."})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.CreateReferenceDocument(ctx,
		core.ReferenceDocument{ID: "ref-" + core.NewTaskID(), Name: strings.ToUpper(document.Name)},
		core.ReferenceDocumentVersion{Filename: "duplicate.md", ContentType: "text/markdown", Content: "# Duplicate"}); err == nil {
		t.Fatal("case-insensitive live-name duplicate was accepted")
	}

	results := make(chan struct {
		version core.ReferenceDocumentVersion
		err     error
	}, 2)
	for index := range 2 {
		go func(index int) {
			version, supersedeErr := st.SupersedeReferenceDocument(ctx, document.ID,
				core.ReferenceDocumentVersion{Filename: "overview.md", ContentType: "text/markdown", Content: fmt.Sprintf("# Product\n\nRevision %d.", index+2)})
			results <- struct {
				version core.ReferenceDocumentVersion
				err     error
			}{version, supersedeErr}
		}(index)
	}
	versions := []int{first.Version}
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatal(result.err)
		}
		versions = append(versions, result.version.Version)
	}
	sort.Ints(versions)
	if fmt.Sprint(versions) != "[1 2 3]" {
		t.Fatalf("serialized supersede versions=%v, want [1 2 3]", versions)
	}

	session, err := st.CreatePlanningSession(ctx, core.PlanningSession{ID: "session-" + core.NewTaskID(), Title: "Reference consultation"})
	if err != nil {
		t.Fatal(err)
	}
	if err = st.RecordReferenceDocumentConsulted(ctx, document.ID, 99, session.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing version consultation error=%v, want ErrNotFound", err)
	}
	if err = st.RecordReferenceDocumentConsulted(ctx, document.ID, 3, "session-missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing session consultation error=%v, want ErrNotFound", err)
	}
	if err = st.RecordReferenceDocumentConsulted(ctx, document.ID, 3, session.ID); err != nil {
		t.Fatal(err)
	}

	if err = st.DeleteReferenceDocument(ctx, document.ID); err != nil {
		t.Fatal(err)
	}
	if err = st.DeleteReferenceDocument(ctx, document.ID); err != nil {
		t.Fatalf("idempotent repeated delete: %v", err)
	}
	if live, listErr := st.ListReferenceDocuments(ctx, false); listErr != nil || containsReferenceDocument(live, document.ID) {
		t.Fatalf("live documents after delete=%+v err=%v", live, listErr)
	}
	if all, listErr := st.ListReferenceDocuments(ctx, true); listErr != nil || !containsReferenceDocument(all, document.ID) {
		t.Fatalf("all documents after delete=%+v err=%v", all, listErr)
	}
	events, err := st.ListReferenceDocumentEvents(ctx, document.ID)
	if err != nil {
		t.Fatal(err)
	}
	deleteEvents := 0
	for _, event := range events {
		if event.TaskID != "" {
			t.Fatalf("reference event is task-bound: %+v", event)
		}
		if event.Kind == "reference_document.deleted" {
			deleteEvents++
			var payload map[string]any
			if json.Unmarshal(event.Payload, &payload) != nil || payload["version"] != float64(3) {
				t.Fatalf("delete payload=%s, want current version 3", event.Payload)
			}
		}
	}
	if deleteEvents != 1 {
		t.Fatalf("delete events=%d, want exactly one after repeated delete", deleteEvents)
	}
	if _, _, err = st.CreateReferenceDocument(ctx,
		core.ReferenceDocument{ID: "ref-" + core.NewTaskID(), Name: document.Name},
		core.ReferenceDocumentVersion{Filename: "replacement.md", ContentType: "text/markdown", Content: "# Replacement"}); err != nil {
		t.Fatalf("reusing a soft-deleted document name: %v", err)
	}
}

func containsReferenceDocument(documents []core.ReferenceDocument, id string) bool {
	for _, document := range documents {
		if document.ID == id {
			return true
		}
	}
	return false
}

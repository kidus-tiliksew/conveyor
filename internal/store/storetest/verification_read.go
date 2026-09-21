package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/verification"
)

// RunVerificationReadCorruption proves that a metadata page does not decode a
// retained payload and that explicit detail still verifies its canonical digest.
func RunVerificationReadCorruption(t *testing.T, x Fixture, replaceBody func(context.Context, string, []byte) error) {
	v := newVerificationFixture(t, x)
	ctx, owner := bootstrapOwner(t, x)
	a := store.VerificationAccess{TaskID: v.access.TaskID, UserID: owner.ID}
	r := x.Backend.(store.VerificationReader)
	e := v.envelope("corruption-check", "corruption-check", "original")
	v.apply(t, store.VerificationCommand{Kind: store.VerificationWriteEvidence, Key: e.SubmissionKey, Evidence: []json.RawMessage{verificationBytes(e)}})
	detail, err := r.ReadVerificationDetail(ctx, a, v.contextID, e.ID)
	requireOK(t, err)
	detail.Envelope.Payload = core.JSONPayload(map[string]string{"invalid": strings.Repeat("private-corruption", 4096)})
	requireOK(t, replaceBody(ctx, e.ID, verificationBytes(detail)))
	page, err := r.ReadVerificationPage(ctx, a, store.VerificationPageRequest{Kind: "evidence", ContextID: v.contextID})
	requireOK(t, err)
	if len(page.Items) != 1 || strings.Contains(string(verificationBytes(page)), "private-corruption") {
		t.Fatal("metadata decoded or disclosed a retained payload")
	}
	got, err := r.ReadVerificationDetail(ctx, a, v.contextID, e.ID)
	if !errors.Is(err, store.ErrVerificationInvalid) || !reflect.DeepEqual(got, store.VerificationEvidenceRecord{}) {
		t.Fatalf("corrupt envelope exposed: %v", err)
	}
}

// RunVerificationRead exercises the same public contract on every backend.
func RunVerificationRead(t *testing.T, x Fixture) {
	v := newVerificationFixture(t, x)
	ctx, owner := bootstrapOwner(t, x)
	a := store.VerificationAccess{TaskID: v.access.TaskID, UserID: owner.ID}
	r, ok := x.Backend.(store.VerificationReader)
	if !ok {
		t.Fatal("backend lacks bounded verification reader")
	}
	marker := "private-payload-" + strings.Repeat("x", 32768)
	initialContext := v.ctx
	fixed := time.Now().UTC()
	v.ctx = store.WithVerificationClockForTest(v.ctx, func() time.Time { return fixed })
	for _, id := range []string{"read-one", "read-two", "read-three"} {
		e := v.envelope(id, id, marker)
		v.apply(t, store.VerificationCommand{Kind: store.VerificationWriteEvidence, Key: id, Evidence: []json.RawMessage{verificationBytes(e)}})
	}
	v.ctx = initialContext
	p := store.VerificationPageRequest{Kind: "contexts", Limit: 1}
	page, err := r.ReadVerificationPage(ctx, a, p)
	requireOK(t, err)
	if len(page.Items) != 1 || page.Items[0].ID != v.contextID {
		t.Fatalf("context discovery: %+v", page)
	}
	p = store.VerificationPageRequest{Kind: "evidence", ContextID: v.contextID, Limit: 1}
	seen := map[string]bool{}
	var ordered []string
	var firstCursor string
	for {
		page, err = r.ReadVerificationPage(ctx, a, p)
		requireOK(t, err)
		encoded := string(verificationBytes(page))
		if firstCursor == "" {
			firstCursor = page.NextCursor
		}
		if strings.Contains(encoded, "private-payload") || strings.Contains(encoded, "safe_inputs") || strings.Contains(encoded, "payload") || len(page.Items) > 1 {
			t.Fatalf("unbounded metadata: %s", encoded)
		}
		for _, item := range page.Items {
			if seen[item.ID] {
				t.Fatal("duplicate cursor row")
			}
			seen[item.ID] = true
			ordered = append(ordered, item.ID)
		}
		if page.NextCursor == "" {
			break
		}
		p.Cursor = page.NextCursor
	}
	if len(seen) != 3 {
		t.Fatalf("missing page rows: %+v", seen)
	}
	if !reflect.DeepEqual(ordered, []string{"read-two", "read-three", "read-one"}) {
		t.Fatalf("timestamp tie ordering changed: %v", ordered)
	}
	for _, request := range []struct {
		ctx    context.Context
		access store.VerificationAccess
		page   store.VerificationPageRequest
	}{
		{ctx, a, store.VerificationPageRequest{Kind: "attempts", ContextID: v.contextID, Cursor: firstCursor}},
		{ctx, a, store.VerificationPageRequest{Kind: "evidence", ContextID: "other-context", Cursor: firstCursor}},
		{ctx, store.VerificationAccess{TaskID: "other-task", UserID: owner.ID}, store.VerificationPageRequest{Kind: "evidence", ContextID: v.contextID, Cursor: firstCursor}},
		{store.WithWorkspace(ctx, "other-workspace"), a, store.VerificationPageRequest{Kind: "evidence", ContextID: v.contextID, Cursor: firstCursor}},
	} {
		if _, err := r.ReadVerificationPage(request.ctx, request.access, request.page); !errors.Is(err, store.ErrVerificationInvalid) {
			t.Fatalf("cursor escaped its scope: %v", err)
		}
	}
	for _, bad := range []store.VerificationPageRequest{{Kind: "evidence", ContextID: v.contextID, Limit: 51}, {Kind: "evidence", ContextID: v.contextID, Limit: -1}, {Kind: "evidence", ContextID: v.contextID, Cursor: "bad"}, {Kind: "unknown"}} {
		if _, err = r.ReadVerificationPage(ctx, a, bad); !errors.Is(err, store.ErrVerificationInvalid) {
			t.Fatalf("invalid page accepted: %v", err)
		}
	}
	full, err := r.ReadVerificationDetail(ctx, a, v.contextID, "read-one")
	requireOK(t, err)
	if !strings.Contains(string(full.Envelope.Payload), marker) {
		t.Fatal("explicit detail lost evidence")
	}
	foreign := newVerificationFixture(t, x)
	for _, request := range []struct {
		a    store.VerificationAccess
		c, e string
	}{{a, foreign.contextID, "read-one"}, {store.VerificationAccess{TaskID: foreign.access.TaskID, UserID: owner.ID}, v.contextID, "read-one"}, {store.VerificationAccess{TaskID: a.TaskID, UserID: "foreign-user"}, v.contextID, "read-one"}} {
		got, err := r.ReadVerificationDetail(ctx, request.a, request.c, request.e)
		if !errors.Is(err, store.ErrVerificationAccess) || !reflect.DeepEqual(got, store.VerificationEvidenceRecord{}) {
			t.Fatalf("foreign evidence: %+v %v", got, err)
		}
	}
	if page, err := r.ReadVerificationPage(store.WithWorkspace(ctx, "other-workspace"), a, store.VerificationPageRequest{Kind: "evidence", ContextID: v.contextID}); !errors.Is(err, store.ErrVerificationAccess) || len(page.Items) != 0 {
		t.Fatalf("foreign workspace page: %+v %v", page, err)
	}
	input := store.VerificationOperatorObservation{ContextID: v.contextID, RunID: v.runID, Key: "operator-observation", Fact: "The expected state is visible.", Supporting: []core.VerificationReference{{EvidenceID: "read-one"}}}
	var wg sync.WaitGroup
	receipts := make([]store.VerificationReceipt, 4)
	errs := make([]error, 4)
	for i := range receipts {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			receipts[i], errs[i] = store.RecordVerificationOperatorObservation(ctx, x.Backend, a, input)
		}(i)
	}
	wg.Wait()
	for i := range receipts {
		requireOK(t, errs[i])
		if !reflect.DeepEqual(receipts[0], receipts[i]) {
			t.Fatal("concurrent replay changed receipt")
		}
	}
	observation, err := r.ReadVerificationDetail(ctx, a, v.contextID, receipts[0].EvidenceIDs[0])
	requireOK(t, err)
	var fact core.OperatorObservationPayload
	requireOK(t, json.Unmarshal(observation.Envelope.Payload, &fact))
	if fact.OperatorID != store.UserActorID(owner.ID) || fact.CapturedAt != observation.Envelope.CapturedAt || observation.Envelope.WorkOrderID != v.access.WorkOrderID || observation.Envelope.CapturedBy.Attribution != "authenticated_operator" {
		t.Fatalf("wrong server attribution: %+v", observation)
	}
	replay, err := store.RecordVerificationOperatorObservation(ctx, x.Backend, a, input)
	requireOK(t, err)
	if !reflect.DeepEqual(replay, receipts[0]) {
		t.Fatal("later replay changed receipt")
	}
	input.Fact = "Different fact"
	if _, err = store.RecordVerificationOperatorObservation(ctx, x.Backend, a, input); !errors.Is(err, store.ErrVerificationConflict) {
		t.Fatalf("changed observation accepted: %v", err)
	}
	again, err := r.ReadVerificationDetail(ctx, a, v.contextID, receipts[0].EvidenceIDs[0])
	requireOK(t, err)
	if !reflect.DeepEqual(again, observation) {
		t.Fatal("replay changed first capture")
	}
	t.Run("LinkedArtifactIntegrity", func(t *testing.T) {
		data := []byte("read boundary artifact")
		hash := verificationSHA(data)
		v.apply(t, store.VerificationCommand{Kind: store.VerificationStageChunk, Chunk: &store.VerificationUploadChunk{UploadID: "read-upload", Content: data}})
		e := v.envelope("read-artifact", "read-artifact", "observed")
		e.Artifacts = []core.VerificationArtifactReference{{ArtifactID: hash, SHA256: hash, MediaType: "text/plain"}}
		v.apply(t, store.VerificationCommand{Kind: store.VerificationWriteEvidence, Key: e.SubmissionKey, Evidence: []json.RawMessage{verificationBytes(e)}, Artifacts: []store.VerificationArtifactInput{{UploadID: "read-upload", Name: "capture.txt", ContentType: "text/plain", SizeBytes: int64(len(data)), SHA256: hash}}})
		detail, err := r.ReadVerificationDetail(ctx, a, v.contextID, e.ID)
		requireOK(t, err)
		meta, content, err := x.Backend.ReadVerificationArtifact(ctx, a, detail.Envelope.ID, hash)
		requireOK(t, err)
		requireOK(t, store.VerifyRetainedVerificationArtifact(detail.Envelope.Artifacts[0], content))
		if string(content) != string(data) {
			t.Fatal("authorized artifact changed")
		}
		for _, access := range []store.VerificationAccess{{TaskID: foreign.access.TaskID, UserID: owner.ID}, {TaskID: a.TaskID, UserID: "foreign-user"}} {
			_, content, err := x.Backend.ReadVerificationArtifact(ctx, access, detail.Envelope.ID, hash)
			if !errors.Is(err, store.ErrVerificationAccess) || len(content) != 0 {
				t.Fatalf("foreign artifact exposed: %v", err)
			}
		}
		if x.SeedArtifact != nil {
			x.SeedArtifact(t, ctx, meta, []byte("corrupted artifact"))
			_, content, err = x.Backend.ReadVerificationArtifact(ctx, a, detail.Envelope.ID, hash)
			if err == nil {
				err = store.VerifyRetainedVerificationArtifact(detail.Envelope.Artifacts[0], content)
			}
			if err == nil {
				t.Fatal("corrupted retained artifact accepted")
			}
			x.SeedArtifact(t, ctx, meta, data)
		}
	})
	t.Run("HistoricalAndSealedContexts", func(t *testing.T) {
		oldContext := v.contextID
		v.apply(t, store.VerificationCommand{Kind: store.VerificationTerminateAttempt, Attempt: &store.VerificationAttempt{State: "failed", Explanation: "Historical failure"}})
		selection := v.snapshot(t).Selections[0]
		newContext := v.apply(t, store.VerificationCommand{Kind: store.VerificationCreateContext, Key: "new-cycle", Context: &store.VerificationContext{Revisions: v.revisions, GoverningPins: v.pins, Discovery: []json.RawMessage{core.JSONPayload(map[string]any{"repository": "conveyor", "revision": v.revisions[0].SHA, "state": "no_manifest"})}}}).ID
		v.contextID = newContext
		v.apply(t, store.VerificationCommand{Kind: store.VerificationRecordSelection, Selection: &selection})
		v.apply(t, store.VerificationCommand{Kind: store.VerificationSeal, Submission: &store.VerificationSubmission{Outcome: "succeeded", Coverage: verificationFixtureCoverage(v.snapshot(t))}})
		page, err := r.ReadVerificationPage(ctx, a, store.VerificationPageRequest{Kind: "contexts"})
		requireOK(t, err)
		if len(page.Items) != 2 || page.Items[0].ID != newContext || page.Items[1].ID != oldContext {
			t.Fatalf("history overwritten: %+v", page)
		}
		var header map[string]string
		requireOK(t, json.Unmarshal(page.Items[0].Metadata, &header))
		if header["outcome"] != "succeeded" || header["sealed_at"] == "" || header["deciding_actor"] != "worker:worker" {
			t.Fatalf("seal projection: %+v", header)
		}
		if _, err = r.ReadVerificationDetail(ctx, a, oldContext, "read-one"); err != nil {
			t.Fatalf("historical evidence inaccessible after seal: %v", err)
		}
		one, err := r.ReadVerificationPage(ctx, a, store.VerificationPageRequest{Kind: "contexts", ContextID: oldContext})
		requireOK(t, err)
		if len(one.Items) != 1 || one.Items[0].ID != oldContext {
			t.Fatal("exact historical context lookup failed")
		}
	})
	t.Run("RequiredAndOptionalAssertions", func(t *testing.T) {
		v := newVerificationFixture(t, x, true)
		obligation := v.apply(t, store.VerificationCommand{Kind: store.VerificationRegisterObligation, Obligation: &store.VerificationObligation{ID: "assertions", Description: "Assert observed state", Sources: []store.VerificationCitation{{DocumentID: "req-fixture", Version: 1, SectionID: "AC-1.1"}}, Contract: verification.Exercise{ID: "assertions", Kind: "script", Argv: []string{"true"}, TimeoutSeconds: 30, RequiredAssertions: []string{"required-check"}, RetryPolicy: "safe_to_replay", SafetyBasis: "read only"}}})
		v.subject = core.VerificationSubject{Kind: "ordinary", ObligationID: "assertions", ContractDigest: obligation.Digest}
		v.start(t, "assertion-start")
		observed := v.envelope("assertion-support", "support", "observed")
		v.apply(t, store.VerificationCommand{Kind: store.VerificationWriteEvidence, Key: "support", Evidence: []json.RawMessage{verificationBytes(observed)}})
		for _, id := range []string{"required-check", "optional-check"} {
			e := v.envelope(id, id, "")
			e.Type = "assertion_result"
			e.Payload = verificationBytes(core.AssertionResultPayload{AssertionID: id, Text: "Check state", Expected: "expected", Actual: "observed", Outcome: "fail", Supporting: []core.VerificationReference{{EvidenceID: "assertion-support"}}})
			v.apply(t, store.VerificationCommand{Kind: store.VerificationWriteEvidence, Key: id, Evidence: []json.RawMessage{verificationBytes(e)}})
		}
		a := store.VerificationAccess{TaskID: v.access.TaskID, UserID: owner.ID}
		page, err := r.ReadVerificationPage(ctx, a, store.VerificationPageRequest{Kind: "assertions", ContextID: v.contextID})
		requireOK(t, err)
		if len(page.Items) != 2 {
			t.Fatalf("assertion projection: %+v", page)
		}
		for _, item := range page.Items {
			var metadata map[string]string
			requireOK(t, json.Unmarshal(item.Metadata, &metadata))
			want := "false"
			if metadata["assertion_id"] == "required-check" {
				want = "true"
			}
			if metadata["required"] != want || metadata["outcome"] != "fail" {
				t.Fatalf("required assertion flag or optional failure lost: %+v", metadata)
			}
		}
	})
	t.Run("KitSelectionPagination", func(t *testing.T) {
		v := newVerificationFixture(t, x, true)
		v.contextID = v.apply(t, store.VerificationCommand{Kind: store.VerificationCreateContext, Key: "kit-cycle", Context: &store.VerificationContext{Revisions: v.revisions, GoverningPins: v.pins, Discovery: []json.RawMessage{core.JSONPayload(map[string]any{"repository": "conveyor", "revision": v.revisions[0].SHA, "state": "manifest"})}}}).ID
		selection := store.VerificationSelection{Receipt: verification.SelectionReceipt{SchemaVersion: 1, Stage: "verify", ContextPins: []verification.Pin{{Kind: "requirement", DocumentID: "req-fixture", Version: 1}}, Kits: []verification.KitReceipt{}}}
		for _, id := range []string{"kit-a", "kit-c", "kit-b"} {
			selection.Receipt.Kits = append(selection.Receipt.Kits, verification.KitReceipt{KitID: id, Digest: strings.Repeat("d", 64), Eligibility: "eligible", Reasons: []verification.SelectionReason{{Code: "matching_pin", Message: strings.Repeat("selection reason ", 256)}}})
		}
		v.subject = core.VerificationSubject{Kind: "kit", KitID: "kit-a", KitVersion: "1", ContentDigest: strings.Repeat("d", 64), ExerciseID: "kit-check"}
		selection.Subjects = []store.VerificationSubjectContract{{Subject: v.subject, Contract: verification.Exercise{ID: "kit-check", Kind: "script", Argv: []string{"private-command"}, TimeoutSeconds: 30, RequiredAssertions: []string{"kit-assertion"}, RetryPolicy: "safe_to_replay", SafetyBasis: "read only"}}}
		v.apply(t, store.VerificationCommand{Kind: store.VerificationRecordSelection, Selection: &selection})
		a := store.VerificationAccess{TaskID: v.access.TaskID, UserID: owner.ID}
		p := store.VerificationPageRequest{Kind: "selections", ContextID: v.contextID, Limit: 1}
		for _, id := range []string{"kit-c", "kit-b", "kit-a"} {
			page, err := r.ReadVerificationPage(ctx, a, p)
			requireOK(t, err)
			if len(page.Items) != 1 {
				t.Fatalf("kit page: %+v", page)
			}
			var metadata map[string]string
			requireOK(t, json.Unmarshal(page.Items[0].Metadata, &metadata))
			if metadata["kit_id"] != id || metadata["truncated"] != "true" || len([]rune(metadata["reasons"])) > 2048 || strings.Contains(string(page.Items[0].Metadata), "private-command") {
				t.Fatalf("kit metadata: %+v", metadata)
			}
			p.Cursor = page.NextCursor
		}
		if p.Cursor != "" {
			t.Fatal("kit cursor did not end")
		}
		v.start(t, "kit-start")
		e := v.envelope("kit-support", "kit-support", "observed")
		v.apply(t, store.VerificationCommand{Kind: store.VerificationWriteEvidence, Key: e.SubmissionKey, Evidence: []json.RawMessage{verificationBytes(e)}})
		e = v.envelope("kit-assertion", "kit-assertion", "")
		e.Type = "assertion_result"
		e.Payload = verificationBytes(core.AssertionResultPayload{AssertionID: "kit-assertion", Text: "Check state", Expected: "expected", Actual: "observed", Outcome: "fail", Supporting: []core.VerificationReference{{EvidenceID: "kit-support"}}})
		v.apply(t, store.VerificationCommand{Kind: store.VerificationWriteEvidence, Key: e.SubmissionKey, Evidence: []json.RawMessage{verificationBytes(e)}})
		page, err := r.ReadVerificationPage(ctx, a, store.VerificationPageRequest{Kind: "assertions", ContextID: v.contextID})
		requireOK(t, err)
		if len(page.Items) != 1 {
			t.Fatalf("kit assertion page: %+v", page)
		}
		var metadata map[string]string
		requireOK(t, json.Unmarshal(page.Items[0].Metadata, &metadata))
		if metadata["required"] != "true" {
			t.Fatalf("kit assertion required flag lost: %+v", metadata)
		}
	})
}

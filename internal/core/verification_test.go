package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

const evidenceTime = "2026-09-20T10:00:00Z"

func evidencePayload(t *testing.T, v any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
func evidenceFixture(t *testing.T, kind string) (VerificationEvidence, VerificationEvidenceAuthority) {
	t.Helper()
	hash := strings.Repeat("a", 64)
	no := false
	zero := 0
	status := 200
	ref := VerificationReference{ArtifactID: "artifact", SHA256: hash}
	e := VerificationEvidence{SchemaVersion: 1, ID: "evidence", SubmissionKey: "submission", Type: kind, CapturedAt: evidenceTime, ReceivedAt: evidenceTime, SubmittedBy: "agent", CapturedBy: VerificationCaptureActor{Identity: "capture-tool", Kind: "tool", Version: "1", Attribution: "self_reported"}, WorkspaceID: "demo", TaskID: "task", WorkOrderID: "order", WorkOrderAttemptID: "order-attempt", ContextID: "context", RunID: "run", Subject: VerificationSubject{Kind: "kit", KitID: "kit", KitVersion: "1", ContentDigest: hash, ExerciseID: "exercise"}, Revisions: []VerificationRevision{{Repository: "repo", RemoteIdentity: "github.com/owner/repo", SHA: strings.Repeat("b", 40)}}, GoverningPins: []VerificationPin{{Kind: "requirement", DocumentID: "req-example", Version: 1}}, SafeInputs: map[string]json.RawMessage{}, Environment: VerificationEnvironment{Target: "unknown", OS: "unknown", Architecture: "unknown", Runtime: "unknown", Deployment: "unknown"}, Artifacts: []VerificationArtifactReference{{ArtifactID: "artifact", SHA256: hash, MediaType: "image/png"}}}
	authority := VerificationEvidenceAuthority{SubmittedBy: "agent"}
	switch kind {
	case "api_exchange":
		e.Payload = evidencePayload(t, APIExchangePayload{Method: "GET", URL: "https://fixture.example/api", RequestAt: evidenceTime, ResponseAt: evidenceTime, ResponseStatus: &status, RequestHeaders: map[string]string{}, ResponseHeaders: map[string]string{}, RequestSummary: "empty", ResponseSummary: "ok"})
	case "state_observation":
		e.Payload = evidencePayload(t, StateObservationPayload{Target: "fixture", Method: "query", CapturedAt: evidenceTime, Value: json.RawMessage(`{"count":1}`)})
	case "assertion_result":
		e.Payload = evidencePayload(t, AssertionResultPayload{AssertionID: "readable", Text: "record readable", Expected: "one record", Actual: "one record", Outcome: "pass", Supporting: []VerificationReference{{EvidenceID: "observation"}}})
	case "execution_report":
		e.Payload = evidencePayload(t, ExecutionReportPayload{Argv: []string{"check"}, Tool: "check", ToolVersion: "1", Runtime: "go", StartedAt: evidenceTime, EndedAt: evidenceTime, ExitCode: &zero, TimedOut: &no, Cancelled: &no, StdoutSHA256: hash, StderrSHA256: hash, StdoutTruncated: &no, StderrTruncated: &no})
	case "visual_capture":
		e.Payload = evidencePayload(t, VisualCapturePayload{Artifact: ref, MediaType: "image/png", CaptureTool: "browser", Target: "unknown", CapturedAt: evidenceTime})
	case "operator_observation":
		authority = VerificationEvidenceAuthority{SubmittedBy: "user", OperateGates: true}
		e.SubmittedBy = "user"
		e.CapturedBy = VerificationCaptureActor{Identity: "user", Kind: "operator", Version: "unknown", Attribution: "authenticated_operator"}
		e.Payload = evidencePayload(t, OperatorObservationPayload{OperatorID: "user", Fact: "Observed record", CapturedAt: evidenceTime, Supporting: []VerificationReference{ref}})
	}
	return e, authority
}
func TestVerificationSixSchemas(t *testing.T) {
	for _, kind := range []string{"api_exchange", "state_observation", "assertion_result", "execution_report", "visual_capture", "operator_observation"} {
		t.Run(kind, func(t *testing.T) {
			e, a := evidenceFixture(t, kind)
			data := evidencePayload(t, e)
			if _, err := DecodeVerificationEvidence(data, a); err != nil {
				t.Fatal(err)
			}
			e.Payload = json.RawMessage(`{}`)
			if err := e.Validate(a); err == nil {
				t.Fatal("accepted missing payload fields")
			}
		})
	}
}
func TestVerificationProvenanceRefusals(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*VerificationEvidence, *VerificationEvidenceAuthority)
	}{
		{"schema", func(e *VerificationEvidence, a *VerificationEvidenceAuthority) { e.SchemaVersion = 2 }},
		{"type", func(e *VerificationEvidence, a *VerificationEvidenceAuthority) { e.Type = "unknown" }},
		{"capture", func(e *VerificationEvidence, a *VerificationEvidenceAuthority) { e.CapturedAt = "" }},
		{"non UTC", func(e *VerificationEvidence, a *VerificationEvidenceAuthority) {
			e.CapturedAt = "2026-09-20T10:00:00+01:00"
		}},
		{"unknown offset", func(e *VerificationEvidence, a *VerificationEvidenceAuthority) {
			e.CapturedAt = "2026-09-20T10:00:00-00:00"
		}},
		{"receipt", func(e *VerificationEvidence, a *VerificationEvidenceAuthority) { e.ReceivedAt = "" }},
		{"context", func(e *VerificationEvidence, a *VerificationEvidenceAuthority) { e.ContextID = "" }},
		{"run", func(e *VerificationEvidence, a *VerificationEvidenceAuthority) { e.RunID = "" }},
		{"attempt", func(e *VerificationEvidence, a *VerificationEvidenceAuthority) { e.WorkOrderAttemptID = "" }},
		{"workspace", func(e *VerificationEvidence, a *VerificationEvidenceAuthority) { e.WorkspaceID = "" }},
		{"task", func(e *VerificationEvidence, a *VerificationEvidenceAuthority) { e.TaskID = "" }},
		{"order", func(e *VerificationEvidence, a *VerificationEvidenceAuthority) { e.WorkOrderID = "" }},
		{"id", func(e *VerificationEvidence, a *VerificationEvidenceAuthority) { e.ID = "" }},
		{"submission", func(e *VerificationEvidence, a *VerificationEvidenceAuthority) { e.SubmissionKey = "" }},
		{"revisions", func(e *VerificationEvidence, a *VerificationEvidenceAuthority) { e.Revisions = nil }},
		{"revision sha", func(e *VerificationEvidence, a *VerificationEvidenceAuthority) { e.Revisions[0].SHA = "main" }},
		{"pins", func(e *VerificationEvidence, a *VerificationEvidenceAuthority) { e.GoverningPins = nil }},
		{"safe inputs", func(e *VerificationEvidence, a *VerificationEvidenceAuthority) { e.SafeInputs = nil }},
		{"kit digest", func(e *VerificationEvidence, a *VerificationEvidenceAuthority) { e.Subject.ContentDigest = "bad" }},
		{"kit exercise", func(e *VerificationEvidence, a *VerificationEvidenceAuthority) { e.Subject.ExerciseID = "" }},
		{"kit mixed fields", func(e *VerificationEvidence, a *VerificationEvidenceAuthority) { e.Subject.ObligationID = "invented" }},
		{"subject", func(e *VerificationEvidence, a *VerificationEvidenceAuthority) { e.Subject.Kind = "synthetic" }},
		{"environment", func(e *VerificationEvidence, a *VerificationEvidenceAuthority) { e.Environment.Target = "" }},
		{"environment attribute", func(e *VerificationEvidence, a *VerificationEvidenceAuthority) {
			e.Environment.Attributes = map[string]string{"release": ""}
		}},
		{"capture identity", func(e *VerificationEvidence, a *VerificationEvidenceAuthority) { e.CapturedBy.Identity = "" }},
		{"capture version", func(e *VerificationEvidence, a *VerificationEvidenceAuthority) { e.CapturedBy.Version = "" }},
		{"forged submitter", func(e *VerificationEvidence, a *VerificationEvidenceAuthority) { e.SubmittedBy = "user" }},
		{"forged runner", func(e *VerificationEvidence, a *VerificationEvidenceAuthority) {
			e.CapturedBy.Attribution = "trusted_runner"
		}},
		{"forged operator", func(e *VerificationEvidence, a *VerificationEvidenceAuthority) { e.CapturedBy.Kind = "operator" }},
		{"hash", func(e *VerificationEvidence, a *VerificationEvidenceAuthority) { e.Artifacts[0].SHA256 = "bad" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, a := evidenceFixture(t, "state_observation")
			tt.mutate(&e, &a)
			if err := e.Validate(a); err == nil {
				t.Fatal("accepted invalid provenance")
			}
		})
	}
}
func TestVerificationOrdinaryAndAttribution(t *testing.T) {
	e, a := evidenceFixture(t, "state_observation")
	e.Subject = VerificationSubject{Kind: "ordinary", ObligationID: "ordinary-check", ContractDigest: strings.Repeat("a", 64)}
	if err := e.Validate(a); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"kit", "version", "digest", "exercise"} {
		copy := e
		switch field {
		case "kit":
			copy.Subject.KitID = "invented"
		case "version":
			copy.Subject.KitVersion = "invented"
		case "digest":
			copy.Subject.ContentDigest = strings.Repeat("a", 64)
		case "exercise":
			copy.Subject.ExerciseID = "invented"
		}
		if err := copy.Validate(a); err == nil {
			t.Fatal(field)
		}
	}
	e.Subject.ContractDigest = ""
	if err := e.Validate(a); err == nil {
		t.Fatal("missing ordinary digest")
	}
	e, a = evidenceFixture(t, "operator_observation")
	a.OperateGates = false
	if err := e.Validate(a); err == nil {
		t.Fatal("operator without capability")
	}
	e, a = evidenceFixture(t, "state_observation")
	e.CapturedBy.Attribution = "trusted_runner"
	a.TrustedRunner = true
	if err := e.Validate(a); err != nil {
		t.Fatal(err)
	}
}
func TestVerificationPayloadRefusals(t *testing.T) {
	tests := []struct{ kind, old, new string }{
		{"assertion_result", `"outcome":"pass"`, `"outcome":"accepted"`},
		{"assertion_result", `"evidence_id":"observation"`, `"evidence_id":"evidence"`},
		{"assertion_result", `"supporting":[{"evidence_id":"observation"}]`, `"supporting":[]`},
		{"assertion_result", `"assertion_id":"readable"`, `"assertion_id":"readable","required":false`},
		{"assertion_result", `"outcome":"pass"`, `"outcome":"pass","outcome":"fail"`},
		{"api_exchange", `"response_status":200`, `"response_status":999`},
		{"api_exchange", `https://fixture.example/api`, `https://secret:password@fixture.example/api`},
		{"api_exchange", `"request_headers":{}`, `"request_headers":{"Authorization":"secret"}`},
		{"execution_report", `"timed_out":false`, `"timed_out":null`},
		{"execution_report", `"exit_code":0,`, ``},
		{"visual_capture", `"media_type":"image/png"`, `"media_type":"text/html"`},
		{"visual_capture", `"artifact_id":"artifact"`, `"artifact_id":"foreign"`},
	}
	for _, tt := range tests {
		t.Run(tt.kind+tt.old, func(t *testing.T) {
			e, a := evidenceFixture(t, tt.kind)
			e.Payload = json.RawMessage(strings.Replace(string(e.Payload), tt.old, tt.new, 1))
			if err := e.Validate(a); err == nil {
				t.Fatalf("accepted %s", e.Payload)
			}
		})
	}
}
func TestVerificationDecodeSizeAndIntegrity(t *testing.T) {
	e, a := evidenceFixture(t, "state_observation")
	data := evidencePayload(t, e)
	if _, err := DecodeVerificationEvidence(append([]byte(strings.Repeat(" ", MaxVerificationEvidenceBytes)), data...), a); err == nil {
		t.Fatal("raw limit bypass")
	}
	e.Environment.Target = strings.Repeat("x", MaxVerificationEvidenceBytes)
	if err := e.Validate(a); err == nil {
		t.Fatal("encoded limit bypass")
	}
	data = []byte(strings.Replace(string(data), `"schema_version":1`, `"schema_version":1,"schema_version":1`, 1))
	if _, err := DecodeVerificationEvidence(data, a); err == nil {
		t.Fatal("duplicate envelope field")
	}
	sum := sha256.Sum256([]byte("retained"))
	hash := hex.EncodeToString(sum[:])
	if err := VerifyVerificationBytes([]byte("retained"), hash); err != nil {
		t.Fatal(err)
	}
	if err := VerifyVerificationBytes([]byte("tampered"), hash); err == nil {
		t.Fatal("hash mismatch accepted")
	}
}

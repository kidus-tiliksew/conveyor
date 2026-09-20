package core

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"
)

// VerificationEvidence is the schema-1, store-independent VK-5 contract
// (req-verification-kits REQ-5). Legacy visual artifacts retain their own role
// and media limits. Stored ownership, reference resolution and batch acyclicity
// belong to the verification service, not this structural validator.
const MaxVerificationEvidenceBytes = 256 << 10

type VerificationPin struct {
	Kind       string `json:"kind"`
	DocumentID string `json:"document_id"`
	Version    int    `json:"version"`
}
type VerificationRevision struct {
	Repository     string `json:"repository"`
	RemoteIdentity string `json:"remote_identity"`
	SHA            string `json:"sha"`
}
type VerificationSubject struct {
	Kind           string `json:"kind"`
	KitID          string `json:"kit_id,omitempty"`
	KitVersion     string `json:"kit_version,omitempty"`
	ContentDigest  string `json:"content_digest,omitempty"`
	ExerciseID     string `json:"exercise_id,omitempty"`
	ObligationID   string `json:"obligation_id,omitempty"`
	ContractDigest string `json:"contract_digest,omitempty"`
}
type VerificationCaptureActor struct {
	Identity    string `json:"identity"`
	Kind        string `json:"kind"` // actor or tool; operator requires authenticated authority
	Version     string `json:"version"`
	Attribution string `json:"attribution"` // self_reported, trusted_runner, authenticated_operator
}
type VerificationEnvironment struct {
	// Missing optional knowledge is represented by the literal "unknown". Empty
	// strings are never silently filled from the source checkout (AC-5.3).
	Target       string            `json:"target"`
	OS           string            `json:"os"`
	Architecture string            `json:"architecture"`
	Runtime      string            `json:"runtime"`
	Deployment   string            `json:"deployment"`
	Attributes   map[string]string `json:"attributes"`
}
type VerificationArtifactReference struct {
	ArtifactID string `json:"artifact_id"`
	SHA256     string `json:"sha256"`
	MediaType  string `json:"media_type"`
}
type VerificationReference struct {
	EvidenceID string `json:"evidence_id,omitempty"`
	ArtifactID string `json:"artifact_id,omitempty"`
	SHA256     string `json:"sha256,omitempty"`
}
type VerificationEvidence struct {
	SchemaVersion      int                             `json:"schema_version"`
	ID                 string                          `json:"id"`
	SubmissionKey      string                          `json:"submission_key"`
	Type               string                          `json:"type"`
	CapturedAt         string                          `json:"captured_at"`
	ReceivedAt         string                          `json:"received_at"`
	SubmittedBy        string                          `json:"submitted_by"`
	CapturedBy         VerificationCaptureActor        `json:"captured_by"`
	WorkspaceID        string                          `json:"workspace_id"`
	TaskID             string                          `json:"task_id"`
	WorkOrderID        string                          `json:"work_order_id"`
	WorkOrderAttemptID string                          `json:"work_order_attempt_id"`
	ContextID          string                          `json:"context_id"`
	RunID              string                          `json:"run_id"`
	Subject            VerificationSubject             `json:"subject"`
	Revisions          []VerificationRevision          `json:"revisions"`
	GoverningPins      []VerificationPin               `json:"governing_pins"`
	SafeInputs         map[string]json.RawMessage      `json:"safe_inputs"`
	Environment        VerificationEnvironment         `json:"environment"`
	Payload            json.RawMessage                 `json:"payload"`
	Artifacts          []VerificationArtifactReference `json:"artifacts"`
}

// VerificationEvidenceAuthority is supplied by an authenticated service/runner,
// never decoded from an evidence item. This slice does not implement access
// control or allow repository payloads to grant operator/runner authority.
type VerificationEvidenceAuthority struct {
	SubmittedBy   string
	OperateGates  bool
	TrustedRunner bool
}

type APIExchangePayload struct {
	Method          string                  `json:"method"`
	URL             string                  `json:"url"`
	RequestAt       string                  `json:"request_at"`
	ResponseAt      string                  `json:"response_at"`
	ResponseStatus  *int                    `json:"response_status,omitempty"`
	TransportError  string                  `json:"transport_error,omitempty"`
	RequestHeaders  map[string]string       `json:"request_headers"`
	ResponseHeaders map[string]string       `json:"response_headers"`
	RequestSummary  string                  `json:"request_summary"`
	ResponseSummary string                  `json:"response_summary"`
	BodyArtifacts   []VerificationReference `json:"body_artifacts,omitempty"`
}
type StateObservationPayload struct {
	Target     string                 `json:"target"`
	Method     string                 `json:"method"`
	CapturedAt string                 `json:"captured_at"`
	Value      json.RawMessage        `json:"value,omitempty"`
	Artifact   *VerificationReference `json:"artifact,omitempty"`
}
type AssertionResultPayload struct {
	AssertionID string                  `json:"assertion_id"`
	Text        string                  `json:"text"`
	Expected    string                  `json:"expected"`
	Actual      string                  `json:"actual"`
	Outcome     string                  `json:"outcome"`
	Supporting  []VerificationReference `json:"supporting"`
	// Required status deliberately has no submitted field (VK-10.4).
}
type ExecutionReportPayload struct {
	Argv            []string `json:"argv"`
	Tool            string   `json:"tool"`
	ToolVersion     string   `json:"tool_version"`
	Runtime         string   `json:"runtime"`
	StartedAt       string   `json:"started_at"`
	EndedAt         string   `json:"ended_at"`
	ExitCode        *int     `json:"exit_code,omitempty"`
	Signal          string   `json:"signal,omitempty"`
	TimedOut        *bool    `json:"timed_out"`
	Cancelled       *bool    `json:"cancelled"`
	StdoutSHA256    string   `json:"stdout_sha256"`
	StderrSHA256    string   `json:"stderr_sha256"`
	StdoutTruncated *bool    `json:"stdout_truncated"`
	StderrTruncated *bool    `json:"stderr_truncated"`
}
type VisualCapturePayload struct {
	Artifact    VerificationReference `json:"artifact"`
	MediaType   string                `json:"media_type"`
	CaptureTool string                `json:"capture_tool"`
	Target      string                `json:"target"`
	CapturedAt  string                `json:"captured_at"`
}
type OperatorObservationPayload struct {
	OperatorID string                  `json:"operator_id"`
	Fact       string                  `json:"fact"`
	CapturedAt string                  `json:"captured_at"`
	Supporting []VerificationReference `json:"supporting"`
}

// DecodeVerificationEvidence applies the byte limit to the actual submitted
// representation, before decoding, and rejects duplicate/unknown fields.
func DecodeVerificationEvidence(data []byte, authority VerificationEvidenceAuthority) (*VerificationEvidence, error) {
	if len(data) > MaxVerificationEvidenceBytes {
		return nil, fmt.Errorf("evidence: exceeds 256 KiB")
	}
	var e VerificationEvidence
	if err := decodeVerificationJSON(data, &e); err != nil {
		return nil, fmt.Errorf("evidence: %w", err)
	}
	if err := e.Validate(authority); err != nil {
		return nil, err
	}
	return &e, nil
}
func (e VerificationEvidence) Validate(authority VerificationEvidenceAuthority) error {
	data, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("evidence: %w", err)
	}
	if len(data) > MaxVerificationEvidenceBytes {
		return fmt.Errorf("evidence: exceeds 256 KiB")
	}
	if e.SchemaVersion != 1 {
		return fmt.Errorf("evidence.schema_version: unsupported schema")
	}
	for _, f := range []struct{ name, value string }{{"id", e.ID}, {"submission_key", e.SubmissionKey}, {"workspace_id", e.WorkspaceID}, {"task_id", e.TaskID}, {"work_order_id", e.WorkOrderID}, {"work_order_attempt_id", e.WorkOrderAttemptID}, {"context_id", e.ContextID}, {"run_id", e.RunID}, {"submitted_by", e.SubmittedBy}, {"captured_by.identity", e.CapturedBy.Identity}, {"captured_by.version", e.CapturedBy.Version}} {
		if strings.TrimSpace(f.value) == "" {
			return fmt.Errorf("evidence.%s: required", f.name)
		}
	}
	if e.SubmittedBy != authority.SubmittedBy || authority.SubmittedBy == "" {
		return fmt.Errorf("evidence.submitted_by: authenticated identity mismatch")
	}
	if _, err := verificationUTC(e.CapturedAt); err != nil {
		return fmt.Errorf("evidence.captured_at: %w", err)
	}
	if _, err := verificationUTC(e.ReceivedAt); err != nil {
		return fmt.Errorf("evidence.received_at: %w", err)
	}
	switch e.CapturedBy.Kind {
	case "actor", "tool", "operator":
	default:
		return fmt.Errorf("evidence.captured_by.kind: unknown kind")
	}
	switch e.CapturedBy.Attribution {
	case "self_reported":
		if e.CapturedBy.Kind == "operator" {
			return fmt.Errorf("evidence.captured_by: an operator cannot be self-reported")
		}
	case "trusted_runner":
		if !authority.TrustedRunner || e.CapturedBy.Kind != "tool" {
			return fmt.Errorf("evidence.captured_by: trusted runner authority required")
		}
	case "authenticated_operator":
		if !authority.OperateGates || e.CapturedBy.Kind != "operator" || e.CapturedBy.Identity != authority.SubmittedBy {
			return fmt.Errorf("evidence.captured_by: authenticated operator authority required")
		}
	default:
		return fmt.Errorf("evidence.captured_by.attribution: required attribution")
	}
	// The fields represent separate roles; the same authenticated operator can
	// capture and submit their observation without inventing a second identity.
	s := e.Subject
	switch s.Kind {
	case "kit":
		if s.KitID == "" || s.KitVersion == "" || !verificationHash(s.ContentDigest) || s.ExerciseID == "" {
			return fmt.Errorf("evidence.subject: kit identity, version, content digest and exercise required")
		}
		if s.ObligationID != "" || s.ContractDigest != "" {
			return fmt.Errorf("evidence.subject: kit forbids obligation fields")
		}
	case "ordinary":
		if s.ObligationID == "" || !verificationHash(s.ContractDigest) {
			return fmt.Errorf("evidence.subject: obligation ID and contract digest required")
		}
		if s.KitID != "" || s.KitVersion != "" || s.ContentDigest != "" || s.ExerciseID != "" {
			return fmt.Errorf("evidence.subject: ordinary forbids kit/exercise fields")
		}
	default:
		return fmt.Errorf("evidence.subject.kind: expected kit or ordinary")
	}
	if len(e.Revisions) == 0 {
		return fmt.Errorf("evidence.revisions: required")
	}
	seen := map[string]bool{}
	for i, r := range e.Revisions {
		if r.Repository == "" || r.RemoteIdentity == "" || !verificationOID(r.SHA) || seen[r.Repository] {
			return fmt.Errorf("evidence.revisions[%d]: invalid or duplicate revision", i)
		}
		seen[r.Repository] = true
	}
	if e.GoverningPins == nil {
		return fmt.Errorf("evidence.governing_pins: explicit list required")
	}
	seen = map[string]bool{}
	for i, p := range e.GoverningPins {
		key := p.Kind + "\x00" + p.DocumentID
		if (p.Kind != "requirement" && p.Kind != "system_design") || p.DocumentID == "" || p.Version <= 0 || seen[key] {
			return fmt.Errorf("evidence.governing_pins[%d]: invalid or duplicate pin", i)
		}
		seen[key] = true
	}
	if e.SafeInputs == nil {
		return fmt.Errorf("evidence.safe_inputs: explicit object required")
	}
	for _, f := range []struct{ name, value string }{{"target", e.Environment.Target}, {"os", e.Environment.OS}, {"architecture", e.Environment.Architecture}, {"runtime", e.Environment.Runtime}, {"deployment", e.Environment.Deployment}} {
		if strings.TrimSpace(f.value) == "" {
			return fmt.Errorf("evidence.environment.%s: use explicit unknown when unavailable", f.name)
		}
	}
	for k, v := range e.Environment.Attributes {
		if strings.TrimSpace(k) == "" || strings.TrimSpace(v) == "" {
			return fmt.Errorf("evidence.environment.attributes: use explicit unknown when unavailable")
		}
	}
	seen = map[string]bool{}
	for i, a := range e.Artifacts {
		if a.ArtifactID == "" || !verificationHash(a.SHA256) || a.MediaType == "" || seen[a.ArtifactID] {
			return fmt.Errorf("evidence.artifacts[%d]: identity, SHA-256 and media type required; duplicates forbidden", i)
		}
		seen[a.ArtifactID] = true
	}
	return e.validatePayload(authority)
}
func (e VerificationEvidence) validatePayload(authority VerificationEvidenceAuthority) error {
	decode := func(v any) error {
		if err := decodeVerificationJSON(e.Payload, v); err != nil {
			return fmt.Errorf("evidence.payload: %w", err)
		}
		return nil
	}
	refs := func(rs []VerificationReference, nonempty bool) error {
		if nonempty && len(rs) == 0 {
			return fmt.Errorf("evidence.payload: supporting references required")
		}
		for _, r := range rs {
			if (r.EvidenceID == "") == (r.ArtifactID == "") {
				return fmt.Errorf("evidence.payload: reference requires exactly one evidence or artifact identity")
			}
			if r.EvidenceID == e.ID {
				return fmt.Errorf("evidence.payload: self reference is cyclic")
			}
			if r.ArtifactID != "" {
				found := false
				for _, a := range e.Artifacts {
					if a.ArtifactID == r.ArtifactID && a.SHA256 == r.SHA256 {
						found = true
					}
				}
				if !found || !verificationHash(r.SHA256) {
					return fmt.Errorf("evidence.payload: artifact reference/hash missing from envelope")
				}
			} else if r.SHA256 != "" && !verificationHash(r.SHA256) {
				return fmt.Errorf("evidence.payload: invalid reference hash")
			}
		}
		return nil
	}
	required := func(values ...string) error {
		for _, v := range values {
			if strings.TrimSpace(v) == "" {
				return fmt.Errorf("evidence.payload: missing required content")
			}
		}
		return nil
	}
	capture := func(v string) error {
		if _, err := verificationUTC(v); err != nil {
			return fmt.Errorf("evidence.payload capture time: %w", err)
		}
		return nil
	}
	switch e.Type {
	case "api_exchange":
		var p APIExchangePayload
		if err := decode(&p); err != nil {
			return err
		}
		if err := required(p.Method, p.URL, p.RequestSummary, p.ResponseSummary); err != nil {
			return err
		}
		u, err := url.Parse(p.URL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
			return fmt.Errorf("evidence.payload.url: sanitized HTTP URL required")
		}
		if err := verificationTimeRange(p.RequestAt, p.ResponseAt); err != nil {
			return err
		}
		if (p.ResponseStatus == nil) == (p.TransportError == "") {
			return fmt.Errorf("evidence.payload: response status or transport error required")
		}
		if p.ResponseStatus != nil && (*p.ResponseStatus < 100 || *p.ResponseStatus > 599) {
			return fmt.Errorf("evidence.payload.response_status: invalid status")
		}
		for _, headers := range []map[string]string{p.RequestHeaders, p.ResponseHeaders} {
			if headers == nil {
				return fmt.Errorf("evidence.payload: explicit permitted headers required")
			}
			for k := range headers {
				switch strings.ToLower(k) {
				case "authorization", "proxy-authorization", "cookie", "set-cookie":
					return fmt.Errorf("evidence.payload: sensitive header %s is forbidden", k)
				}
			}
		}
		return refs(p.BodyArtifacts, false)
	case "state_observation":
		var p StateObservationPayload
		if err := decode(&p); err != nil {
			return err
		}
		if err := required(p.Target, p.Method); err != nil {
			return err
		}
		if err := capture(p.CapturedAt); err != nil {
			return err
		}
		if len(p.Value) == 0 && p.Artifact == nil {
			return fmt.Errorf("evidence.payload: observed value or artifact required")
		}
		if p.Artifact != nil {
			return refs([]VerificationReference{*p.Artifact}, true)
		}
		return nil
	case "assertion_result":
		var p AssertionResultPayload
		if err := decode(&p); err != nil {
			return err
		}
		if err := required(p.AssertionID, p.Text, p.Expected, p.Actual); err != nil {
			return err
		}
		if p.Outcome != "pass" && p.Outcome != "fail" && p.Outcome != "unknown" {
			return fmt.Errorf("evidence.payload.outcome: expected pass, fail or unknown")
		}
		return refs(p.Supporting, true)
	case "execution_report":
		var p ExecutionReportPayload
		if err := decode(&p); err != nil {
			return err
		}
		if len(p.Argv) == 0 || strings.TrimSpace(p.Argv[0]) == "" {
			return fmt.Errorf("evidence.payload.argv: required")
		}
		if err := required(p.Tool, p.ToolVersion, p.Runtime); err != nil {
			return err
		}
		if err := verificationTimeRange(p.StartedAt, p.EndedAt); err != nil {
			return err
		}
		if p.TimedOut == nil || p.Cancelled == nil || p.StdoutTruncated == nil || p.StderrTruncated == nil {
			return fmt.Errorf("evidence.payload: explicit timeout, cancellation and truncation flags required")
		}
		if p.ExitCode == nil && p.Signal == "" && !*p.TimedOut && !*p.Cancelled {
			return fmt.Errorf("evidence.payload: terminal execution outcome required")
		}
		if !verificationHash(p.StdoutSHA256) || !verificationHash(p.StderrSHA256) {
			return fmt.Errorf("evidence.payload: output SHA-256 hashes required")
		}
		return nil
	case "visual_capture":
		var p VisualCapturePayload
		if err := decode(&p); err != nil {
			return err
		}
		if err := required(p.CaptureTool, p.Target); err != nil {
			return err
		}
		if err := capture(p.CapturedAt); err != nil {
			return err
		}
		if p.Artifact.ArtifactID == "" {
			return fmt.Errorf("evidence.payload: visual artifact required")
		}
		if err := refs([]VerificationReference{p.Artifact}, true); err != nil {
			return err
		}
		switch p.MediaType {
		case "image/png", "image/jpeg", "image/webp", "image/gif", "video/mp4", "video/webm":
		default:
			return fmt.Errorf("evidence.payload.media_type: unsupported visual media")
		}
		for _, a := range e.Artifacts {
			if a.ArtifactID == p.Artifact.ArtifactID && a.MediaType != p.MediaType {
				return fmt.Errorf("evidence.payload.media_type: artifact media mismatch")
			}
		}
		return nil
	case "operator_observation":
		var p OperatorObservationPayload
		if err := decode(&p); err != nil {
			return err
		}
		if !authority.OperateGates || p.OperatorID != authority.SubmittedBy || e.CapturedBy.Kind != "operator" || e.CapturedBy.Attribution != "authenticated_operator" || e.CapturedBy.Identity != p.OperatorID {
			return fmt.Errorf("evidence.payload: authenticated operator with operate-gates capability required")
		}
		if err := required(p.Fact); err != nil {
			return err
		}
		if err := capture(p.CapturedAt); err != nil {
			return err
		}
		return refs(p.Supporting, true)
	default:
		return fmt.Errorf("evidence.type: unknown schema-1 payload %q", e.Type)
	}
}
func verificationUTC(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("UTC RFC3339 capture time required")
	}
	_, offset := t.Zone()
	if offset != 0 || (!strings.HasSuffix(s, "Z") && !strings.HasSuffix(s, "+00:00")) {
		return time.Time{}, fmt.Errorf("UTC time required")
	}
	return t, nil
}
func verificationTimeRange(start, end string) error {
	a, err := verificationUTC(start)
	if err != nil {
		return err
	}
	b, err := verificationUTC(end)
	if err != nil {
		return err
	}
	if b.Before(a) {
		return fmt.Errorf("evidence.payload: end precedes start")
	}
	return nil
}
func verificationHash(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == sha256.Size && s == strings.ToLower(s)
}
func verificationOID(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && (len(b) == 20 || len(b) == 32) && s == strings.ToLower(s)
}

// VerifyVerificationBytes checks retained bytes when available. Hash syntax alone
// cannot prove artifact integrity or media type; the artifact service must call
// its byte/media validators before retaining an envelope.
func VerifyVerificationBytes(data []byte, expected string) error {
	sum := sha256.Sum256(data)
	if !verificationHash(expected) || hex.EncodeToString(sum[:]) != expected {
		return fmt.Errorf("verification bytes: SHA-256 mismatch")
	}
	return nil
}

func decodeVerificationJSON(data []byte, out any) error {
	if len(bytes.TrimSpace(data)) == 0 || bytes.TrimSpace(data)[0] != '{' {
		return fmt.Errorf("expected JSON object")
	}
	// DisallowUnknownFields does not reject duplicate fields. Check the token
	// stream first, including nested payloads and safe-input objects.
	d := json.NewDecoder(bytes.NewReader(data))
	if err := uniqueVerificationJSON(d); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("expected one JSON value")
	}
	d = json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	return d.Decode(out)
}
func uniqueVerificationJSON(d *json.Decoder) error {
	tok, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			tok, err := d.Token()
			if err != nil {
				return err
			}
			key, ok := tok.(string)
			if !ok {
				return fmt.Errorf("expected object key")
			}
			if seen[key] {
				return fmt.Errorf("duplicate field %s", key)
			}
			seen[key] = true
			if err := uniqueVerificationJSON(d); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := uniqueVerificationJSON(d); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter")
	}
	_, err = d.Token()
	return err
}

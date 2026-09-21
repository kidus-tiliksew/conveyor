package store

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func verificationReadScalar(raw json.RawMessage, path []string) string {
	for _, key := range path {
		var object map[string]json.RawMessage
		if json.Unmarshal(raw, &object) != nil {
			return ""
		}
		raw = object[key]
	}
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

func verificationReadBound(value string) (string, bool) {
	r := []rune(value)
	if len(r) > 2048 {
		return string(r[:2048]), true
	}
	return value, false
}

func verificationReadAt(row VerificationRow) string {
	path := []string{"CreatedAt"}
	if row.Table == "verification_attempts" {
		path = []string{"StartedAt"}
	}
	if row.Table == "verification_evidence" {
		path = []string{"Envelope", "received_at"}
	}
	at, err := time.Parse(time.RFC3339Nano, verificationReadScalar(row.Body, path))
	if err != nil {
		at = time.Unix(0, 0).UTC()
	}
	return at.UTC().Format(VerificationReadTimeFormat)
}

func (m *volatileMemory) ReadVerificationPage(ctx context.Context, a VerificationAccess, p VerificationPageRequest) (VerificationReadPage, error) {
	cursor, err := ValidateVerificationPage(ctx, a, &p)
	if err != nil {
		return VerificationReadPage{}, err
	}
	if err = AuthorizeVerificationUserRead(ctx, m, a); err != nil {
		return VerificationReadPage{}, err
	}
	ws, _ := WorkspaceFromContext(ctx)
	m.mu.RLock()
	defer m.mu.RUnlock()
	if task, ok := m.tasks[a.TaskID]; !ok || task.Workspace != ws {
		return VerificationReadPage{}, ErrVerificationAccess
	}
	if p.ContextID != "" {
		row, ok := m.verificationRows[ws+"\x00verification_contexts\x00"+p.ContextID]
		if !ok || row.TaskID != a.TaskID {
			return VerificationReadPage{}, ErrVerificationAccess
		}
	}
	items := []VerificationReadItem{}
	add := func(item VerificationReadItem) {
		if cursor.ID != "" && (item.At > cursor.At || item.At == cursor.At && item.ID >= cursor.ID) {
			return
		}
		items = append(items, item)
		// Keep memory bounded even for a long retained task history.
		sort.Slice(items, func(i, j int) bool {
			if items[i].At != items[j].At {
				return items[i].At > items[j].At
			}
			return items[i].ID > items[j].ID
		})
		if len(items) > p.Limit+1 {
			items = items[:p.Limit+1]
		}
	}
	tableKind := p.Kind
	if tableKind == "assertions" {
		tableKind = "evidence"
	}
	for key, row := range m.verificationRows {
		if !strings.HasPrefix(key, ws+"\x00") || row.TaskID != a.TaskID || row.Table != "verification_"+tableKind || p.ContextID != "" && row.ContextID != p.ContextID {
			continue
		}
		if tableKind == "evidence" && row.State != "evidence" {
			continue
		}
		item := VerificationReadItem{ID: row.ID, ContextID: row.ContextID, RunID: row.RunID, State: row.State, At: verificationReadAt(row)}
		if p.Kind == "selections" {
			var selected struct {
				Receipt struct {
					Kits []struct {
						KitID       string          `json:"kit_id"`
						Digest      string          `json:"digest"`
						Eligibility string          `json:"eligibility"`
						Reasons     json.RawMessage `json:"reasons"`
					} `json:"kits"`
				}
			}
			if err = json.Unmarshal(row.Body, &selected); err != nil {
				return VerificationReadPage{}, err
			}
			item.At = verificationReadAt(m.verificationRows[ws+"\x00verification_contexts\x00"+row.ContextID])
			for _, kit := range selected.Receipt.Kits {
				reasons, truncated := verificationReadBound(string(kit.Reasons))
				item.ID = row.ID + ":" + kit.KitID
				item.Metadata = verificationJSON(map[string]string{"kit_id": kit.KitID, "digest": kit.Digest, "eligibility": kit.Eligibility, "reasons": reasons, "truncated": strconv.FormatBool(truncated)})
				add(item)
			}
			continue
		}
		if p.Kind == "assertions" {
			if verificationReadScalar(row.Body, []string{"Envelope", "type"}) != "assertion_result" {
				continue
			}
			var envelope struct {
				Envelope struct{ Subject core.VerificationSubject }
			}
			if err = json.Unmarshal(row.Body, &envelope); err != nil {
				return VerificationReadPage{}, err
			}
			var contracts []VerificationRow
			for key, r := range m.verificationRows {
				if strings.HasPrefix(key, ws+"\x00") && r.TaskID == a.TaskID && r.ContextID == row.ContextID && (r.Table == "verification_selections" || r.Table == "verification_obligations") {
					contracts = append(contracts, r)
				}
			}
			contract, err := verificationContract(contracts, row.ContextID, envelope.Envelope.Subject)
			if err != nil {
				return VerificationReadPage{}, err
			}
			assertionID := verificationReadScalar(row.Body, []string{"Envelope", "payload", "assertion_id"})
			required := false
			for _, id := range contract.RequiredAssertions {
				if id == assertionID {
					required = true
				}
			}
			bounded, _ := verificationReadBound(assertionID)
			item.Metadata = verificationJSON(map[string]string{"type": "assertion_result", "assertion_id": bounded, "outcome": verificationReadScalar(row.Body, []string{"Envelope", "payload", "outcome"}), "evidence_id": row.ID, "required": strconv.FormatBool(required)})
			add(item)
			continue
		}
		metadata := map[string]string{}
		truncated := false
		for name, path := range VerificationReadFieldsFor(p.Kind) {
			var cut bool
			metadata[name], cut = verificationReadBound(verificationReadScalar(row.Body, path))
			truncated = truncated || cut
		}
		metadata["truncated"] = strconv.FormatBool(truncated)
		if p.Kind == "contexts" {
			o := m.workOrders[metadata["work_order_id"]]
			metadata["source_sha"], metadata["claimant"], metadata["stage_state"] = o.HeadSHA, o.ClaimantID, string(o.State)
			count := 0
			for k, r := range m.verificationRows {
				if strings.HasPrefix(k, ws+"\x00") && r.TaskID == a.TaskID && r.ContextID == row.ID && r.Table == "verification_attempts" {
					count++
				}
			}
			metadata["attempt_count"] = strconv.Itoa(count)
		}
		item.Metadata = verificationJSON(metadata)
		add(item)
	}
	return VerificationPageResult(ctx, a, p, items), nil
}

func (m *volatileMemory) ReadVerificationDetail(ctx context.Context, a VerificationAccess, contextID, evidenceID string) (VerificationEvidenceRecord, error) {
	if a.UserID == "" {
		return VerificationEvidenceRecord{}, ErrVerificationAccess
	}
	if err := AuthorizeVerificationUserRead(ctx, m, a); err != nil {
		return VerificationEvidenceRecord{}, err
	}
	ws, _ := WorkspaceFromContext(ctx)
	m.mu.RLock()
	defer m.mu.RUnlock()
	task, ok := m.tasks[a.TaskID]
	if !ok || task.Workspace != ws {
		return VerificationEvidenceRecord{}, ErrVerificationAccess
	}
	vc, ok := m.verificationRows[ws+"\x00verification_contexts\x00"+contextID]
	if !ok || vc.TaskID != a.TaskID {
		return VerificationEvidenceRecord{}, ErrVerificationAccess
	}
	row, ok := m.verificationRows[ws+"\x00verification_evidence\x00"+evidenceID]
	if !ok || row.TaskID != a.TaskID || row.ContextID != contextID || row.State != "evidence" {
		return VerificationEvidenceRecord{}, ErrVerificationAccess
	}
	var result VerificationEvidenceRecord
	if json.Unmarshal(row.Body, &result) != nil || VerificationEvidenceDigest(result.Envelope) != result.Digest {
		return VerificationEvidenceRecord{}, ErrVerificationInvalid
	}
	return result, nil
}

// normalizeVerificationObservation executes only inside the existing mutation
// lock. feature-verification-kit-execution VK-6 / DEC-43: the first capture time participates in canonical replay identity.
func normalizeVerificationObservation(ctx context.Context, c VerificationCommand, rows []VerificationRow, now time.Time) (VerificationCommand, error) {
	in := c.OperatorObservation
	actor := ActorFromContext(ctx)
	ws, ok := WorkspaceFromContext(ctx)
	if !ok || c.Kind != VerificationWriteEvidence || c.Access.UserID == "" || actor.Role != core.ActorUser || actor.ID != UserActorID(c.Access.UserID) || !c.Authority.OperateGates || in.ContextID != c.ContextID || in.RunID != c.RunID || in.Key != c.Key {
		return c, ErrVerificationAccess
	}
	contextRow, ok := verificationFind(rows, "verification_contexts", c.ContextID)
	if !ok || contextRow.TaskID != c.Access.TaskID {
		return c, ErrVerificationAccess
	}
	vc := verificationDecode[VerificationContext](contextRow)
	runRow, ok := verificationFind(rows, "verification_attempts", c.RunID)
	if !ok || runRow.TaskID != c.Access.TaskID || runRow.ContextID != c.ContextID || vc.WorkspaceID != ws {
		return c, ErrVerificationAccess
	}
	run := verificationDecode[VerificationAttempt](runRow)
	if run.WorkOrderAttemptID != vc.WorkOrderAttemptID {
		return c, ErrVerificationAccess
	}
	c.Access.WorkOrderID, c.Access.WorkOrderAttemptID = vc.WorkOrderID, vc.WorkOrderAttemptID
	id := verificationHash(verificationJSON([]string{"operator-observation", actor.ID, c.ContextID, c.RunID, c.Key}))
	at := now.UTC().Format(time.RFC3339Nano)
	if old, found := verificationFind(rows, "verification_evidence", id); found {
		if old.TaskID != c.Access.TaskID || old.ContextID != c.ContextID || old.RunID != c.RunID || old.State != "evidence" {
			return c, ErrVerificationAccess
		}
		at = verificationDecode[VerificationEvidenceRecord](old).Envelope.CapturedAt
	}
	artifacts := []core.VerificationArtifactReference{}
	for _, ref := range in.Supporting {
		if ref.EvidenceID != "" {
			r, found := verificationFind(rows, "verification_evidence", ref.EvidenceID)
			if !found || r.State != "evidence" || r.TaskID != c.Access.TaskID || r.ContextID != c.ContextID || r.RunID != c.RunID {
				return c, ErrVerificationAccess
			}
		}
		if ref.ArtifactID != "" {
			found := false
			for _, r := range rows {
				if r.Table != "verification_evidence" || r.State != "evidence" || r.TaskID != c.Access.TaskID || r.ContextID != c.ContextID || r.RunID != c.RunID {
					continue
				}
				for _, a := range verificationDecode[VerificationEvidenceRecord](r).Envelope.Artifacts {
					if a.ArtifactID == ref.ArtifactID && a.SHA256 == ref.SHA256 {
						artifacts = append(artifacts, a)
						found = true
						break
					}
				}
				if found {
					break
				}
			}
			if !found {
				return c, ErrVerificationAccess
			}
		}
	}
	e := core.VerificationEvidence{SchemaVersion: 1, ID: id, SubmissionKey: c.Key, Type: "operator_observation", CapturedAt: at, ReceivedAt: now.UTC().Format(time.RFC3339Nano), SubmittedBy: actor.ID,
		CapturedBy:  core.VerificationCaptureActor{Identity: actor.ID, Kind: "operator", Version: "1", Attribution: "authenticated_operator"},
		WorkspaceID: ws, TaskID: c.Access.TaskID, WorkOrderID: vc.WorkOrderID, WorkOrderAttemptID: vc.WorkOrderAttemptID, ContextID: vc.ID, RunID: run.ID, Subject: run.Subject, Revisions: vc.Revisions, GoverningPins: vc.GoverningPins,
		SafeInputs: map[string]json.RawMessage{}, Environment: run.Environment, Artifacts: artifacts,
		Payload: verificationJSON(core.OperatorObservationPayload{OperatorID: actor.ID, Fact: in.Fact, CapturedAt: at, Supporting: in.Supporting})}
	c.OperatorObservation = nil
	c.Evidence = []json.RawMessage{verificationJSON(e)}
	return c, nil
}

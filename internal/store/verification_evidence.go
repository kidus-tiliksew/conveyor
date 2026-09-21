package store

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/redact"
)

type verificationBatch struct {
	Digest  string
	Receipt VerificationReceipt
	Uploads []verificationAcceptedUpload
}

type verificationAcceptedUpload struct {
	Input     VerificationArtifactInput
	Reference core.VerificationArtifactReference
}

func verificationEvidenceMutation(c VerificationCommand, rows []VerificationRow, vc VerificationContext, actor string, now time.Time, r *redact.Redactor, out *VerificationMutation) error {
	if c.Key == "" || len(c.Evidence) == 0 || len(c.Evidence) > 100 {
		return ErrVerificationInvalid
	}
	runRow, found := verificationFind(rows, "verification_attempts", c.RunID)
	if !found || runRow.ContextID != c.ContextID {
		return ErrVerificationAccess
	}
	attempt := verificationDecode[VerificationAttempt](runRow)
	if attempt.WorkOrderAttemptID != c.Access.WorkOrderAttemptID {
		return ErrVerificationAccess
	}
	// Normalize submitted material without received_at so retry after a lost
	// response returns the first server receipt and original receive time.
	items := make([]core.VerificationEvidence, 0, len(c.Evidence))
	ids := map[string]bool{}
	total := 0
	authority := c.Authority
	authority.SubmittedBy = actor
	for _, raw := range c.Evidence {
		total += len(raw)
		if len(raw) > core.MaxVerificationEvidenceBytes || total > 8<<20 {
			return ErrVerificationInvalid
		}
		b, err := verificationSanitizeJSON(r, raw)
		if err != nil {
			return err
		}
		// Decoder validates the sanitized representation, including unknown fields.
		var submitted core.VerificationEvidence
		if err = json.Unmarshal(b, &submitted); err != nil {
			return ErrVerificationInvalid
		}
		if submitted.WorkspaceID != vc.WorkspaceID || submitted.TaskID != vc.TaskID || submitted.WorkOrderID != vc.WorkOrderID || submitted.WorkOrderAttemptID != vc.WorkOrderAttemptID || submitted.ContextID != vc.ID || submitted.RunID != attempt.ID || !verificationEqual(submitted.Subject, attempt.Subject) || !verificationEqual(submitted.Revisions, vc.Revisions) || !verificationEqual(submitted.GoverningPins, vc.GoverningPins) {
			return ErrVerificationAccess
		}
		if c.Access.UserID != "" && submitted.Type != "operator_observation" {
			return ErrVerificationAccess
		}
		if !verificationIdentifier(submitted.ID) || submitted.SubmissionKey != c.Key || ids[submitted.ID] {
			return ErrVerificationInvalid
		}
		if _, err = core.DecodeVerificationEvidence(b, authority); err != nil {
			return fmt.Errorf("%w: %v", ErrVerificationInvalid, err)
		}
		ids[submitted.ID] = true
		submitted.ReceivedAt = ""
		items = append(items, submitted)
	}
	batchID := "submission:" + c.RunID + ":" + c.Key
	var prior *verificationBatch
	if old, ok := verificationFind(rows, "verification_evidence", batchID); ok {
		value := verificationDecode[verificationBatch](old)
		prior = &value
	}
	out.Receipt = VerificationReceipt{ID: verificationID(), EvidenceIDs: []string{}, ArtifactIDs: []string{}}
	var uploads []verificationAcceptedUpload
	var canonicalArtifacts []VerificationArtifactInput
	// Finalization consumes all contiguous chunks only in the evidence transaction.
	// Until commit these bytes remain inaccessible staging records.
	replacements := map[string]core.VerificationArtifactReference{}
	for _, input := range c.Artifacts {
		input.Name, _ = r.Redact(input.Name)
		input.SanitationRecord, _ = r.Redact(input.SanitationRecord)
		input.MaskingAttestation, _ = r.Redact(input.MaskingAttestation)
		var chunks []VerificationUploadChunk
		for _, row := range rows {
			if row.Table == "verification_upload_chunks" && row.LogicalKey == input.UploadID {
				if row.ContextID != c.ContextID || row.RunID != c.RunID {
					return ErrVerificationAccess
				}
				chunk := verificationDecode[VerificationUploadChunk](row)
				if !chunk.ExpiresAt.After(now) {
					return ErrVerificationState
				}
				chunks = append(chunks, chunk)
				out.DeleteChunks = append(out.DeleteChunks, row.ID)
			}
		}
		if len(chunks) == 0 {
			matched := false
			if prior != nil {
				for _, saved := range prior.Uploads {
					if verificationEqual(saved.Input, input) {
						replacements[input.SHA256] = saved.Reference
						uploads = append(uploads, saved)
						normalized := input
						normalized.UploadID = ""
						normalized.SHA256 = saved.Reference.SHA256
						normalized.ContentType = saved.Reference.MediaType
						normalized.SizeBytes = 0
						canonicalArtifacts = append(canonicalArtifacts, normalized)
						matched = true
						break
					}
				}
			}
			if !matched {
				if prior != nil {
					return ErrVerificationConflict
				}
				return ErrVerificationInvalid
			}
			continue
		}
		verificationSortChunks(chunks)
		var data []byte
		for i, chunk := range chunks {
			if chunk.Index != i {
				return ErrVerificationInvalid
			}
			data = append(data, chunk.Content...)
			if len(data) > core.MaxArtifactBytes {
				return ErrVerificationInvalid
			}
		}
		if int64(len(data)) != input.SizeBytes || verificationHash(data) != input.SHA256 {
			return ErrVerificationInvalid
		}
		media, err := core.ValidateTypedVerificationArtifact(input.ContentType, data)
		if err != nil {
			return err
		}
		switch media {
		case "application/json":
			data, err = verificationSanitizeJSON(r, data)
		case "text/plain":
			clean, _ := r.Redact(string(data))
			data = []byte(clean)
		default:
			if input.SanitationRecord == "" || input.MaskingAttestation == "" {
				return ErrVerificationInvalid
			}
		}
		if err != nil {
			return err
		}
		if _, err = core.ValidateTypedVerificationArtifact(media, data); err != nil {
			return err
		}
		id := verificationHash(data)
		a := core.Artifact{ID: id, Workspace: vc.WorkspaceID, TaskID: vc.TaskID, Name: input.Name, Role: core.ArtifactRoleTypedVerificationEvidence, ContentType: media, SizeBytes: int64(len(data)), CreatedAt: now}
		out.Artifacts = append(out.Artifacts, VerificationRetainedArtifact{a, data})
		out.Receipt.ArtifactIDs = append(out.Receipt.ArtifactIDs, id)
		replacements[input.SHA256] = core.VerificationArtifactReference{ArtifactID: id, SHA256: id, MediaType: media}
		uploads = append(uploads, verificationAcceptedUpload{input, replacements[input.SHA256]})
		normalized := input
		normalized.UploadID = ""
		normalized.SHA256 = id
		normalized.ContentType = media
		normalized.SizeBytes = 0
		canonicalArtifacts = append(canonicalArtifacts, normalized)
	}
	links := append([]VerificationEvidenceLink{}, c.Links...)
	for i := range items {
		e := &items[i]
		for j, a := range e.Artifacts {
			if replacement, ok := replacements[a.ArtifactID]; ok {
				e.Artifacts[j] = replacement
				e.Payload = verificationRewriteArtifact(e.Payload, a.ArtifactID, replacement.ArtifactID)
			} else {
				// Reusing bytes requires a same-task accepted evidence link, never merely
				// knowledge of a content hash (VK-6). The adapter verifies retained bytes.
				known := false
				for _, row := range rows {
					if row.Table == "verification_evidence" && row.State == "evidence" {
						old := verificationDecode[VerificationEvidenceRecord](row)
						for _, ref := range old.Envelope.Artifacts {
							if verificationEqual(a, ref) {
								known = true
							}
						}
					}
				}
				if !known {
					return ErrVerificationAccess
				}
			}
		}
		e.ReceivedAt = now.UTC().Format(time.RFC3339Nano)
		if err := e.Validate(authority); err != nil {
			return fmt.Errorf("%w: %v", ErrVerificationInvalid, err)
		}
		e.ReceivedAt = ""
		for _, id := range verificationPayloadReferences(e.Payload) {
			links = append(links, VerificationEvidenceLink{e.ID, id})
		}
	}

	digest := verificationHash(verificationJSON(struct {
		Items       []core.VerificationEvidence
		Links       []VerificationEvidenceLink
		Artifacts   []VerificationArtifactInput
		Publication *VerificationPublication
	}{items, c.Links, canonicalArtifacts, c.Publication}))
	if prior != nil {
		if prior.Digest != digest {
			return ErrVerificationConflict
		}
		*out = VerificationMutation{Receipt: prior.Receipt}
		return nil
	}
	if _, err := verificationWritableRun(c, rows); err != nil {
		return err
	}
	out.Receipt.Digest = digest
	for i := range items {
		if _, exists := verificationFind(rows, "verification_evidence", items[i].ID); exists {
			return ErrVerificationConflict
		}
		items[i].ReceivedAt = now.UTC().Format(time.RFC3339Nano)
	}
	// Validate all links as a graph before writing any item. Existing envelopes
	// cannot acquire new outgoing links through a later batch.
	graph := map[string][]string{}
	for _, row := range rows {
		if row.Table == "verification_evidence_links" {
			l := verificationDecode[VerificationEvidenceLink](row)
			graph[l.From] = append(graph[l.From], l.To)
		}
	}
	seenLinks := map[string]bool{}
	for _, l := range links {
		if !ids[l.From] || l.From == l.To {
			return ErrVerificationInvalid
		}
		if !ids[l.To] {
			row, ok := verificationFind(rows, "verification_evidence", l.To)
			if !ok || row.State != "evidence" || row.TaskID != c.Access.TaskID {
				return ErrVerificationAccess
			}
		}
		key := verificationHash(verificationJSON(l))
		if seenLinks[key] {
			continue
		}
		seenLinks[key] = true
		graph[l.From] = append(graph[l.From], l.To)
		out.Rows = append(out.Rows, verificationRow("verification_evidence_links", key, c.Access.TaskID, c.ContextID, c.RunID, key, "", l))
	}
	visiting := map[string]bool{}
	done := map[string]bool{}
	var visit func(string) bool
	visit = func(id string) bool {
		if visiting[id] {
			return false
		}
		if done[id] {
			return true
		}
		visiting[id] = true
		for _, to := range graph[id] {
			if !visit(to) {
				return false
			}
		}
		visiting[id] = false
		done[id] = true
		return true
	}
	for id := range graph {
		if !visit(id) {
			return ErrVerificationInvalid
		}
	}
	for _, e := range items {
		out.ArtifactReferences = append(out.ArtifactReferences, e.Artifacts...)
		record := VerificationEvidenceRecord{Envelope: e, Digest: VerificationEvidenceDigest(e)}
		for _, ref := range e.Artifacts {
			found := false
			for _, upload := range uploads {
				if upload.Reference.ArtifactID == ref.ArtifactID {
					record.ArtifactPolicies = append(record.ArtifactPolicies, VerificationArtifactPolicy{ArtifactID: ref.ArtifactID, MediaType: ref.MediaType, SanitationRecord: upload.Input.SanitationRecord, MaskingAttestation: upload.Input.MaskingAttestation})
					found = true
					break
				}
			}
			if !found {
				for _, row := range rows {
					if row.Table == "verification_evidence" && row.State == "evidence" {
						for _, policy := range verificationDecode[VerificationEvidenceRecord](row).ArtifactPolicies {
							if policy.ArtifactID == ref.ArtifactID {
								record.ArtifactPolicies = append(record.ArtifactPolicies, policy)
								found = true
								break
							}
						}
					}
					if found {
						break
					}
				}
			}
		}
		out.Rows = append(out.Rows, verificationRow("verification_evidence", e.ID, c.Access.TaskID, c.ContextID, c.RunID, c.RunID+":"+c.Key+":"+e.ID, "evidence", record))
		out.Receipt.EvidenceIDs = append(out.Receipt.EvidenceIDs, e.ID)
	}
	if c.Publication != nil {
		if err := verificationPublicationMutation(c, *c.Publication, rows, now, out); err != nil {
			return err
		}
	}
	out.Rows = append(out.Rows, verificationRow("verification_evidence", batchID, c.Access.TaskID, c.ContextID, c.RunID, batchID, "receipt", verificationBatch{Digest: digest, Receipt: out.Receipt, Uploads: uploads}))
	return nil
}
func verificationPayloadReferences(raw []byte) []string {
	var v any
	_ = json.Unmarshal(raw, &v)
	var ids []string
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, c := range x {
				if k == "evidence_id" {
					if id, ok := c.(string); ok && id != "" {
						ids = append(ids, id)
					}
				} else {
					walk(c)
				}
			}
		case []any:
			for _, c := range x {
				walk(c)
			}
		}
	}
	walk(v)
	return ids
}
func verificationRewriteArtifact(raw []byte, old, new string) []byte {
	var v any
	_ = json.Unmarshal(raw, &v)
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, c := range x {
				if (k == "artifact_id" || k == "sha256") && c == old {
					x[k] = new
				} else {
					walk(c)
				}
			}
		case []any:
			for _, c := range x {
				walk(c)
			}
		}
	}
	walk(v)
	return verificationJSON(v)
}

// VerifyRetainedVerificationArtifact rechecks the bytes inside the same
// transaction as evidence acceptance, including reused content addresses.
func VerifyRetainedVerificationArtifact(ref core.VerificationArtifactReference, content []byte) error {
	if ref.ArtifactID != ref.SHA256 || verificationHash(content) != ref.SHA256 {
		return ErrVerificationInvalid
	}
	media, err := core.ValidateTypedVerificationArtifact(ref.MediaType, content)
	if err != nil || media != ref.MediaType {
		return ErrVerificationInvalid
	}
	return nil
}
func VerificationArtifactMedia(rows []VerificationRow, evidenceID, artifactID string) string {
	row, ok := verificationFind(rows, "verification_evidence", evidenceID)
	if !ok {
		return ""
	}
	for _, ref := range verificationDecode[VerificationEvidenceRecord](row).Envelope.Artifacts {
		if ref.ArtifactID == artifactID {
			return ref.MediaType
		}
	}
	return ""
}

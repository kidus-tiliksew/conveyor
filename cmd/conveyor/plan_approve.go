package main

// Approval digests and draft state (feature-local-planning-drafts LP-2, LP-4;
// req-local-planning REQ-2 AC-2.1–AC-2.4). Approvals are local records that
// permit `conveyor plan push` to create unconfirmed proposals; they grant no
// confirmation authority (DEC-69).

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// PlanPushRecord is one item's push record. `conveyor plan push` writes
// these; approve preserves them unchanged and status reads them.
type PlanPushRecord struct {
	Digest    string     `json:"digest"`
	Phase     string     `json:"phase"`
	IntentAt  *time.Time `json:"intent_at,omitempty"`
	ChosenID  string     `json:"chosen_id,omitempty"`
	IntakeKey string     `json:"intake_key,omitempty"`
	ServerID  string     `json:"server_id,omitempty"`
	Version   int        `json:"version,omitempty"`
}

// PlanState is `.plan/state.json` (LP-2).
type PlanState struct {
	DraftID   string                    `json:"draft_id,omitempty"`
	Approvals map[string]string         `json:"approvals"`
	Pushes    map[string]PlanPushRecord `json:"pushes"`
}

func planStatePath(root string) string {
	return filepath.Join(root, planStateDir, planStateFile)
}

// LoadPlanState reads the draft's state. A missing file is an empty state; a
// malformed file is an error and is never reset.
func LoadPlanState(root string) (PlanState, error) {
	state := PlanState{Approvals: map[string]string{}, Pushes: map[string]PlanPushRecord{}}
	data, err := os.ReadFile(planStatePath(root))
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return PlanState{}, fmt.Errorf("read draft state: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&state); err != nil {
		return PlanState{}, fmt.Errorf("draft state %s is malformed: %w", filepath.Join(planStateDir, planStateFile), err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		return PlanState{}, fmt.Errorf("draft state %s is malformed: trailing data after the state object", filepath.Join(planStateDir, planStateFile))
	}
	if state.Approvals == nil {
		state.Approvals = map[string]string{}
	}
	if state.Pushes == nil {
		state.Pushes = map[string]PlanPushRecord{}
	}
	return state, nil
}

// planStateWriter is the narrow seam tests use to fail one step of the
// atomic replacement.
var planStateWriter = struct {
	write  func(*os.File, []byte) (int, error)
	sync   func(*os.File) error
	rename func(oldPath, newPath string) error
}{
	write:  func(f *os.File, data []byte) (int, error) { return f.Write(data) },
	sync:   func(f *os.File) error { return f.Sync() },
	rename: os.Rename,
}

// SavePlanState replaces `.plan/state.json` atomically: it writes a temporary
// file in `.plan/`, syncs and closes it, and renames it over the old state.
// On any failure the old state stays in place and the temporary file is
// removed.
func SavePlanState(root string, state PlanState) (err error) {
	if state.Approvals == nil {
		state.Approvals = map[string]string{}
	}
	if state.Pushes == nil {
		state.Pushes = map[string]PlanPushRecord{}
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode draft state: %w", err)
	}
	data = append(data, '\n')
	dir := filepath.Join(root, planStateDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create draft state directory: %w", err)
	}
	temp, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return fmt.Errorf("create draft state: %w", err)
	}
	tempPath := temp.Name()
	closed := false
	defer func() {
		if !closed {
			_ = temp.Close()
		}
		if err != nil {
			_ = os.Remove(tempPath)
		}
	}()
	if _, err = planStateWriter.write(temp, data); err != nil {
		return fmt.Errorf("write draft state: %w", err)
	}
	if err = planStateWriter.sync(temp); err != nil {
		return fmt.Errorf("sync draft state: %w", err)
	}
	closed = true
	if err = temp.Close(); err != nil {
		return fmt.Errorf("close draft state: %w", err)
	}
	if err = planStateWriter.rename(tempPath, planStatePath(root)); err != nil {
		return fmt.Errorf("replace draft state: %w", err)
	}
	return nil
}

// planDigestEnvelope is the object an item's digest covers (LP-4).
func planDigestEnvelope(item PlanItem) (map[string]any, error) {
	request, err := BuildPlanItemRequest(item)
	if err != nil {
		return nil, err
	}
	var baseDigest any
	if item.HasBase {
		sum := sha256.Sum256(item.Base)
		baseDigest = hex.EncodeToString(sum[:])
	}
	return map[string]any{
		"kind":                string(item.Kind),
		"item_id":             item.ID,
		"mode":                item.Mode,
		"base_version":        item.BaseVersion,
		"base_content_sha256": baseDigest,
		"request_body":        request.Body,
	}, nil
}

// PlanItemDigest returns `sha256:<hex>` over the canonical JSON of the item's
// kind, ID, mode, base version, base copy SHA-256, and push request body.
// Generated send-time values (DEC-n, intake key, server IDs) never enter it.
func PlanItemDigest(item PlanItem) (string, error) {
	envelope, err := planDigestEnvelope(item)
	if err != nil {
		return "", err
	}
	canonical, err := planCanonicalJSON(envelope)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// planCanonicalJSON encodes value with object keys sorted at every level, no
// insignificant whitespace, and no HTML escaping. The value is first
// round-tripped through generic JSON so struct field order cannot leak in.
func planCanonicalJSON(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var generic any
	if err := decoder.Decode(&generic); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(generic); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n")), nil
}

func newPlanDraftID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate draft_id: %w", err)
	}
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	encoded := hex.EncodeToString(value[:])
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32], nil
}

// planApproval is one validated `<item-id>@<digest>` argument.
type planApproval struct {
	ItemID string
	Digest string
}

// planApprovalFailure names one refused argument (AC-2.2).
type planApprovalFailure struct {
	ItemID string
	Reason string
}

func (f planApprovalFailure) String() string {
	return fmt.Sprintf("%s: %s", f.ItemID, f.Reason)
}

// approvePlanItems validates every pair against the current draft and, only
// when all pass, records them in one state replacement. It returns the
// recorded approvals or every failure; on failure nothing is written.
func approvePlanItems(draft *PlanDraft, pairs []string) ([]planApproval, []planApprovalFailure, error) {
	var approvals []planApproval
	var failures []planApprovalFailure
	for _, pair := range pairs {
		at := strings.LastIndex(pair, "@")
		if at <= 0 || at == len(pair)-1 {
			failures = append(failures, planApprovalFailure{ItemID: pair, Reason: "argument must be <item-id>@<digest>"})
			continue
		}
		id, digest := pair[:at], pair[at+1:]
		items := draft.Item(id)
		switch {
		case len(items) == 0:
			failures = append(failures, planApprovalFailure{ItemID: id, Reason: "the draft defines no such item"})
			continue
		case len(items) > 1:
			failures = append(failures, planApprovalFailure{ItemID: id, Reason: "the item ID is defined by more than one file"})
			continue
		}
		current, err := PlanItemDigest(items[0])
		if err != nil {
			failures = append(failures, planApprovalFailure{ItemID: id, Reason: fmt.Sprintf("cannot compute a digest: %v", err)})
			continue
		}
		if current != digest {
			failures = append(failures, planApprovalFailure{ItemID: id, Reason: fmt.Sprintf("current content digest is %s, not %s", current, digest)})
			continue
		}
		approvals = append(approvals, planApproval{ItemID: id, Digest: digest})
	}
	if len(failures) > 0 {
		return nil, failures, nil
	}
	state := draft.State
	next := PlanState{DraftID: state.DraftID, Approvals: map[string]string{}, Pushes: map[string]PlanPushRecord{}}
	for id, digest := range state.Approvals {
		next.Approvals[id] = digest
	}
	for id, record := range state.Pushes {
		next.Pushes[id] = record
	}
	if next.DraftID == "" {
		id, err := newPlanDraftID()
		if err != nil {
			return nil, nil, err
		}
		next.DraftID = id
	}
	for _, approval := range approvals {
		next.Approvals[approval.ItemID] = approval.Digest
	}
	if err := SavePlanState(draft.Root, next); err != nil {
		return nil, nil, err
	}
	draft.State = next
	return approvals, nil, nil
}

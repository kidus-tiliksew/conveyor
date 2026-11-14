package stageprefs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// Entry limits (component-stage-preferences invariant 2; req-stage-preferences
// AC-1.3) and the review seat limit (AC-1.2).
const (
	MaxNameBytes   = 128
	MaxEffortBytes = 32
	MaxReviewSeats = 8
)

// The five stage keys, in the order every surface shows them. Risk is the
// risk agent's stage and holds one entry like plan, implement, and verify
// (req-stage-preferences AC-1.1; DEC-135(13); component-stage-preferences).
const (
	StagePlan      = "plan"
	StageImplement = "implement"
	StageVerify    = "verify"
	StageRisk      = "risk"
	StageReview    = "review"
)

// Stages lists the stage keys in display order.
var Stages = []string{StagePlan, StageImplement, StageVerify, StageRisk, StageReview}

// stageList names the stage keys for refusal messages.
const stageList = "plan, implement, verify, risk, and review"

// ValidationError names the refused field and, for a review seat entry, the
// seat's 1-based position (component-stage-preferences invariant 2).
type ValidationError struct {
	Field    string
	Position int
	Message  string
}

func (e *ValidationError) Error() string { return e.Field + ": " + e.Message }

func invalid(field string, position int, format string, args ...any) error {
	return &ValidationError{Field: field, Position: position, Message: fmt.Sprintf(format, args...)}
}

// ParseStages decodes a whole set's stages object under the closed shape:
// only the five stage keys, only the three entry fields, only primary and
// seats on review, one entry for plan, implement, verify, and risk, and at most
// eight seats (invariant 3; AC-1.2, AC-1.5). An absent key is an unset stage.
func ParseStages(field string, raw json.RawMessage) (store.StagePreferenceStages, error) {
	var stages store.StagePreferenceStages
	if isNull(raw) {
		return stages, invalid(field, 0, "must be an object with the stage keys "+stageList)
	}
	object, err := closedObject(field, raw, Stages)
	if err != nil {
		return stages, err
	}
	for _, stage := range Stages {
		value, present := object[stage]
		if !present {
			continue
		}
		if err := setStage(&stages, field+"."+stage, stage, value); err != nil {
			return store.StagePreferenceStages{}, err
		}
	}
	return stages, nil
}

// ParseStageName refuses a stage key other than the five (invariant 3).
func ParseStageName(field, stage string) error {
	for _, known := range Stages {
		if stage == known {
			return nil
		}
	}
	return invalid(field, 0, "unknown stage %q; the stages are "+stageList, stage)
}

// SetStageValue replaces one stage of stages with value, the stage's whole
// value in the set's shape, where null clears the stage.
func SetStageValue(stages *store.StagePreferenceStages, field, stage string, value json.RawMessage) error {
	if err := ParseStageName("stage", stage); err != nil {
		return err
	}
	var next store.StagePreferenceStages
	if err := setStage(&next, field, stage, value); err != nil {
		return err
	}
	switch stage {
	case StagePlan:
		stages.Plan = next.Plan
	case StageImplement:
		stages.Implement = next.Implement
	case StageVerify:
		stages.Verify = next.Verify
	case StageRisk:
		stages.Risk = next.Risk
	case StageReview:
		stages.Review = next.Review
	}
	return nil
}

func setStage(stages *store.StagePreferenceStages, field, stage string, raw json.RawMessage) error {
	if stage == StageReview {
		review, err := parseReview(field, raw)
		if err != nil {
			return err
		}
		stages.Review = review
		return nil
	}
	if isArray(raw) {
		return invalid(field, 0, "%s holds one entry or null, not a list", stage)
	}
	entry, err := parseEntry(field, 0, raw)
	if err != nil {
		return err
	}
	switch stage {
	case StagePlan:
		stages.Plan = entry
	case StageImplement:
		stages.Implement = entry
	case StageVerify:
		stages.Verify = entry
	case StageRisk:
		stages.Risk = entry
	}
	return nil
}

// parseReview reads review's primary and seats. A review with neither is
// unset and normalizes to null (AC-1.2).
func parseReview(field string, raw json.RawMessage) (*store.ReviewStagePreference, error) {
	if isNull(raw) {
		return nil, nil
	}
	object, err := closedObject(field, raw, []string{"primary", "seats"})
	if err != nil {
		return nil, err
	}
	review := &store.ReviewStagePreference{Seats: []store.StagePreferenceEntry{}}
	if value, present := object["primary"]; present {
		if review.Primary, err = parseEntry(field+".primary", 0, value); err != nil {
			return nil, err
		}
	}
	if value, present := object["seats"]; present && !isNull(value) {
		if !isArray(value) {
			return nil, invalid(field+".seats", 0, "must be a list of seat entries")
		}
		var seats []json.RawMessage
		if err := json.Unmarshal(value, &seats); err != nil {
			return nil, invalid(field+".seats", 0, "must be a list of seat entries")
		}
		if len(seats) > MaxReviewSeats {
			return nil, invalid(fmt.Sprintf("%s.seats[%d]", field, MaxReviewSeats), MaxReviewSeats+1, "review holds at most %d seat entries", MaxReviewSeats)
		}
		for i, seat := range seats {
			seatField := fmt.Sprintf("%s.seats[%d]", field, i)
			if isNull(seat) {
				return nil, invalid(seatField, i+1, "seat %d needs an entry with a harness and a model", i+1)
			}
			entry, err := parseEntry(seatField, i+1, seat)
			if err != nil {
				return nil, err
			}
			review.Seats = append(review.Seats, *entry)
		}
	}
	if review.Primary == nil && len(review.Seats) == 0 {
		return nil, nil
	}
	return review, nil
}

// parseEntry reads one {harness, model, effort} entry, or null. Values are
// free text: nothing compares them with a list of harnesses or models
// (invariant 2; DEC-132(2)).
func parseEntry(field string, position int, raw json.RawMessage) (*store.StagePreferenceEntry, error) {
	if isNull(raw) {
		return nil, nil
	}
	object, err := closedObjectAt(field, position, raw, []string{"harness", "model", "effort"})
	if err != nil {
		return nil, err
	}
	harness, err := text(field+".harness", position, object["harness"], true, MaxNameBytes)
	if err != nil {
		return nil, err
	}
	model, err := text(field+".model", position, object["model"], true, MaxNameBytes)
	if err != nil {
		return nil, err
	}
	effort, err := text(field+".effort", position, object["effort"], false, MaxEffortBytes)
	if err != nil {
		return nil, err
	}
	return &store.StagePreferenceEntry{Harness: harness, Model: model, Effort: effort}, nil
}

func text(field string, position int, raw json.RawMessage, required bool, limit int) (string, error) {
	if raw == nil || isNull(raw) {
		if required {
			return "", invalid(field, position, "is required")
		}
		return "", nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", invalid(field, position, "must be text")
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return "", invalid(field, position, "must not hold a control character")
		}
	}
	value = strings.TrimSpace(value)
	if required && value == "" {
		return "", invalid(field, position, "must not be blank")
	}
	if len(value) > limit {
		return "", invalid(field, position, "must be at most %d bytes", limit)
	}
	return value, nil
}

func closedObject(field string, raw json.RawMessage, allowed []string) (map[string]json.RawMessage, error) {
	return closedObjectAt(field, 0, raw, allowed)
}

// closedObjectAt decodes a JSON object and refuses the first unknown key in
// sorted order, so a refusal always names the same field (invariant 3).
func closedObjectAt(field string, position int, raw json.RawMessage, allowed []string) (map[string]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, invalid(field, position, "must be an object with the fields %s", strings.Join(allowed, ", "))
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &object); err != nil {
		return nil, invalid(field, position, "must be an object with the fields %s", strings.Join(allowed, ", "))
	}
	var unknown []string
	for key := range object {
		known := false
		for _, name := range allowed {
			if key == name {
				known = true
				break
			}
		}
		if !known {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, invalid(field+"."+unknown[0], position, "is not a stage preference field; only %s are allowed here", strings.Join(allowed, ", "))
	}
	return object, nil
}

func isNull(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
}

func isArray(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '['
}

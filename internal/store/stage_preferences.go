package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// StagePreferenceEntry is one stage's harness, model, and optional effort, as
// free text (req-stage-preferences AC-1.1, AC-1.3; component-stage-preferences).
type StagePreferenceEntry struct {
	Harness string `json:"harness"`
	Model   string `json:"model"`
	Effort  string `json:"effort"`
}

// ReviewStagePreference is the review stage's optional primary entry and its
// ordered seat entries, where position N is review seat N
// (req-stage-preferences AC-1.2).
type ReviewStagePreference struct {
	Primary *StagePreferenceEntry  `json:"primary"`
	Seats   []StagePreferenceEntry `json:"seats"`
}

// StagePreferenceStages holds exactly the five stage keys. Nil means the stage
// is unset (component-stage-preferences, Public surfaces). Risk is the risk
// agent's entry (req-stage-preferences AC-1.1; DEC-135(13)).
type StagePreferenceStages struct {
	Plan      *StagePreferenceEntry  `json:"plan"`
	Implement *StagePreferenceEntry  `json:"implement"`
	Verify    *StagePreferenceEntry  `json:"verify"`
	Risk      *StagePreferenceEntry  `json:"risk"`
	Review    *ReviewStagePreference `json:"review"`
}

// StagePreferences is one person's whole set. Version 0 with a nil UpdatedAt
// means the person never saved a set (component-stage-preferences invariant 1).
type StagePreferences struct {
	Version   int64                 `json:"version"`
	UpdatedAt *time.Time            `json:"updated_at"`
	Stages    StagePreferenceStages `json:"stages"`
}

// StagePreferenceUpdate computes a person's next stages from the set current
// inside the write's transaction. An error aborts the write and changes
// nothing; the store returns that error unchanged.
type StagePreferenceUpdate func(current StagePreferences) (StagePreferenceStages, error)

// StagePreferenceStore keeps one versioned set of stage preferences per
// person. The user ID always comes from the caller's credential, never from
// request input (req-stage-preferences AC-1.6). UpdateStagePreferences runs
// apply against the current row while it holds that person's row lock, so
// concurrent writers, first writes included, serialize, and a successful
// write advances the version by exactly one (component-stage-preferences
// invariant 4). No write appends an event (REQ-3, AC-3.3).
//
// Only the stage preferences service in internal/stageprefs calls this
// interface; TestStagePreferencesAdviceOnly enforces that boundary
// (component-stage-preferences invariant 5).
type StagePreferenceStore interface {
	GetStagePreferences(ctx context.Context, userID string) (StagePreferences, error)
	UpdateStagePreferences(ctx context.Context, userID string, apply StagePreferenceUpdate) (StagePreferences, error)
}

// StagePreferenceColumns is the stored form of one set's five stages: each
// column holds a stage's JSON value, or nil when the stage is unset. The
// durable backends share it so every backend stores the same bytes.
type StagePreferenceColumns struct {
	Plan, Implement, Verify, Risk, Review []byte
}

// EncodeStagePreferenceColumns encodes each stage of stages for storage.
func EncodeStagePreferenceColumns(stages StagePreferenceStages) (StagePreferenceColumns, error) {
	var columns StagePreferenceColumns
	var err error
	encode := func(value any, isNil bool) []byte {
		if err != nil || isNil {
			return nil
		}
		var raw []byte
		raw, err = json.Marshal(value)
		return raw
	}
	columns.Plan = encode(stages.Plan, stages.Plan == nil)
	columns.Implement = encode(stages.Implement, stages.Implement == nil)
	columns.Verify = encode(stages.Verify, stages.Verify == nil)
	columns.Risk = encode(stages.Risk, stages.Risk == nil)
	columns.Review = encode(stages.Review, stages.Review == nil)
	if err != nil {
		return StagePreferenceColumns{}, fmt.Errorf("encode stage preferences: %w", err)
	}
	return columns, nil
}

// DecodeStagePreferenceColumns decodes the stored columns. A nil, empty, or
// JSON null column is an unset stage, so a row saved before the risk column
// existed reads risk as unset.
func DecodeStagePreferenceColumns(columns StagePreferenceColumns) (StagePreferenceStages, error) {
	var stages StagePreferenceStages
	decode := func(raw []byte, target any) error {
		if len(raw) == 0 || string(raw) == "null" {
			return nil
		}
		return json.Unmarshal(raw, target)
	}
	for _, column := range []struct {
		raw    []byte
		target any
	}{
		{columns.Plan, &stages.Plan},
		{columns.Implement, &stages.Implement},
		{columns.Verify, &stages.Verify},
		{columns.Risk, &stages.Risk},
		{columns.Review, &stages.Review},
	} {
		if err := decode(column.raw, column.target); err != nil {
			return StagePreferenceStages{}, fmt.Errorf("decode stage preferences: %w", err)
		}
	}
	if stages.Review != nil && stages.Review.Seats == nil {
		stages.Review.Seats = []StagePreferenceEntry{}
	}
	return stages, nil
}

// CloneStagePreferenceStages returns a deep copy, so a caller's later edits
// never reach a stored set.
func CloneStagePreferenceStages(stages StagePreferenceStages) StagePreferenceStages {
	entry := func(value *StagePreferenceEntry) *StagePreferenceEntry {
		if value == nil {
			return nil
		}
		copied := *value
		return &copied
	}
	cloned := StagePreferenceStages{Plan: entry(stages.Plan), Implement: entry(stages.Implement), Verify: entry(stages.Verify), Risk: entry(stages.Risk)}
	if stages.Review != nil {
		cloned.Review = &ReviewStagePreference{Primary: entry(stages.Review.Primary), Seats: append([]StagePreferenceEntry{}, stages.Review.Seats...)}
	}
	return cloned
}

// StagePreferenceTime is the update time every backend records: UTC at the
// microsecond precision both durable backends keep.
func StagePreferenceTime(now time.Time) time.Time {
	return now.UTC().Truncate(time.Microsecond)
}

// Package stageprefs owns each person's stage preferences: validation and
// every read and write behind the REST routes, the MCP tools, the CLI, and the
// Settings card (component-stage-preferences; req-stage-preferences).
//
// The set is advice to the person's own agent sessions. No dispatch, work
// order, claim, submission, review, verification, or merge path imports this
// package or queries its table, and TestStagePreferencesAdviceOnly fails when
// one does (component-stage-preferences invariant 5; REQ-3; DEC-131(3)).
package stageprefs

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// ErrVersionConflict refuses a whole-set replacement based on a version that
// is no longer current. The write changes nothing (AC-1.7).
var ErrVersionConflict = errors.New("stage preferences changed; read the current set and try again")

// ErrNoUser refuses a call without the credential's owner.
var ErrNoUser = errors.New("stage preferences need the caller's own user")

// Service reads and writes stage preferences through one store, so every
// surface shows the same set and version (invariant 6).
type Service struct {
	store store.StagePreferenceStore
}

// New returns the service over a backend's stage preference store.
func New(backend store.StagePreferenceStore) *Service {
	return &Service{store: backend}
}

// Get returns the person's whole set. A person who never saved one reads
// version 0 with every stage unset (AC-2.4).
func (s *Service) Get(ctx context.Context, userID string) (store.StagePreferences, error) {
	if strings.TrimSpace(userID) == "" {
		return store.StagePreferences{}, ErrNoUser
	}
	set, err := s.store.GetStagePreferences(ctx, userID)
	if err != nil {
		return store.StagePreferences{}, err
	}
	return normalized(set), nil
}

// Replace validates a PUT body, {"stages": {...}}, and replaces the whole set
// when expectedVersion is still current. Validation runs before any store
// access, and a stale version returns ErrVersionConflict without a write
// (invariant 4; AC-1.7).
func (s *Service) Replace(ctx context.Context, userID string, expectedVersion int64, body []byte) (store.StagePreferences, error) {
	if strings.TrimSpace(userID) == "" {
		return store.StagePreferences{}, ErrNoUser
	}
	stages, err := ParseReplacement(body)
	if err != nil {
		return store.StagePreferences{}, err
	}
	set, err := s.store.UpdateStagePreferences(ctx, userID, func(current store.StagePreferences) (store.StagePreferenceStages, error) {
		if current.Version != expectedVersion {
			return store.StagePreferenceStages{}, ErrVersionConflict
		}
		return stages, nil
	})
	if err != nil {
		return store.StagePreferences{}, err
	}
	return normalized(set), nil
}

// ParseReplacement reads a whole-set body under the closed shape. The body
// holds only "stages" (AC-1.5).
func ParseReplacement(body []byte) (store.StagePreferenceStages, error) {
	object, err := closedObject("body", body, []string{"stages"})
	if err != nil {
		var invalidErr *ValidationError
		if errors.As(err, &invalidErr) && strings.HasPrefix(invalidErr.Field, "body.") {
			invalidErr.Field = strings.TrimPrefix(invalidErr.Field, "body.")
		}
		return store.StagePreferenceStages{}, err
	}
	raw, present := object["stages"]
	if !present {
		return store.StagePreferenceStages{}, invalid("stages", 0, "is required")
	}
	return ParseStages("stages", raw)
}

// SetStage replaces or, with a null value, clears one stage. It reads and
// writes the current row in one transaction, so a write to one stage never
// drops another stage written in between (invariant 4).
func (s *Service) SetStage(ctx context.Context, userID, stage string, value json.RawMessage) (store.StagePreferences, error) {
	if strings.TrimSpace(userID) == "" {
		return store.StagePreferences{}, ErrNoUser
	}
	if err := ParseStageName("stage", stage); err != nil {
		return store.StagePreferences{}, err
	}
	var parsed store.StagePreferenceStages
	if err := SetStageValue(&parsed, "value", stage, value); err != nil {
		return store.StagePreferences{}, err
	}
	set, err := s.store.UpdateStagePreferences(ctx, userID, func(current store.StagePreferences) (store.StagePreferenceStages, error) {
		next := store.CloneStagePreferenceStages(current.Stages)
		switch stage {
		case StagePlan:
			next.Plan = parsed.Plan
		case StageImplement:
			next.Implement = parsed.Implement
		case StageVerify:
			next.Verify = parsed.Verify
		case StageReview:
			next.Review = parsed.Review
		}
		return next, nil
	})
	if err != nil {
		return store.StagePreferences{}, err
	}
	return normalized(set), nil
}

// normalized gives every surface one shape: a review with no primary and no
// seats reads as null, and a set review always carries a seats list.
func normalized(set store.StagePreferences) store.StagePreferences {
	set.Stages = store.CloneStagePreferenceStages(set.Stages)
	if review := set.Stages.Review; review != nil {
		if review.Primary == nil && len(review.Seats) == 0 {
			set.Stages.Review = nil
		} else if review.Seats == nil {
			review.Seats = []store.StagePreferenceEntry{}
		}
	}
	return set
}

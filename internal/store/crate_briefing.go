package store

// Crate briefing storage (feature-crate-insights CI-2; component-crates
// invariant 29, through component-requirement-insights invariants 14 to 16;
// req-crate-insights AC-5.2, AC-5.4, AC-5.6; DEC-142(2)). The briefing
// commands touch only the crate_briefings table: they append no event and
// change no task, work order, gate, document, or crate field.

import (
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/crateinsight"
)

// ErrCrateBriefingCurrent refuses a briefing whose fingerprint equals the
// stored row's. Nothing is written (component-requirement-insights
// invariant 14, through component-crates invariant 29).
var ErrCrateBriefingCurrent = errors.New("crate_briefing_current")

// CrateBriefingEntry is one task to look at first.
type CrateBriefingEntry = crateinsight.BriefingEntry

// CrateBriefing is the one stored briefing of a crate (component-crates,
// Storage). WorkspaceID is always the bound workspace, never a caller
// field. GeneratedBy is the credential-derived requesting actor the caller
// supplies. GeneratedAt is set by the store to its UTC storage time. The
// token counts are stored and never served.
type CrateBriefing struct {
	WorkspaceID string
	CrateID     string
	Fingerprint string
	Headline    string
	Account     string
	LookFirst   []CrateBriefingEntry
	Model       string
	TokensIn    int64
	TokensOut   int64
	GeneratedBy string
	GeneratedAt time.Time
}

// ValidateCrateBriefing checks the caller-supplied fields every backend
// refuses before any write.
func ValidateCrateBriefing(briefing CrateBriefing) error {
	switch {
	case strings.TrimSpace(briefing.CrateID) == "":
		return errors.New("crate briefing requires a crate ID")
	case strings.TrimSpace(briefing.Fingerprint) == "":
		return errors.New("crate briefing requires a fingerprint")
	case briefing.TokensIn < 0 || briefing.TokensOut < 0:
		return errors.New("crate briefing token counts must not be negative")
	}
	return nil
}

// CloneCrateBriefingEntries copies entries at a store boundary. A nil list
// becomes an empty one, so every backend returns the same shape.
func CloneCrateBriefingEntries(entries []CrateBriefingEntry) []CrateBriefingEntry {
	if entries == nil {
		return []CrateBriefingEntry{}
	}
	return slices.Clone(entries)
}

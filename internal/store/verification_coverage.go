package store

import (
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"strings"
)

// VerificationCoverageSource makes the verifier's interpretation reviewable;
// it does not claim to infer obligations automatically from governing prose.
type VerificationCoverageReference struct {
	DocumentID string `json:"document_id"`
	Version    int    `json:"version"`
	SectionID  string `json:"section_id"`
}

type VerificationCoverageSource struct {
	Source      VerificationCoverageReference `json:"source"`
	Disposition string                        `json:"disposition"`
	Explanation string                        `json:"explanation"`
	Subjects    []core.VerificationSubject    `json:"subjects"`
}

func ValidateVerificationCoverage(coverage VerificationCoverage, snapshot VerificationSnapshot) error {
	if coverage.ObligationIDs == nil || coverage.Sources == nil || strings.TrimSpace(coverage.Justification) == "" || len(snapshot.Contexts) != 1 {
		return ErrVerificationInvalid
	}
	obligations := map[string]bool{}
	subjects := map[core.VerificationSubject]bool{}
	for _, o := range snapshot.Obligations {
		obligations[o.ID] = true
		subjects[core.VerificationSubject{Kind: "ordinary", ObligationID: o.ID, ContractDigest: o.Digest}] = false
	}
	for _, selection := range snapshot.Selections {
		for _, s := range selection.Subjects {
			subjects[s.Subject] = false
		}
	}
	if len(obligations) != len(coverage.ObligationIDs) {
		return ErrVerificationState
	}
	for _, id := range coverage.ObligationIDs {
		if !obligations[id] {
			return ErrVerificationState
		}
		delete(obligations, id)
	}
	seen := map[VerificationCoverageReference]bool{}
	pins := map[string]bool{}
	for _, mapping := range coverage.Sources {
		if seen[mapping.Source] || mapping.Subjects == nil || strings.TrimSpace(mapping.Explanation) == "" {
			return ErrVerificationInvalid
		}
		seen[mapping.Source] = true
		pins[mapping.Source.DocumentID] = true
		switch mapping.Disposition {
		case "covered":
			if len(mapping.Subjects) == 0 {
				return ErrVerificationInvalid
			}
		case "not_applicable":
			if len(mapping.Subjects) != 0 {
				return ErrVerificationInvalid
			}
		default:
			return ErrVerificationInvalid
		}
		local := map[core.VerificationSubject]bool{}
		for _, subject := range mapping.Subjects {
			if _, ok := subjects[subject]; !ok || local[subject] {
				return ErrVerificationInvalid
			}
			local[subject], subjects[subject] = true, true
		}
	}
	for _, covered := range subjects {
		if !covered {
			return ErrVerificationState
		}
	}
	for _, pin := range snapshot.Contexts[0].GoverningPins {
		if !pins[pin.DocumentID] {
			return ErrVerificationState
		}
	}
	// Registration can grow as additional ordinary obligations are identified,
	// but an existing source or check cannot disappear at completion.
	if prior := snapshot.Contexts[0].Coverage; prior != nil {
		for _, old := range prior.Sources {
			found := false
			for _, next := range coverage.Sources {
				if old.Source != next.Source {
					continue
				}
				found = true
				if old.Disposition == "covered" && next.Disposition != "covered" {
					return ErrVerificationState
				}
				for _, subject := range old.Subjects {
					retained := false
					for _, candidate := range next.Subjects {
						retained = retained || subject == candidate
					}
					if !retained {
						return ErrVerificationState
					}
				}
			}
			if !found {
				return ErrVerificationState
			}
		}
	}
	return nil
}

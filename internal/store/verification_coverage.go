package store

import (
	"fmt"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
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
	if len(snapshot.Contexts) != 1 {
		return ErrVerificationInvalid
	}
	switch {
	case coverage.Sources == nil:
		return verificationRefusal(ErrVerificationInvalid, VerificationCodeFieldRequired, "coverage.sources", verificationRemedyList, "coverage.sources is required; send [] for an empty list.")
	case coverage.ObligationIDs == nil:
		return verificationRefusal(ErrVerificationInvalid, VerificationCodeFieldRequired, "coverage.obligation_ids", verificationRemedyList, "coverage.obligation_ids is required; send [] when no ordinary obligation is registered.")
	case strings.TrimSpace(coverage.Justification) == "":
		return verificationRefusal(ErrVerificationInvalid, VerificationCodeFieldRequired, "coverage.justification", verificationRemedyList, "coverage.justification is required, even for an empty set.")
	}
	obligations := map[string]bool{}
	registered := []string{}
	subjects := map[core.VerificationSubject]bool{}
	for _, o := range snapshot.Obligations {
		obligations[o.ID] = true
		registered = append(registered, o.ID)
		subjects[core.VerificationSubject{Kind: "ordinary", ObligationID: o.ID, ContractDigest: o.Digest}] = false
	}
	for _, selection := range snapshot.Selections {
		for _, s := range selection.Subjects {
			subjects[s.Subject] = false
		}
	}
	listed := map[string]bool{}
	for i, id := range coverage.ObligationIDs {
		if strings.TrimSpace(id) == "" || listed[id] {
			return verificationRefusal(ErrVerificationInvalid, VerificationCodeObligationIDInvalid, fmt.Sprintf("coverage.obligation_ids[%d]", i), verificationRemedyCoverage, "obligation IDs must be non-blank and listed once.")
		}
		listed[id] = true
		if !obligations[id] {
			return verificationRefusal(ErrVerificationState, VerificationCodeObligationsMismatch, fmt.Sprintf("coverage.obligation_ids[%d]", i), verificationRemedyCoverage, "%q is not a registered obligation; the registered obligations are [%s].", id, strings.Join(registered, ", "))
		}
	}
	if len(listed) != len(obligations) {
		return verificationRefusal(ErrVerificationState, VerificationCodeObligationsMismatch, "coverage.obligation_ids", verificationRemedyCoverage, "coverage.obligation_ids must list exactly the registered obligations [%s].", strings.Join(registered, ", "))
	}
	seen := map[VerificationCoverageReference]bool{}
	pins := map[string]bool{}
	for i, mapping := range coverage.Sources {
		field := fmt.Sprintf("coverage.sources[%d]", i)
		if seen[mapping.Source] {
			return verificationRefusal(ErrVerificationInvalid, VerificationCodeSourceDuplicate, field+".source", verificationRemedyCoverage, "each source citation appears once; %s v%d %s repeats.", mapping.Source.DocumentID, mapping.Source.Version, mapping.Source.SectionID)
		}
		if mapping.Subjects == nil {
			return verificationRefusal(ErrVerificationInvalid, VerificationCodeFieldRequired, field+".subjects", verificationRemedyList, "subjects is required; send [] for a not_applicable source.")
		}
		if strings.TrimSpace(mapping.Explanation) == "" {
			return verificationRefusal(ErrVerificationInvalid, VerificationCodeFieldRequired, field+".explanation", verificationRemedyList, "explanation is required.")
		}
		seen[mapping.Source] = true
		pins[mapping.Source.DocumentID] = true
		switch mapping.Disposition {
		case "covered":
			if len(mapping.Subjects) == 0 {
				return verificationRefusal(ErrVerificationInvalid, VerificationCodeDispositionInvalid, field+".disposition", verificationRemedyCoverage, "covered needs at least one subject.")
			}
		case "not_applicable":
			if len(mapping.Subjects) != 0 {
				return verificationRefusal(ErrVerificationInvalid, VerificationCodeDispositionInvalid, field+".disposition", verificationRemedyCoverage, "not_applicable needs an empty subjects list.")
			}
		default:
			return verificationRefusal(ErrVerificationInvalid, VerificationCodeDispositionInvalid, field+".disposition", verificationRemedyCoverage, "disposition must be covered or not_applicable.")
		}
		local := map[core.VerificationSubject]bool{}
		for j, subject := range mapping.Subjects {
			if _, ok := subjects[subject]; !ok || local[subject] {
				return verificationRefusal(ErrVerificationInvalid, VerificationCodeSubjectUnknown, fmt.Sprintf("%s.subjects[%d]", field, j), verificationRemedySubject, "%s must equal a selected kit subject or a registered obligation subject of this context, including its digest, and appear once per source.", verificationSubjectName(subject))
			}
			local[subject], subjects[subject] = true, true
		}
	}
	for subject, covered := range subjects {
		if !covered {
			return verificationRefusal(ErrVerificationState, VerificationCodeSubjectUnmapped, "coverage.sources", verificationRemedyCoverage, "%s appears in no covered source.", verificationSubjectName(subject))
		}
	}
	for _, pin := range snapshot.Contexts[0].GoverningPins {
		if !pins[pin.DocumentID] {
			return verificationRefusal(ErrVerificationState, VerificationCodePinUnmapped, "coverage.sources", verificationRemedyCoverage, "governing pin %s v%d appears in no source.", pin.DocumentID, pin.Version)
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
					return verificationRefusal(ErrVerificationState, VerificationCodeCoverageChanged, "coverage.sources", verificationRemedyCoverage, "registered source %s v%d %s was covered and must stay covered.", old.Source.DocumentID, old.Source.Version, old.Source.SectionID)
				}
				for _, subject := range old.Subjects {
					retained := false
					for _, candidate := range next.Subjects {
						retained = retained || subject == candidate
					}
					if !retained {
						return verificationRefusal(ErrVerificationState, VerificationCodeCoverageChanged, "coverage.sources", verificationRemedyCoverage, "registered source %s v%d %s must keep mapped subject %s.", old.Source.DocumentID, old.Source.Version, old.Source.SectionID, verificationSubjectName(subject))
					}
				}
			}
			if !found {
				return verificationRefusal(ErrVerificationState, VerificationCodeCoverageChanged, "coverage.sources", verificationRemedyCoverage, "registered source %s v%d %s cannot disappear.", old.Source.DocumentID, old.Source.Version, old.Source.SectionID)
			}
		}
	}
	return nil
}

// verificationSubjectName renders a subject identity from the caller's own context.
func verificationSubjectName(s core.VerificationSubject) string {
	if s.Kind == "kit" {
		return fmt.Sprintf("kit subject %s/%s (version %s, digest %s)", s.KitID, s.ExerciseID, s.KitVersion, s.ContentDigest)
	}
	return fmt.Sprintf("ordinary obligation %s (digest %s)", s.ObligationID, s.ContractDigest)
}

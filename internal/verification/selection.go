package verification

import (
	"fmt"
	"strings"
)

type Pin struct {
	Kind       string `json:"kind"`
	DocumentID string `json:"document_id"`
	Version    int    `json:"version"`
}

const (
	PinRequirement  = "requirement"
	PinSystemDesign = "system_design"
)

type SelectionContext struct {
	// Pins must come from the authoritative snapshot. Pending/unconfirmed pins
	// are not members of this set; the evaluator never upgrades them to authority.
	Pins             []Pin  `json:"pins"`
	Stage            string `json:"stage"`
	ManifestRevision string `json:"manifest_revision"`
	SourceRevision   string `json:"source_revision,omitempty"`
}
type SelectionReason struct {
	Code    string `json:"code"`
	Path    string `json:"path,omitempty"`
	Pin     *Pin   `json:"pin,omitempty"`
	Message string `json:"message,omitempty"`
}
type KitReceipt struct {
	KitID       string            `json:"kit_id"`
	Digest      string            `json:"digest"`
	Pins        []Pin             `json:"pins"`
	StageMatch  bool              `json:"stage_match"`
	Eligibility string            `json:"eligibility"`
	Reasons     []SelectionReason `json:"reasons"`
}
type SelectionReceipt struct {
	SchemaVersion    int          `json:"schema_version"`
	ManifestRevision string       `json:"manifest_revision"`
	SourceRevision   string       `json:"source_revision,omitempty"`
	ContextPins      []Pin        `json:"context_pins"`
	Stage            string       `json:"stage"`
	Kits             []KitReceipt `json:"kits"`
	Diagnostics      []Diagnostic `json:"diagnostics"`
}

func (r SelectionReceipt) Invalid() bool {
	if len(r.Diagnostics) > 0 {
		return true
	}
	for _, k := range r.Kits {
		if k.Eligibility == "invalid" {
			return true
		}
	}
	return false
}
func kitPins(k Kit) []Pin {
	pins := []Pin{}
	for _, p := range k.GoverningPins.Requirements {
		pins = append(pins, Pin{PinRequirement, p.DocumentID, p.Version})
	}
	for _, p := range k.GoverningPins.SystemDesigns {
		pins = append(pins, Pin{PinSystemDesign, p.DocumentID, p.Version})
	}
	return pins
}

// Evaluate is pure; it copies pins and explanations into a receipt for every
// entry. Exact snapshot membership, not global currency, governs eligibility
// (req-verification-kits AC-2.1 through AC-2.4, VK-2).
func Evaluate(m *Manifest, context SelectionContext, trees map[string][]TreeEntry) SelectionReceipt {
	r := SelectionReceipt{SchemaVersion: 1, ManifestRevision: context.ManifestRevision, SourceRevision: context.SourceRevision, ContextPins: append([]Pin{}, context.Pins...), Stage: context.Stage, Kits: []KitReceipt{}, Diagnostics: []Diagnostic{}}
	if m == nil {
		r.Diagnostics = append(r.Diagnostics, Diagnostic{"manifest", "unavailable"})
		return r
	}
	r.Diagnostics = append(r.Diagnostics, m.Diagnostics...)
	if len(m.Kits) > MaxKits {
		r.Diagnostics = append(r.Diagnostics, Diagnostic{"manifest.kits", "exceeds 100 kits"})
	}
	if m.SchemaVersion != 1 {
		r.Diagnostics = append(r.Diagnostics, Diagnostic{"manifest.schema_version", "unsupported schema"})
	}
	if strings.TrimSpace(context.Stage) == "" {
		r.Diagnostics = append(r.Diagnostics, Diagnostic{"context.stage", "required"})
	}
	exact := map[Pin]bool{}
	identities := map[string]bool{}
	for i, p := range context.Pins {
		if (p.Kind != PinRequirement && p.Kind != PinSystemDesign) || p.DocumentID == "" || p.Version <= 0 {
			r.Diagnostics = append(r.Diagnostics, Diagnostic{fmt.Sprintf("context.pins[%d]", i), "invalid authoritative pin"})
		}
		if identities[p.Kind+"\x00"+p.DocumentID] {
			r.Diagnostics = append(r.Diagnostics, Diagnostic{fmt.Sprintf("context.pins[%d]", i), "duplicate authoritative document identity"})
		}
		exact[p] = true
		identities[p.Kind+"\x00"+p.DocumentID] = true
	}
	counts := map[string]int{}
	for _, k := range m.Kits {
		counts[k.ID]++
	}
	for i, k := range m.Kits {
		entry := KitReceipt{KitID: k.ID, Pins: kitPins(k), Eligibility: "ineligible", Reasons: []SelectionReason{}}
		ds := append([]Diagnostic{}, k.Diagnostics...)
		ds = append(ds, validateKit(k, fmt.Sprintf("manifest.kits[%d]", i), nil)...)
		if counts[k.ID] > 1 {
			ds = append(ds, Diagnostic{fmt.Sprintf("manifest.kits[%d].id", i), "duplicate kit ID"})
		}
		for _, e := range k.Exercises {
			for _, s := range e.Stages {
				if s == context.Stage {
					entry.StageMatch = true
				}
			}
		}
		if len(ds) == 0 {
			entries, ok := trees[k.ID]
			if !ok {
				ds = append(ds, Diagnostic{"tree." + k.ID, "missing exact-revision tree"})
			} else {
				digest, err := ContentDigest(k, entries)
				if err != nil {
					ds = append(ds, Diagnostic{"tree." + k.ID, err.Error()})
				} else {
					entry.Digest = digest
				}
			}
		}
		if len(ds) > 0 || len(r.Diagnostics) > 0 {
			entry.Eligibility = "invalid"
			for _, d := range ds {
				entry.Reasons = append(entry.Reasons, SelectionReason{Code: "invalid_entry", Path: d.Path, Message: d.Message})
			}
			if len(r.Diagnostics) > 0 {
				entry.Reasons = append(entry.Reasons, SelectionReason{Code: "invalid_manifest_or_context"})
			}
		} else {
			if len(entry.Pins) == 0 {
				entry.Reasons = append(entry.Reasons, SelectionReason{Code: "no_governing_pins"})
			}
			for _, p := range entry.Pins {
				if !exact[p] {
					code := "pin_not_authoritative"
					if identities[p.Kind+"\x00"+p.DocumentID] {
						code = "pin_version_mismatch"
					}
					pin := p
					entry.Reasons = append(entry.Reasons, SelectionReason{Code: code, Pin: &pin})
				}
			}
			if !entry.StageMatch {
				entry.Reasons = append(entry.Reasons, SelectionReason{Code: "stage_excluded"})
			}
			if len(entry.Reasons) == 0 {
				entry.Eligibility = "eligible"
			}
		}
		r.Kits = append(r.Kits, entry)
	}
	return r
}

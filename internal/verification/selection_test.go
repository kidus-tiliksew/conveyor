package verification

import (
	"reflect"
	"testing"
)

func TestSelectionExactAuthoritativeSubset(t *testing.T) {
	pins := []Pin{{PinRequirement, "req-example", 3}, {PinSystemDesign, "component-example", 2}}
	tests := []struct {
		name, stage, want, reason string
		pins                      []Pin
		mutate                    func(*Manifest)
	}{
		{name: "subset", stage: "verify", want: "eligible", pins: pins},
		{name: "extra", stage: "verify", want: "eligible", pins: append(append([]Pin{}, pins...), Pin{PinRequirement, "extra", 1})},
		{name: "missing", stage: "verify", want: "ineligible", reason: "pin_not_authoritative", pins: pins[:1]},
		{name: "older kit pin drifts", stage: "verify", want: "eligible", reason: "pin_version_drift", pins: []Pin{{PinRequirement, "req-example", 4}, pins[1]}},
		{name: "newer kit pin", stage: "verify", want: "ineligible", reason: "pin_version_mismatch", pins: []Pin{{PinRequirement, "req-example", 2}, pins[1]}},
		{name: "unconfirmed excluded from authority", stage: "verify", want: "ineligible", reason: "pin_not_authoritative", pins: pins[1:]},
		{name: "kind mismatch", stage: "verify", want: "ineligible", reason: "pin_not_authoritative", pins: []Pin{{PinSystemDesign, "req-example", 3}, pins[1]}},
		{name: "no pins", stage: "verify", want: "ineligible", reason: "no_governing_pins", pins: pins, mutate: func(m *Manifest) { m.Kits[0].GoverningPins = GoverningPins{}; m.Kits[0].Exercises[0].Supports = nil }},
		{name: "excluded stage", stage: "review", want: "ineligible", reason: "stage_excluded", pins: pins},
		{name: "invalid entry", stage: "verify", want: "invalid", reason: "invalid_entry", pins: pins, mutate: func(m *Manifest) { m.Kits[0].Path = "../escape" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := fixtureManifest(t)
			if tt.mutate != nil {
				tt.mutate(m)
			}
			r := Evaluate(m, SelectionContext{Pins: tt.pins, Stage: tt.stage, ManifestRevision: "sha"}, map[string][]TreeEntry{"sample": fixtureTree()})
			if len(r.Kits) != 1 || r.Kits[0].Eligibility != tt.want || r.ManifestRevision != "sha" {
				t.Fatalf("%+v", r)
			}
			if tt.want != "invalid" && len(r.Kits[0].Digest) != 64 {
				t.Fatal("missing digest")
			}
			if tt.reason != "" {
				found := false
				for _, reason := range r.Kits[0].Reasons {
					found = found || reason.Code == tt.reason
				}
				if !found {
					t.Fatalf("missing reason: %+v", r.Kits[0])
				}
			}
		})
	}
}
func TestSelectionReceiptOwnsExplanation(t *testing.T) {
	m := fixtureManifest(t)
	ctx := SelectionContext{Pins: []Pin{{PinRequirement, "req-example", 4}}, Stage: "verify", ManifestRevision: "exact-sha"}
	r := Evaluate(m, ctx, map[string][]TreeEntry{"sample": fixtureTree()})
	ctx.Pins[0].Version = 99
	m.Kits[0].GoverningPins.Requirements[0].Version = 99
	if r.ContextPins[0].Version != 4 || r.Kits[0].Pins[0].Version != 3 || r.Kits[0].Reasons[0].Pin.Version != 3 {
		t.Fatal("receipt aliased caller data")
	}
}
func TestSelectionInvalidTreeAndSiblings(t *testing.T) {
	m := fixtureManifest(t)
	second := m.Kits[0]
	second.ID = "other"
	m.Kits = append(m.Kits, second)
	r := Evaluate(m, SelectionContext{Pins: kitPins(second), Stage: "verify"}, map[string][]TreeEntry{"other": fixtureTree()})
	if len(r.Kits) != 2 || r.Kits[0].Eligibility != "invalid" || r.Kits[1].Eligibility != "eligible" || !r.Invalid() {
		t.Fatalf("%+v", r)
	}
	m.Diagnostics = []Diagnostic{{"manifest", "malformed"}}
	r = Evaluate(m, SelectionContext{Stage: "verify"}, nil)
	if !r.Invalid() || r.Kits[1].Eligibility != "invalid" {
		t.Fatal(r)
	}
}

// TestSelectionPinVersionDrift proves invariant 12 of
// component-verification-kit-contract: an older kit pin stays eligible with a
// pin_version_drift warning naming both versions, and a newer, absent, or
// other-kind pin excludes the kit and records the pin (req-verification-kits
// AC-2.1, AC-2.2; DEC-111(1)).
func TestSelectionPinVersionDrift(t *testing.T) {
	// The fixture kit pins requirement req-example v3 and system design
	// component-example v2.
	req := func(v int) Pin { return Pin{PinRequirement, "req-example", v} }
	design := func(v int) Pin { return Pin{PinSystemDesign, "component-example", v} }
	drift := func(p Pin, authoritative int, message string) SelectionReason {
		return SelectionReason{Code: ReasonPinVersionDrift, Pin: &p, Message: message}
	}
	exclude := func(code string, p Pin) SelectionReason { return SelectionReason{Code: code, Pin: &p} }
	tests := []struct {
		name    string
		context []Pin
		want    string
		reasons []SelectionReason
	}{
		{name: "exact match", context: []Pin{req(3), design(2)}, want: "eligible", reasons: []SelectionReason{}},
		{name: "older requirement pin", context: []Pin{req(4), design(2)}, want: "eligible", reasons: []SelectionReason{drift(req(3), 4, "requirement req-example: kit pins v3, authoritative v4")}},
		{name: "older system design pin", context: []Pin{req(3), design(7)}, want: "eligible", reasons: []SelectionReason{drift(design(2), 7, "system_design component-example: kit pins v2, authoritative v7")}},
		{name: "both pins older", context: []Pin{req(15), design(3)}, want: "eligible", reasons: []SelectionReason{drift(req(3), 15, "requirement req-example: kit pins v3, authoritative v15"), drift(design(2), 3, "system_design component-example: kit pins v2, authoritative v3")}},
		{name: "newer pin", context: []Pin{req(2), design(2)}, want: "ineligible", reasons: []SelectionReason{exclude(ReasonPinVersionMismatch, req(3))}},
		{name: "absent document", context: []Pin{design(2)}, want: "ineligible", reasons: []SelectionReason{exclude(ReasonPinNotAuthoritative, req(3))}},
		{name: "kind mismatch", context: []Pin{{PinSystemDesign, "req-example", 9}, design(2)}, want: "ineligible", reasons: []SelectionReason{exclude(ReasonPinNotAuthoritative, req(3))}},
		{name: "older and newer pins", context: []Pin{req(4), design(1)}, want: "ineligible", reasons: []SelectionReason{drift(req(3), 4, "requirement req-example: kit pins v3, authoritative v4"), exclude(ReasonPinVersionMismatch, design(2))}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := Evaluate(fixtureManifest(t), SelectionContext{Pins: tt.context, Stage: "verify", ManifestRevision: "sha"}, map[string][]TreeEntry{"sample": fixtureTree()})
			if len(r.Kits) != 1 || len(r.Diagnostics) != 0 {
				t.Fatalf("%+v", r)
			}
			kit := r.Kits[0]
			if kit.Eligibility != tt.want || !reflect.DeepEqual(kit.Reasons, tt.reasons) {
				t.Fatalf("eligibility %q reasons %+v, want %q %+v", kit.Eligibility, kit.Reasons, tt.want, tt.reasons)
			}
			if len(kit.Digest) != 64 {
				t.Fatal("missing digest")
			}
			for _, reason := range kit.Reasons {
				if reason.Warning() != (reason.Code == ReasonPinVersionDrift) {
					t.Fatalf("warning classification: %+v", reason)
				}
			}
		})
	}
	// A drift warning never hides another exclusion: a drifted kit that
	// declares no exercise for the stage stays ineligible.
	r := Evaluate(fixtureManifest(t), SelectionContext{Pins: []Pin{req(4), design(2)}, Stage: "review"}, map[string][]TreeEntry{"sample": fixtureTree()})
	if r.Kits[0].Eligibility != "ineligible" || len(r.Kits[0].Reasons) != 2 || r.Kits[0].Reasons[0].Code != ReasonPinVersionDrift || r.Kits[0].Reasons[1].Code != "stage_excluded" {
		t.Fatalf("%+v", r.Kits[0])
	}
}

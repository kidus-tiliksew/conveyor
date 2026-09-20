package verification

import "testing"

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
		{name: "mismatched", stage: "verify", want: "ineligible", reason: "pin_version_mismatch", pins: []Pin{{PinRequirement, "req-example", 4}, pins[1]}},
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

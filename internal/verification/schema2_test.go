package verification

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// validManifestV2 is validManifest under schema 2 (VK-3.1): kit and exercise
// descriptions plus mapping-form required assertions.
var validManifestV2 = strings.NewReplacer(
	"schema_version: 1\nkits:", "schema_version: 2\nkits:",
	"    name: Sample kit\n", "    name: Sample kit\n    description: Creates a record through the API and reads it back.\n",
	"      - id: observe\n", "      - id: observe\n        description: Posts a record fixture and polls the read endpoint.\n",
	"required_assertions: [readable]", "required_assertions:\n          - id: readable\n            description: The created record is returned by GET with identical fields.",
).Replace(validManifest)

// schema1NormalizedKit is NormalizeKit output for the schema-1 fixture kit,
// captured on the base commit before schema 2 existed. Schema-1 bytes must
// never change: selection receipts and runner digest checks depend on them.
const schema1NormalizedKit = `{"id":"sample","name":"Sample kit","version":"1.0","path":"kits/sample","governing_pins":{"requirements":[{"document_id":"req-example","version":3}],"system_designs":[{"document_id":"component-example","version":2}]},"exercises":[{"id":"observe","stages":["verify"],"kind":"script","argv":["./exercise.sh"],"cwd":".","timeout_seconds":120,"prerequisites":[{"id":"api","kind":"service","environment_binding":"api"}],"permissions":[{"kind":"network","target_binding":"api"}],"inputs":[{"name":"case_id","type":"string","required":true,"sensitive":false}],"required_assertions":["readable"],"retry_policy":"reconciliation_required","operations":[{"id":"create","target_binding":"api","reconciliation":{"argv":["./reconcile.sh"],"timeout_seconds":60}}],"evidence_outputs":[{"type":"api_exchange","schema_version":1,"minimum_items":1}],"supports":[{"document_id":"req-example","version":3,"acceptance_criterion_id":"AC-1.1"}]}],"ui":{"argv":["./ui.sh"],"port":8080,"assets":["index.html"]}}`

func parseV2(t *testing.T, input string) *Manifest {
	t.Helper()
	m, err := Parse(strings.NewReader(input), nil)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestSchema2ParsesDescriptions(t *testing.T) {
	m := parseV2(t, validManifestV2)
	k := m.Kits[0]
	e := k.Exercises[0]
	if m.SchemaVersion != 2 || k.SchemaVersion != 2 {
		t.Fatalf("schema = %d/%d", m.SchemaVersion, k.SchemaVersion)
	}
	if k.Description != "Creates a record through the API and reads it back." || e.Description != "Posts a record fixture and polls the read endpoint." {
		t.Fatalf("descriptions lost: %q %q", k.Description, e.Description)
	}
	want := []Assertion{{ID: "readable", Description: "The created record is returned by GET with identical fields."}}
	if !reflect.DeepEqual(e.RequiredAssertions, want) || !reflect.DeepEqual(e.AssertionIDs(), []string{"readable"}) {
		t.Fatalf("assertions = %+v", e.RequiredAssertions)
	}
}

func TestSchema1DescriptionsAbsent(t *testing.T) {
	m := fixtureManifest(t)
	k := m.Kits[0]
	if k.SchemaVersion != 1 || k.Description != "" || k.Exercises[0].Description != "" {
		t.Fatalf("schema-1 kit carries descriptions: %+v", k)
	}
	if !reflect.DeepEqual(k.Exercises[0].RequiredAssertions, []Assertion{{ID: "readable"}}) {
		t.Fatalf("assertions = %+v", k.Exercises[0].RequiredAssertions)
	}
}

func TestSchema2Refusals(t *testing.T) {
	kitDesc := "    description: Creates a record through the API and reads it back.\n"
	exDesc := "        description: Posts a record fixture and polls the read endpoint.\n"
	asDesc := "            description: The created record is returned by GET with identical fields."
	tests := []struct{ name, old, new, want string }{
		{"missing kit description", kitDesc, "", "manifest.kits[0].description: required description"},
		{"empty kit description", kitDesc, "    description: \"\"\n", "manifest.kits[0].description: required description"},
		{"whitespace kit description", kitDesc, "    description: \"   \"\n", "manifest.kits[0].description: required description"},
		{"over-length kit description", kitDesc, "    description: " + strings.Repeat("a", MaxDescription+1) + "\n", "manifest.kits[0].description: description exceeds 1000"},
		{"non-string kit description", kitDesc, "    description: 5\n", "manifest.kits[0].description: expected string"},
		{"missing exercise description", exDesc, "", "exercises[0].description: required description"},
		{"over-length exercise description", exDesc, "        description: " + strings.Repeat("a", MaxDescription+1) + "\n", "exercises[0].description: description exceeds 1000"},
		{"non-string exercise description", exDesc, "        description: [a]\n", "exercises[0].description: expected string"},
		{"missing assertion description", "\n" + asDesc, "", "required_assertions[0].description: required description"},
		{"empty assertion description", asDesc, "            description: \"\"", "required_assertions[0].description: required description"},
		{"over-length assertion description", asDesc, "            description: " + strings.Repeat("a", MaxAssertionDescription+1), "required_assertions[0].description: description exceeds 500"},
		{"non-string assertion description", asDesc, "            description: true", "required_assertions[0].description: expected string"},
		{"bare-string assertion", "required_assertions:\n          - id: readable\n" + asDesc, "required_assertions: [readable]", "required_assertions[0]: expected mapping with id and description"},
		{"unknown assertion key", asDesc, asDesc + "\n            severity: high", "required_assertions[0].severity: unknown field"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := strings.Replace(validManifestV2, tt.old, tt.new, 1)
			if input == validManifestV2 {
				t.Fatal("fixture replacement did not apply")
			}
			m, err := Parse(strings.NewReader(input), nil)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %v, want %s", err, tt.want)
			}
			if len(m.Kits) != 1 || len(m.Kits[0].Diagnostics) == 0 {
				t.Fatalf("entry not invalid: %+v", m.Kits)
			}
			r := Evaluate(m, SelectionContext{Pins: []Pin{{PinRequirement, "req-example", 3}, {PinSystemDesign, "component-example", 2}}, Stage: "verify"}, map[string][]TreeEntry{"sample": fixtureTree()})
			if r.Kits[0].Eligibility != "invalid" {
				t.Fatalf("eligibility = %s", r.Kits[0].Eligibility)
			}
		})
	}
}

func TestSchema1RefusesSchema2Forms(t *testing.T) {
	tests := []struct{ name, old, new, want string }{
		{"kit description", "    name: Sample kit\n", "    name: Sample kit\n    description: Not allowed.\n", "manifest.kits[0].description: description requires schema_version 2"},
		{"empty kit description", "    name: Sample kit\n", "    name: Sample kit\n    description: \"\"\n", "manifest.kits[0].description: description requires schema_version 2"},
		{"exercise description", "      - id: observe\n", "      - id: observe\n        description: Not allowed.\n", "exercises[0].description: description requires schema_version 2"},
		{"mapping assertion", "required_assertions: [readable]", "required_assertions: [{id: readable, description: Not allowed.}]", "required_assertions[0]: schema 1 requires an assertion ID string"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := strings.Replace(validManifest, tt.old, tt.new, 1)
			if input == validManifest {
				t.Fatal("fixture replacement did not apply")
			}
			_, err := Parse(strings.NewReader(input), nil)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %v, want %s", err, tt.want)
			}
		})
	}
}

func TestSchema2ExactLimits(t *testing.T) {
	// Multi-byte characters count as one code point each.
	input := strings.NewReplacer(
		"description: Creates a record through the API and reads it back.", "description: "+strings.Repeat("é", MaxDescription),
		"description: Posts a record fixture and polls the read endpoint.", "description: \"  "+strings.Repeat("界", MaxDescription)+"  \"",
		"description: The created record is returned by GET with identical fields.", "description: "+strings.Repeat("ü", MaxAssertionDescription),
	).Replace(validManifestV2)
	m := parseV2(t, input)
	if n := len([]rune(m.Kits[0].Description)); n != MaxDescription {
		t.Fatalf("kit description = %d code points", n)
	}
	multi := "description: |\n          First line.\n          Second line.\n"
	m = parseV2(t, strings.Replace(validManifestV2, "description: Posts a record fixture and polls the read endpoint.\n", multi, 1))
	if m.Kits[0].Exercises[0].Description != "First line.\nSecond line.\n" {
		t.Fatalf("line breaks lost: %q", m.Kits[0].Exercises[0].Description)
	}
}

func TestSchemaReadBeforeKits(t *testing.T) {
	reorder := func(input string) string {
		i := strings.Index(input, "\n")
		return input[i+1:] + input[:i] + "\n"
	}
	m := parseV2(t, reorder(validManifestV2))
	if m.Kits[0].Description == "" || m.Kits[0].SchemaVersion != 2 {
		t.Fatalf("schema 2 not applied when kits precede schema_version: %+v", m.Kits[0])
	}
	v1 := reorder(strings.Replace(validManifest, "    name: Sample kit\n", "    name: Sample kit\n    description: Not allowed.\n", 1))
	if _, err := Parse(strings.NewReader(v1), nil); err == nil || !strings.Contains(err.Error(), "description requires schema_version 2") {
		t.Fatalf("schema 1 not applied when kits precede schema_version: %v", err)
	}
}

func TestSchemaVersions(t *testing.T) {
	for _, v := range []string{"0", "3"} {
		_, err := Parse(strings.NewReader(strings.Replace(validManifest, "schema_version: 1", "schema_version: "+v, 1)), nil)
		if err == nil || !strings.Contains(err.Error(), "unsupported schema; expected 1 or 2") {
			t.Fatalf("schema %s: %v", v, err)
		}
	}
}

func TestNormalizeKitSchema1BytesUnchanged(t *testing.T) {
	b, err := NormalizeKit(fixtureManifest(t).Kits[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != schema1NormalizedKit {
		t.Fatalf("schema-1 normalized bytes changed:\n%s", b)
	}
}

func TestDigestGoldenSchema2(t *testing.T) {
	k := parseV2(t, validManifestV2).Kits[0]
	digest, err := ContentDigest(k, fixtureTree())
	if err != nil {
		t.Fatal(err)
	}
	const want = "db6e97c54d729f3642634d64bd173bfab30b1d09233419013d79a747484ec849"
	if digest != want {
		t.Fatalf("digest = %s, want %s", digest, want)
	}
	b, _ := NormalizeKit(k)
	if !strings.Contains(string(b), `"description":"Creates a record through the API and reads it back."`) || !strings.Contains(string(b), `"required_assertions":[{"id":"readable","description":`) {
		t.Fatalf("schema-2 descriptions missing from digest input: %s", b)
	}
}

// AC-10.3: descriptions never enter eligibility.
func TestDescriptionOnlyEditKeepsSelection(t *testing.T) {
	pins := []Pin{{PinRequirement, "req-example", 3}, {PinSystemDesign, "component-example", 2}}
	evaluate := func(input string, stage string) SelectionReceipt {
		return Evaluate(parseV2(t, input), SelectionContext{Pins: pins, Stage: stage, ManifestRevision: "sha"}, map[string][]TreeEntry{"sample": fixtureTree()})
	}
	edited := strings.NewReplacer(
		"reads it back.", "reads it back again.",
		"polls the read endpoint.", "polls the read endpoint once.",
		"identical fields.", "the same fields.",
	).Replace(validManifestV2)
	for _, stage := range []string{"verify", "review"} {
		before, after := evaluate(validManifestV2, stage), evaluate(edited, stage)
		b, a := before.Kits[0], after.Kits[0]
		if b.Eligibility != a.Eligibility || b.StageMatch != a.StageMatch || !reflect.DeepEqual(b.Reasons, a.Reasons) || !reflect.DeepEqual(b.Pins, a.Pins) {
			t.Fatalf("%s: selection changed: %+v vs %+v", stage, b, a)
		}
		if b.Digest == a.Digest {
			t.Fatalf("%s: schema-2 digest ignores descriptions", stage)
		}
	}
}

func TestAssertionJSONForms(t *testing.T) {
	b, err := json.Marshal([]Assertion{{ID: "bare"}, {ID: "described", Description: "Observed."}})
	if err != nil || string(b) != `["bare",{"id":"described","description":"Observed."}]` {
		t.Fatalf("marshal = %s %v", b, err)
	}
	var got []Assertion
	if err := json.Unmarshal(b, &got); err != nil || !reflect.DeepEqual(got, []Assertion{{ID: "bare"}, {ID: "described", Description: "Observed."}}) {
		t.Fatalf("unmarshal = %+v %v", got, err)
	}
	if err := json.Unmarshal([]byte(`[{"id":"x","severity":"high"}]`), &got); err == nil {
		t.Fatal("unknown assertion field accepted")
	}
}

func TestAssertionYAMLRoundTrip(t *testing.T) {
	for _, input := range []string{validManifest, validManifestV2} {
		m := parseV2(t, input)
		data, err := yaml.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		again := parseV2(t, string(data))
		if !reflect.DeepEqual(again.Kits[0].Exercises[0].RequiredAssertions, m.Kits[0].Exercises[0].RequiredAssertions) {
			t.Fatalf("round trip changed assertions: %+v", again.Kits[0].Exercises[0].RequiredAssertions)
		}
	}
}

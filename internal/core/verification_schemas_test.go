package core

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/verification"
)

func TestVerificationEvidenceSchemaRuntimeParity(t *testing.T) {
	schemas := VerificationEvidenceSchemas()
	if len(schemas) != 6 {
		t.Fatal("six schemas required")
	}
	for kind, value := range schemas {
		t.Run(kind, func(t *testing.T) {
			e, authority := evidenceFixture(t, kind)
			raw := evidencePayload(t, e)
			schema := value.(map[string]any)
			var decoded map[string]any
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatal(err)
			}
			if err := validateVerificationSchema(decoded, schema); err != nil {
				t.Fatalf("published schema rejects valid %s: %v", kind, err)
			}
			if _, err := DecodeVerificationEvidence(raw, authority); err != nil {
				t.Fatal(err)
			}
			for _, mutate := range []func(map[string]any){func(v map[string]any) { v["schema_version"] = float64(2) }, func(v map[string]any) { v["type"] = "unknown" }, func(v map[string]any) { v["unknown"] = true }, func(v map[string]any) { v["captured_at"] = "not-a-time" }, func(v map[string]any) { delete(v, "run_id") }, func(v map[string]any) { v["payload"].(map[string]any)["unknown"] = true }} {
				json.Unmarshal(raw, &decoded)
				mutate(decoded)
				bad, _ := json.Marshal(decoded)
				if err := validateVerificationSchema(decoded, schema); err == nil {
					t.Fatal("schema accepted invalid envelope")
				}
				if _, err := DecodeVerificationEvidence(bad, authority); err == nil {
					t.Fatal("runtime accepted invalid envelope")
				}
			}
		})
	}
}
func TestArtifactMediaVerificationRecordingPolicy(t *testing.T) {
	for _, media := range []string{"video/mp4", "video/webm"} {
		if _, err := ValidateArtifactMedia(media, []byte("invalid recording"), TypedVerificationMedia); err == nil {
			t.Fatalf("%s accepted text", media)
		}
		if _, err := ValidateArtifactMedia(media, []byte("legacy explicit declaration")); err != nil {
			t.Fatal("legacy explicit non-image policy changed")
		}
	}
}

// Bare-string required assertions stay valid MCP input beside the schema-2
// {id, description} form (component-verification-kit-contract).
func TestVerificationAssertionSchemaAcceptsBothForms(t *testing.T) {
	schema := VerificationJSONSchema(reflect.TypeOf(verification.Exercise{}))
	items := schema["properties"].(map[string]any)["required_assertions"].(map[string]any)["items"].(map[string]any)
	if _, ok := items["anyOf"]; !ok {
		t.Fatalf("assertion schema = %+v", items)
	}
	for _, tc := range []struct {
		raw string
		ok  bool
	}{
		{`["bare"]`, true},
		{`[{"id":"described","description":"Observed."}]`, true},
		{`[{"id":"described"}]`, true},
		{`[{"description":"missing id"}]`, false},
		{`[{"id":"x","severity":"high"}]`, false},
		{`[5]`, false},
	} {
		var value any
		if err := json.Unmarshal([]byte(tc.raw), &value); err != nil {
			t.Fatal(err)
		}
		err := validateVerificationSchema(value, schema["properties"].(map[string]any)["required_assertions"].(map[string]any))
		if (err == nil) != tc.ok {
			t.Fatalf("%s: err = %v", tc.raw, err)
		}
	}
}

// TestVerificationSchemaErrorNamesPathAndKeyword pins the structural refusal
// detail: each keyword names its JSON path, and an unknown property's name
// never enters the error text (component-verification-evidence;
// component-verification-service "Correctable refusals").
func TestVerificationSchemaErrorNamesPathAndKeyword(t *testing.T) {
	type wire struct {
		Environment VerificationEnvironment `json:"environment"`
		Pins        []VerificationPin       `json:"pins"`
		Subject     VerificationSubject     `json:"subject"`
		At          time.Time               `json:"at"`
		Count       int                     `json:"count"`
		Attributes  map[string]int          `json:"attributes,omitempty"`
	}
	valid := func() map[string]any {
		return map[string]any{
			"environment": map[string]any{"target": "fixture", "os": "linux", "architecture": "amd64", "runtime": "go", "deployment": "local"},
			"pins":        []any{map[string]any{"kind": "requirement", "document_id": "req-fixture", "version": float64(1)}, map[string]any{"kind": "system_design", "document_id": "component-fixture", "version": float64(2)}},
			"subject":     map[string]any{"kind": "ordinary", "obligation_id": "check", "contract_digest": strings.Repeat("a", 64)},
			"at":          "2026-10-28T12:00:00Z",
			"count":       float64(3),
		}
	}
	schema := VerificationJSONSchema(reflect.TypeOf(wire{}))
	if err := validateVerificationSchema(valid(), schema); err != nil {
		t.Fatalf("valid request refused: %v", err)
	}
	for _, tc := range []struct {
		name, path, keyword, property string
		mutate                        func(map[string]any)
	}{
		{"unknown top-level", "", "additionalProperties", "bogus_field", func(v map[string]any) { v["bogus_field"] = true }},
		{"unknown nested", "environment", "additionalProperties", "sk-abcdefghijklmnopqrstuvwxyz012345", func(v map[string]any) {
			v["environment"].(map[string]any)["sk-abcdefghijklmnopqrstuvwxyz012345"] = "x"
		}},
		{"required top-level", "environment", "required", "", func(v map[string]any) { delete(v, "environment") }},
		{"required nested", "environment.deployment", "required", "", func(v map[string]any) { delete(v["environment"].(map[string]any), "deployment") }},
		{"type", "count", "type", "", func(v map[string]any) { v["count"] = "3" }},
		{"type fraction", "count", "type", "", func(v map[string]any) { v["count"] = 1.5 }},
		{"pattern", "environment.deployment", "pattern", "", func(v map[string]any) { v["environment"].(map[string]any)["deployment"] = " " }},
		{"enum", "pins[1].kind", "enum", "", func(v map[string]any) { v["pins"].([]any)[1].(map[string]any)["kind"] = "other" }},
		{"minimum", "pins[0].version", "minimum", "", func(v map[string]any) { v["pins"].([]any)[0].(map[string]any)["version"] = float64(0) }},
		{"format", "at", "format", "", func(v map[string]any) { v["at"] = "yesterday" }},
		{"map entry", "attributes.*", "type", "", func(v map[string]any) { v["attributes"] = map[string]any{"caller-key": "x"} }},
		{"oneOf", "subject", "oneOf", "", func(v map[string]any) { v["subject"] = map[string]any{"kind": "kit"} }},
		{"required before properties", "environment", "required", "", func(v map[string]any) { delete(v, "environment"); v["aaa"] = 1 }},
		{"sorted properties", "", "additionalProperties", "aaa", func(v map[string]any) { v["count"] = "x"; v["aaa"] = 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for range 5 { // map order must not change the reported failure
				value := valid()
				tc.mutate(value)
				err := validateVerificationSchema(value, schema)
				var shape *VerificationSchemaError
				if !errors.As(err, &shape) || shape.Path != tc.path || shape.Keyword != tc.keyword || shape.Property != tc.property {
					t.Fatalf("got %#v (%v), want path %q keyword %q property %q", shape, err, tc.path, tc.keyword, tc.property)
				}
				want := tc.path
				if want == "" {
					want = "request"
				}
				if !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), tc.keyword) {
					t.Fatalf("error %q does not name path %q and keyword %q", err, want, tc.keyword)
				}
				if tc.property != "" && strings.Contains(err.Error()+shape.Rule(), tc.property) {
					t.Fatalf("error text carries the caller's property name: %q", err.Error()+shape.Rule())
				}
				if shape.Rule() == "" || len(shape.Rule()) > 1024 {
					t.Fatalf("rule %q", shape.Rule())
				}
			}
		})
	}
	for _, tc := range []struct {
		keyword string
		value   any
		schema  map[string]any
	}{
		{"maximum", float64(600), map[string]any{"type": "integer", "maximum": 599}},
		{"minItems", []any{}, map[string]any{"type": "array", "minItems": 1}},
		{"const", float64(2), map[string]any{"type": "integer", "const": 1}},
		{"anyOf", map[string]any{}, map[string]any{"anyOf": []map[string]any{{"required": []string{"a"}}, {"required": []string{"b"}}}}},
	} {
		var shape *VerificationSchemaError
		if err := validateVerificationSchema(tc.value, tc.schema); !errors.As(err, &shape) || shape.Keyword != tc.keyword || shape.Path != "" {
			t.Fatalf("%s: got %v", tc.keyword, err)
		}
	}
}

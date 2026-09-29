package core

import (
	"encoding/json"
	"reflect"
	"testing"

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
// {id, description} form (feature-verification-kit-execution VK-3.1).
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

package core

import (
	"encoding/json"
	"testing"
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

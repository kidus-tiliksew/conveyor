package core

import (
	"bytes"
	"image"
	"image/png"
	"testing"
)

func TestTypedVerificationArtifactPolicy(t *testing.T) {
	role := ArtifactRoleTypedVerificationEvidence
	if !role.Valid() || role.ModelInputEligible() {
		t.Fatal("typed evidence role must be valid and excluded from model input")
	}
	if (Artifact{Role: role, TaskID: "task", ContentType: "image/png", SizeBytes: 100}).EligibleVerificationEvidence() {
		t.Fatal("typed evidence satisfied legacy visual gate")
	}
	if !ArtifactRoleVerificationEvidence.Valid() || ArtifactRoleVerificationEvidence.ModelInputEligible() {
		t.Fatal("legacy evidence policy changed")
	}
	var pngBytes bytes.Buffer
	if err := png.Encode(&pngBytes, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		media string
		data  []byte
		valid bool
	}{
		{"text/plain", []byte("safe text"), true}, {"application/json", []byte(`{"safe":true}`), true}, {"image/png", pngBytes.Bytes(), true},
		{"text/html", []byte("<script>bad()</script>"), false}, {"application/zip", []byte("PK"), false}, {"image/png", []byte("fake"), false},
		{"video/mp4", []byte("ftyp"), false}, {"video/webm", []byte("webm"), false}, {"application/json", []byte("invalid"), false}, {"text/plain", []byte{255}, false},
	} {
		_, err := ValidateTypedVerificationArtifact(test.media, test.data)
		if (err == nil) != test.valid {
			t.Errorf("%s valid=%v: %v", test.media, test.valid, err)
		}
	}
}

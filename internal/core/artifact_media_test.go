package core

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/testimage"
)

func TestArtifactMedia(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	fixtures := map[string][]byte{}
	for media, encode := range map[string]func(*bytes.Buffer) error{
		"image/png":  func(b *bytes.Buffer) error { return png.Encode(b, img) },
		"image/jpeg": func(b *bytes.Buffer) error { return jpeg.Encode(b, img, nil) },
		"image/gif":  func(b *bytes.Buffer) error { return gif.Encode(b, img, nil) },
	} {
		var b bytes.Buffer
		if err := encode(&b); err != nil {
			t.Fatal(err)
		}
		fixtures[media] = b.Bytes()
	}
	webp, err := base64.StdEncoding.DecodeString("UklGRiIAAABXRUJQVlA4IBYAAAAwAQCdASoBAAEADsD+JaQAA3AAAAAA")
	if err != nil {
		t.Fatal(err)
	}
	fixtures["image/webp"] = webp
	for media, content := range fixtures {
		t.Run(media, func(t *testing.T) {
			for _, declared := range []string{media, media + "; charset=binary", "", "application/octet-stream"} {
				got, err := ValidateArtifactMedia(declared, content)
				if err != nil || got != media {
					t.Fatalf("%s: %s %v", declared, got, err)
				}
			}
			wrong := "image/png"
			if media == wrong {
				wrong = "image/jpeg"
			}
			if _, err := ValidateArtifactMedia(wrong, content); err == nil || !strings.Contains(err.Error(), wrong) || !strings.Contains(err.Error(), media) {
				t.Fatalf("mismatch: %v", err)
			}
			if _, err := ValidateArtifactMedia(media, content[:len(content)/2]); err == nil {
				t.Fatal("truncated image accepted")
			}
		})
	}
	for _, declared := range []string{"text/plain", "application/pdf"} {
		if got, err := ValidateArtifactMedia(declared, []byte("document")); err != nil || got != declared {
			t.Fatalf("non-image: %s %v", got, err)
		}
	}
	if got, err := ValidateArtifactMedia("application/octet-stream", []byte("text")); err != nil || got != "text/plain; charset=utf-8" {
		t.Fatalf("inference: %s %v", got, err)
	}
	for _, content := range [][]byte{[]byte("broken"), []byte("\x89PNG\r\n\x1a\n")} {
		if _, err := ValidateArtifactMedia("image/png", content); err == nil {
			t.Fatal("invalid image accepted")
		}
	}
	if _, err := ValidateArtifactMedia("text/plain", make([]byte, MaxArtifactBytes+1)); err == nil {
		t.Fatal("oversized input accepted")
	}
}

// TestValidateVerificationEvidenceArtifact pins the legacy evidence byte
// policy: declared recordings are byte-checked like typed retention, so a
// declaration alone never establishes the type (req-review-gates-evidence
// AC-8.1; component-artifacts).
func TestValidateVerificationEvidenceArtifact(t *testing.T) {
	mp4, webm := testimage.MP4("legacy"), testimage.WebM("legacy")
	for _, tc := range []struct {
		name, media, want string
		content           []byte
	}{
		{"mp4", "video/mp4", "video/mp4", mp4},
		{"mp4 parameters and case", `VIDEO/MP4; codecs="avc1.42E01E"`, "video/mp4", mp4},
		{"webm", "video/webm", "video/webm", webm},
		{"webm parameters", "video/webm; codecs=vp9", "video/webm", webm},
		{"png", "image/png", "image/png", testimage.PNG("legacy")},
		{"jpeg", "image/jpeg", "image/jpeg", testimage.JPEG("legacy")},
		{"webp", "image/webp", "image/webp", testimage.WebP("legacy")},
		{"empty recording", "video/mp4", "", nil},
		{"spoofed mp4", "video/mp4", "", testimage.PNG("spoofed")},
		{"spoofed webm", "video/webm", "", []byte("plain text named capture.webm")},
		{"truncated mp4", "video/mp4", "", mp4[:len(mp4)-3]},
		{"mp4 bytes declared webm", "video/webm", "", mp4},
		{"webm bytes declared mp4", "video/mp4", "", webm},
		{"gif", "image/gif", "", testimage.GIF("legacy")},
		{"image declared over recording", "image/png", "", mp4},
	} {
		got, err := ValidateVerificationEvidenceArtifact(tc.media, tc.content)
		if tc.want == "" {
			if err == nil {
				t.Errorf("%s: admitted as %s", tc.name, got)
			} else if !strings.HasPrefix(err.Error(), "verification evidence") {
				t.Errorf("%s: refusal %q lacks the verification evidence prefix", tc.name, err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%s: got %q, %v; want %s", tc.name, got, err, tc.want)
		}
	}
}

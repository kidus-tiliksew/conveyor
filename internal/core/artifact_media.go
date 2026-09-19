package core

import (
	"bytes"
	"fmt"
	"image"
	"image/gif"
	_ "image/jpeg"
	_ "image/png"
	"mime"
	"net/http"
	"strings"

	_ "golang.org/x/image/webp"
)

const MaxArtifactBytes = 25 << 20

func SupportedArtifactImage(mediaType string) bool {
	switch mediaType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	}
	return false
}

// ValidateArtifactMedia enforces ART-HTTP-1 and ART-STORE-1
// (component-http-api, component-persistence; req-intake-and-triage REQ-1).
// Explicit non-image declarations retain their existing behavior.
func ValidateArtifactMedia(declared string, content []byte) (string, error) {
	detected := strings.Split(http.DetectContentType(content), ";")[0]
	fail := func(err error) (string, error) {
		return "", fmt.Errorf("artifact media declared %q, detected %q: %w", declared, detected, err)
	}
	if len(content) > MaxArtifactBytes {
		return fail(fmt.Errorf("artifact exceeds 25 MiB"))
	}
	normalized := strings.TrimSpace(declared)
	if normalized != "" {
		var err error
		normalized, _, err = mime.ParseMediaType(normalized)
		if err != nil {
			return fail(fmt.Errorf("invalid media type: %w", err))
		}
		normalized = strings.ToLower(normalized)
	}
	inferred := normalized == "" || normalized == "application/octet-stream"
	if inferred {
		normalized = detected
	}
	if !strings.HasPrefix(normalized, "image/") {
		if !inferred {
			return strings.TrimSpace(declared), nil
		}
		return http.DetectContentType(content), nil
	}
	if !SupportedArtifactImage(normalized) || !SupportedArtifactImage(detected) {
		return fail(fmt.Errorf("unsupported or invalid image; supported types are PNG, JPEG, GIF and WebP"))
	}
	if normalized != detected {
		return fail(fmt.Errorf("declared and detected image types differ; use %s", detected))
	}
	// Decode the entire image, not only its signature or configuration.
	var err error
	if detected == "image/gif" {
		_, err = gif.DecodeAll(bytes.NewReader(content))
	} else {
		_, _, err = image.Decode(bytes.NewReader(content))
	}
	if err != nil {
		return fail(fmt.Errorf("corrupt image: %w", err))
	}
	return detected, nil
}

package core

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"mime"
	"strings"
	"unicode/utf8"
)

// ValidateTypedVerificationArtifact is the dedicated VK-6 byte policy. The
// legacy visual-only role and its eligibility predicate remain unchanged.
func ValidateTypedVerificationArtifact(contentType string, content []byte) (string, error) {
	media, _, err := mime.ParseMediaType(contentType)
	if err != nil || len(content) == 0 || len(content) > MaxArtifactBytes {
		return "", fmt.Errorf("invalid typed verification artifact")
	}
	media = strings.ToLower(media)
	switch media {
	case "text/plain":
		if !utf8.Valid(content) {
			return "", fmt.Errorf("typed text must be UTF-8")
		}
	case "application/json":
		if !json.Valid(content) {
			return "", fmt.Errorf("typed JSON is invalid")
		}
	case "image/png", "image/jpeg", "image/webp":
		if _, err = NormalizeVerificationEvidenceContentType(media, int64(len(content))); err != nil {
			return "", err
		}
		if _, err = ValidateArtifactMedia(media, content); err != nil {
			return "", err
		}
	case "video/mp4", "video/webm":
		if _, err = ValidateArtifactMedia(media, content, TypedVerificationMedia); err != nil {
			return "", err
		}
	default:
		return "", fmt.Errorf("unsupported typed verification media")
	}
	return media, nil
}

// validateVerificationRecording sniffs bounded container structure, not codecs.
func validateVerificationRecording(media string, content []byte) (string, error) {
	switch media {
	case "video/mp4":
		// ISO BMFF boxes must cover the complete retained byte sequence. The first
		// box is a file type box; an ftyp label alone is not a recording.
		if len(content) < 24 || string(content[4:8]) != "ftyp" {
			return "", fmt.Errorf("invalid MP4 container")
		}
		found := false
		for offset := 0; offset < len(content); {
			if len(content)-offset < 8 {
				return "", fmt.Errorf("truncated MP4 box")
			}
			n := int(binary.BigEndian.Uint32(content[offset : offset+4]))
			if n < 8 || n > len(content)-offset {
				return "", fmt.Errorf("invalid MP4 box size")
			}
			if string(content[offset+4:offset+8]) == "mdat" && n > 8 {
				found = true
			}
			offset += n
		}
		if !found {
			return "", fmt.Errorf("MP4 has no media data")
		}
	case "video/webm":
		if len(content) < 16 || !bytes.HasPrefix(content, []byte{0x1a, 0x45, 0xdf, 0xa3}) || !bytes.Contains(content, []byte("webm")) || !bytes.Contains(content, []byte{0x1f, 0x43, 0xb6, 0x75}) {
			return "", fmt.Errorf("invalid WebM container")
		}
	default:
		return "", fmt.Errorf("unsupported verification recording")
	}
	return media, nil
}

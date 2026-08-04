package core

import (
	"bytes"
	"fmt"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// ReferenceDocument is informative workspace context. It intentionally has no
// requirement identity or citation surface (spec §21.58 change 1).
type ReferenceDocument struct {
	ID             string    `json:"id"`
	Workspace      string    `json:"workspace"`
	Name           string    `json:"name"`
	CurrentVersion int       `json:"current_version"`
	DeletedAt      time.Time `json:"deleted_at,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type ReferenceDocumentVersion struct {
	DocumentID        string    `json:"document_id"`
	Workspace         string    `json:"workspace"`
	Version           int       `json:"version"`
	Filename          string    `json:"filename"`
	ContentType       string    `json:"content_type"`
	Content           string    `json:"content"`
	SupersedesVersion int       `json:"supersedes_version,omitempty"`
	CreatedBy         string    `json:"created_by"`
	CreatedAt         time.Time `json:"created_at"`
}

func ValidateReferenceDocumentName(name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("reference document name is required")
	}
	if strings.ContainsAny(name, "`~") {
		return fmt.Errorf("reference document name must not contain backticks or tildes")
	}
	return nil
}

func NormalizeReferenceMarkdown(filename, contentType string, content []byte) (string, error) {
	mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(contentType))
	if err != nil {
		return "", fmt.Errorf("reference document content type is invalid: %w", err)
	}
	extension := strings.ToLower(filepath.Ext(strings.TrimSpace(filename)))
	mediaType = strings.ToLower(mediaType)
	if extension != ".md" && extension != ".markdown" {
		return "", fmt.Errorf("reference documents must use a .md or .markdown filename")
	}
	if mediaType != "text/markdown" && mediaType != "text/x-markdown" && mediaType != "text/plain" && mediaType != "application/octet-stream" {
		return "", fmt.Errorf("reference document content type %q is not Markdown", mediaType)
	}
	if !utf8.Valid(content) || bytes.IndexByte(content, 0) >= 0 {
		return "", fmt.Errorf("reference document must contain text")
	}
	detected := strings.ToLower(strings.TrimSpace(strings.SplitN(http.DetectContentType(content), ";", 2)[0]))
	// Markdown may begin with a raw HTML comment or container. DetectContentType
	// reports those ordinary Markdown documents as text/html.
	if detected != "text/plain" && detected != "text/html" && detected != "application/octet-stream" {
		return "", fmt.Errorf("reference document content is not Markdown text")
	}
	text := strings.TrimSpace(string(content))
	if text == "" {
		return "", fmt.Errorf("reference document must not be empty")
	}
	return text, nil
}

func ReferenceDocumentVersionLineageID(documentID string, version int) string {
	return fmt.Sprintf("%s:v%d", documentID, version)
}

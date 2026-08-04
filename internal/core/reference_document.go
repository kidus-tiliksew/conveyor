package core

import (
	"fmt"
	"mime"
	"path/filepath"
	"strings"
	"time"
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

func NormalizeReferenceMarkdown(filename, contentType string, content []byte) (string, error) {
	text := strings.TrimSpace(string(content))
	mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(contentType))
	if err != nil {
		return "", fmt.Errorf("reference document content type is invalid: %w", err)
	}
	extension := strings.ToLower(filepath.Ext(strings.TrimSpace(filename)))
	mediaType = strings.ToLower(mediaType)
	if (extension != ".md" && extension != ".markdown") || (mediaType != "text/markdown" && mediaType != "text/x-markdown") {
		return "", fmt.Errorf("reference documents must use a .md or .markdown filename and text/markdown content type")
	}
	if text == "" {
		return "", fmt.Errorf("reference document must not be empty")
	}
	return text, nil
}

func ReferenceDocumentVersionLineageID(documentID string, version int) string {
	return fmt.Sprintf("%s:v%d", documentID, version)
}

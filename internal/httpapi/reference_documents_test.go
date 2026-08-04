package httpapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func referenceDocumentUploadRequest(t *testing.T, path, name, filename, contentType string, content []byte) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if name != "" {
		if err := writer.WriteField("name", name); err != nil {
			t.Fatal(err)
		}
	}
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, filename))
	header.Set("Content-Type", contentType)
	part, err := writer.CreatePart(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = part.Write(content); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, path+"?workspace_id=demo", &body)
	request.Header.Set("Authorization", "Bearer token")
	request.Header.Set("Content-Type", writer.FormDataContentType())
	return request
}

func TestReferenceDocumentUploadBoundary(t *testing.T) {
	server := NewServer(store.NewMemory())
	server.BearerToken = "token"
	server.Workspace = "demo"

	tests := []struct {
		name        string
		filename    string
		contentType string
		content     []byte
		wantStatus  int
	}{
		{name: "browser octet stream", filename: "overview.md", contentType: "application/octet-stream", content: []byte("# Overview\n\nFacts."), wantStatus: http.StatusCreated},
		{name: "generic text", filename: "details.markdown", contentType: "text/plain", content: []byte("# Details"), wantStatus: http.StatusCreated},
		{name: "invalid extension", filename: "overview.txt", contentType: "text/plain", content: []byte("# Overview"), wantStatus: http.StatusBadRequest},
		{name: "known bad media", filename: "overview.md", contentType: "application/pdf", content: []byte("content"), wantStatus: http.StatusBadRequest},
		{name: "pdf disguised as octet stream", filename: "overview.md", contentType: "application/octet-stream", content: []byte("%PDF-1.7\n"), wantStatus: http.StatusBadRequest},
		{name: "empty", filename: "overview.md", contentType: "text/markdown", content: nil, wantStatus: http.StatusBadRequest},
		{name: "malformed media", filename: "overview.md", contentType: "not a media type", content: []byte("# Overview"), wantStatus: http.StatusBadRequest},
		{name: "oversized", filename: "overview.md", contentType: "text/markdown", content: bytes.Repeat([]byte("a"), maxReferenceDocumentBytes+(1<<20)), wantStatus: http.StatusRequestEntityTooLarge},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, referenceDocumentUploadRequest(t, "/v1/reference-documents", test.name, test.filename, test.contentType, test.content))
			if response.Code != test.wantStatus {
				t.Fatalf("status=%d body=%s, want %d", response.Code, response.Body, test.wantStatus)
			}
		})
	}
}

type failingReferenceDocumentStore struct{ store.Store }

func (failingReferenceDocumentStore) CreateReferenceDocument(context.Context, core.ReferenceDocument, core.ReferenceDocumentVersion) (core.ReferenceDocument, core.ReferenceDocumentVersion, error) {
	return core.ReferenceDocument{}, core.ReferenceDocumentVersion{}, errors.New("database password leaked")
}

func TestCreateReferenceDocumentMapsStoreErrors(t *testing.T) {
	st := store.NewMemory()
	server := NewServer(st)
	server.BearerToken = "token"
	server.Workspace = "demo"

	create := func(name string) *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, referenceDocumentUploadRequest(t, "/v1/reference-documents", name, name+".md", "text/markdown", []byte("# Overview")))
		return response
	}
	if response := create("Overview"); response.Code != http.StatusCreated {
		t.Fatalf("first create status=%d body=%s", response.Code, response.Body)
	}
	if response := create("overview"); response.Code != http.StatusConflict || response.Body.String() != "a reference document with that name already exists\n" {
		t.Fatalf("conflict status=%d body=%q", response.Code, response.Body.String())
	}

	failing := NewServer(failingReferenceDocumentStore{Store: st})
	failing.BearerToken = "token"
	failing.Workspace = "demo"
	response := httptest.NewRecorder()
	failing.Handler().ServeHTTP(response, referenceDocumentUploadRequest(t, "/v1/reference-documents", "Failure", "failure.md", "text/markdown", []byte("# Failure")))
	if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), "password") {
		t.Fatalf("unexpected failure status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestReferenceDocumentNameConflictClassification(t *testing.T) {
	conflict := &pgconn.PgError{Code: "23505", ConstraintName: "reference_documents_live_name_idx", Message: "raw constraint detail"}
	if !isReferenceDocumentNameConflict(conflict) {
		t.Fatal("production-store live-name conflict was not classified")
	}
	other := &pgconn.PgError{Code: "23505", ConstraintName: "some_other_constraint"}
	if isReferenceDocumentNameConflict(other) {
		t.Fatal("unrelated unique violation was classified as a live-name conflict")
	}
}

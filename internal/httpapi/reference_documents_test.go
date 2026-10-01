package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"strings"
	"testing"

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
		{name: "HTML comment", filename: "comment.md", contentType: "text/markdown", content: []byte("<!-- centered -->\n# Overview"), wantStatus: http.StatusCreated},
		{name: "HTML container", filename: "container.md", contentType: "text/markdown", content: []byte(`<div align="center">Overview</div>`), wantStatus: http.StatusCreated},
		{name: "invalid extension", filename: "overview.txt", contentType: "text/plain", content: []byte("# Overview"), wantStatus: http.StatusBadRequest},
		{name: "known bad media", filename: "overview.md", contentType: "application/pdf", content: []byte("content"), wantStatus: http.StatusBadRequest},
		{name: "pdf disguised as octet stream", filename: "overview.md", contentType: "application/octet-stream", content: []byte("%PDF-1.7\n"), wantStatus: http.StatusBadRequest},
		{name: "png disguised as markdown", filename: "overview.md", contentType: "application/octet-stream", content: []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}, wantStatus: http.StatusBadRequest},
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

func TestReferenceDocumentNameRejectsFenceCharacters(t *testing.T) {
	server := NewServer(store.NewMemory())
	server.BearerToken = "token"
	server.Workspace = "demo"
	for _, name := range []string{"Overview ```", "Overview~~~"} {
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, referenceDocumentUploadRequest(t, "/v1/reference-documents", name, "overview.md", "text/markdown", []byte("# Overview")))
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "backticks or tildes") {
			t.Fatalf("name=%q status=%d body=%q", name, response.Code, response.Body.String())
		}
	}
}

func TestReferenceDocumentUploadOverLimitDoesNotSpool(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("TMPDIR", tempDir)

	server := NewServer(store.NewMemory())
	server.BearerToken = "token"
	server.Workspace = "demo"
	request := referenceDocumentUploadRequest(
		t,
		"/v1/reference-documents",
		"Overview",
		"overview.md",
		"text/markdown",
		bytes.Repeat([]byte("a"), maxReferenceDocumentBytes+1),
	)
	if request.ContentLength >= maxReferenceUploadBytes {
		t.Fatalf("test request length=%d must exercise the parser below the body cap=%d", request.ContentLength, maxReferenceUploadBytes)
	}

	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d body=%s, want %d", response.Code, response.Body, http.StatusRequestEntityTooLarge)
	}
	entries, err := os.ReadDir(tempDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("multipart parser created temp files for an oversized upload: %v", entries)
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
	conflict := fmt.Errorf("wrapped: %w", store.ErrReferenceDocumentNameConflict)
	if !isReferenceDocumentNameConflict(conflict) {
		t.Fatal("production-store live-name conflict was not classified")
	}
	other := errors.New("reference document name already exists")
	if isReferenceDocumentNameConflict(other) {
		t.Fatal("unrelated unique violation was classified as a live-name conflict")
	}
}

// referenceDocumentRoleHarness serves the reference-document routes through
// real credential verification and the recording membership fixture, seeded
// with one live document in workspace demo.
type referenceDocumentRoleHarness struct {
	store       store.Store
	ctx         context.Context
	fixture     *membershipFixture
	credentials staticCredentialVerifier
	server      *Server
	document    core.ReferenceDocument
}

type referenceDocumentWrite struct {
	name       string
	wantStatus int
	request    func(token string) *http.Request
}

func newReferenceDocumentRoleHarness(t *testing.T) *referenceDocumentRoleHarness {
	t.Helper()
	st := store.NewMemory()
	ctx := store.WithWorkspace(t.Context(), "demo")
	document, _, err := st.CreateReferenceDocument(ctx, core.ReferenceDocument{ID: "ref-overview", Name: "Overview"}, core.ReferenceDocumentVersion{Filename: "overview.md", ContentType: "text/markdown", Content: "# One"})
	if err != nil {
		t.Fatal(err)
	}
	fixture := &membershipFixture{workspaces: []core.Workspace{{ID: "demo"}}, roles: map[string]map[string]core.WorkspaceRole{}}
	credentials := staticCredentialVerifier{}
	server := NewServer(st)
	server.Workspaces, server.Memberships, server.Credentials = fixture, fixture, credentials
	return &referenceDocumentRoleHarness{store: st, ctx: ctx, fixture: fixture, credentials: credentials, server: server, document: document}
}

// member binds a personal access token for a demo member holding role.
func (h *referenceDocumentRoleHarness) member(role core.WorkspaceRole) string {
	userID, token := string(role), string(role)+"-token"
	scope := core.CredentialScopeUser
	if role == core.WorkspaceRoleOperator {
		scope = core.CredentialScopeOperator
	}
	h.fixture.roles[userID] = map[string]core.WorkspaceRole{"demo": role}
	h.credentials[token] = core.AuthenticatedCredential{ID: "pat_" + userID, OwnerUserID: userID, Kind: core.CredentialUser, Scope: scope}
	return token
}

// writes returns valid create, supersede, and delete requests in lifecycle
// order, so an admitted caller creates "Created", revises the seeded document,
// and then removes it from the active set.
func (h *referenceDocumentRoleHarness) writes(t *testing.T) []referenceDocumentWrite {
	t.Helper()
	authorize := func(request *http.Request, token string) *http.Request {
		request.Header.Set("Authorization", "Bearer "+token)
		return request
	}
	return []referenceDocumentWrite{
		{name: "create", wantStatus: http.StatusCreated, request: func(token string) *http.Request {
			return authorize(referenceDocumentUploadRequest(t, "/v1/reference-documents", "Created", "created.md", "text/markdown", []byte("# Created")), token)
		}},
		{name: "supersede", wantStatus: http.StatusCreated, request: func(token string) *http.Request {
			return authorize(referenceDocumentUploadRequest(t, "/v1/reference-documents/"+h.document.ID+"/versions", "", "overview.md", "text/markdown", []byte("# Two")), token)
		}},
		{name: "delete", wantStatus: http.StatusNoContent, request: func(token string) *http.Request {
			return authorize(httptest.NewRequest(http.MethodDelete, "/v1/reference-documents/"+h.document.ID+"?workspace_id=demo", nil), token)
		}},
	}
}

func (h *referenceDocumentRoleHarness) serve(request *http.Request) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	h.server.Handler().ServeHTTP(response, request)
	return response
}

// Maintainers manage informative reference documents over REST: upload,
// supersede, and remove, with live-name uniqueness and retained history
// unchanged (req-accounts-and-membership AC-2.7; req-260805-3f9a04 AC-1.4).
func TestReferenceDocumentLifecycleForManagingRoles(t *testing.T) {
	for _, role := range []core.WorkspaceRole{core.WorkspaceRoleMaintainer, core.WorkspaceRoleOperator} {
		t.Run(string(role), func(t *testing.T) {
			harness := newReferenceDocumentRoleHarness(t)
			token := harness.member(role)
			read := func(path string, into any) {
				t.Helper()
				request := httptest.NewRequest(http.MethodGet, path+"?workspace_id=demo", nil)
				request.Header.Set("Authorization", "Bearer "+token)
				response := harness.serve(request)
				if response.Code != http.StatusOK {
					t.Fatalf("GET %s status=%d body=%s", path, response.Code, response.Body)
				}
				if err := json.Unmarshal(response.Body.Bytes(), into); err != nil {
					t.Fatal(err)
				}
			}
			upload := func(path, name, content string) *httptest.ResponseRecorder {
				request := referenceDocumentUploadRequest(t, path, name, "guide.md", "text/markdown", []byte(content))
				request.Header.Set("Authorization", "Bearer "+token)
				return harness.serve(request)
			}

			response := upload("/v1/reference-documents", "Guide", "# Guide one")
			if response.Code != http.StatusCreated {
				t.Fatalf("create status=%d body=%s", response.Code, response.Body)
			}
			var created struct {
				Document core.ReferenceDocument        `json:"document"`
				Version  core.ReferenceDocumentVersion `json:"version"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
				t.Fatal(err)
			}
			if created.Document.Name != "Guide" || created.Document.CurrentVersion != 1 || created.Version.Version != 1 || created.Version.CreatedBy != store.UserActorID(string(role)) {
				t.Fatalf("created=%+v", created)
			}
			if response = upload("/v1/reference-documents", "guide", "# Duplicate"); response.Code != http.StatusConflict {
				t.Fatalf("live-name conflict status=%d body=%s", response.Code, response.Body)
			}

			versionsPath := "/v1/reference-documents/" + created.Document.ID + "/versions"
			if response = upload(versionsPath, "", "# Guide two"); response.Code != http.StatusCreated {
				t.Fatalf("supersede status=%d body=%s", response.Code, response.Body)
			}
			var superseded core.ReferenceDocumentVersion
			if err := json.Unmarshal(response.Body.Bytes(), &superseded); err != nil {
				t.Fatal(err)
			}
			if superseded.Version != 2 || superseded.SupersedesVersion != 1 || superseded.Content != "# Guide two" {
				t.Fatalf("superseded=%+v", superseded)
			}

			request := httptest.NewRequest(http.MethodDelete, "/v1/reference-documents/"+created.Document.ID+"?workspace_id=demo", nil)
			request.Header.Set("Authorization", "Bearer "+token)
			if response = harness.serve(request); response.Code != http.StatusNoContent {
				t.Fatalf("delete status=%d body=%s", response.Code, response.Body)
			}
			var active []core.ReferenceDocument
			read("/v1/reference-documents", &active)
			for _, document := range active {
				if document.ID == created.Document.ID {
					t.Fatalf("removed document still active: %+v", active)
				}
			}
			var history []core.ReferenceDocumentVersion
			read(versionsPath, &history)
			if len(history) != 2 || history[0].Content != "# Guide one" || history[1].Content != "# Guide two" {
				t.Fatalf("retained history=%+v", history)
			}
			if response = upload(versionsPath, "", "# Guide three"); response.Code != http.StatusNotFound {
				t.Fatalf("supersede removed document status=%d body=%s", response.Code, response.Body)
			}
			// Removal frees only the live name; the retained document stays readable.
			if response = upload("/v1/reference-documents", "Guide", "# Guide again"); response.Code != http.StatusCreated {
				t.Fatalf("recreate after removal status=%d body=%s", response.Code, response.Body)
			}
		})
	}
}

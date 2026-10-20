package main

// REST adapter for `conveyor plan push` (feature-local-planning-drafts LP-1,
// LP-5; req-local-planning REQ-3; DEC-69, DEC-81(1)). It reads and posts
// through existing factory routes with the credential resolveClientConfig
// resolves. It refuses every confirm and dismiss route, so push can create
// only unconfirmed proposals and tasks (AC-3.4).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// planAPIError is a non-2xx factory answer. Message is the response body
// with surrounding whitespace removed.
type planAPIError struct {
	Method  string
	Path    string
	Status  int
	Message string
}

func (e *planAPIError) Error() string {
	return fmt.Sprintf("%s %s answered %d %s: %s", e.Method, e.Path, e.Status, http.StatusText(e.Status), e.Message)
}

// planAPIStatus returns the HTTP status of a factory refusal, or 0 for a
// transport or decoding failure.
func planAPIStatus(err error) int {
	var apiErr *planAPIError
	if errors.As(err, &apiErr) {
		return apiErr.Status
	}
	return 0
}

type planAPI struct {
	base       string
	token      string
	workspace  string
	configErr  error
	httpClient *http.Client
	credential func() string
}

func newPlanAPI() *planAPI {
	c := newClient()
	return &planAPI{base: c.base, token: c.token, workspace: c.workspace, configErr: c.configErr, httpClient: http.DefaultClient, credential: c.credentialDiagnostic}
}

// planForbiddenRoute reports a confirm or dismiss route. Push never sends one.
func planForbiddenRoute(path string) bool {
	clean := path
	if index := strings.IndexByte(clean, '?'); index >= 0 {
		clean = clean[:index]
	}
	for _, segment := range strings.Split(clean, "/") {
		if segment == "confirm" || segment == "dismiss" {
			return true
		}
	}
	return false
}

func (a *planAPI) send(ctx context.Context, method, path, contentType string, body []byte, out any) error {
	if a.configErr != nil {
		return a.configErr
	}
	if planForbiddenRoute(path) {
		return fmt.Errorf("plan push refuses %s %s: push never calls a confirm or dismiss route", method, path)
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, a.base+path, reader)
	if err != nil {
		return err
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	if a.token != "" {
		request.Header.Set("Authorization", "Bearer "+a.token)
	}
	if a.workspace != "" {
		request.Header.Set("X-Workspace-ID", a.workspace)
	}
	response, err := a.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		return fmt.Errorf("%s %s: read answer: %w", method, path, err)
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		message := strings.TrimSpace(string(data))
		if response.StatusCode == http.StatusUnauthorized && a.credential != nil {
			message = fmt.Sprintf("%s (%s)", message, a.credential())
		}
		return &planAPIError{Method: method, Path: path, Status: response.StatusCode, Message: message}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%s %s: decode answer: %w", method, path, err)
	}
	return nil
}

func (a *planAPI) get(ctx context.Context, path string, out any) error {
	return a.send(ctx, http.MethodGet, path, "", nil, out)
}

func (a *planAPI) postJSON(ctx context.Context, path string, body map[string]any, out any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	return a.send(ctx, http.MethodPost, path, "application/json", data, out)
}

// postTask sends the multipart form of `POST /v1/tasks` with a `task` JSON
// part and an `idempotency_key` part and no attachments (LP-5). JSON task
// intake ignores intake keys, so push never uses it.
func (a *planAPI) postTask(ctx context.Context, task map[string]any, intakeKey string, out any) error {
	data, err := planMultipartTask(task, intakeKey)
	if err != nil {
		return err
	}
	return a.send(ctx, http.MethodPost, "/v1/tasks", data.contentType, data.body, out)
}

type planMultipartBody struct {
	contentType string
	body        []byte
}

func planMultipartTask(task map[string]any, intakeKey string) (planMultipartBody, error) {
	metadata, err := json.Marshal(task)
	if err != nil {
		return planMultipartBody{}, err
	}
	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)
	if err := writer.WriteField("task", string(metadata)); err != nil {
		return planMultipartBody{}, err
	}
	if err := writer.WriteField("idempotency_key", intakeKey); err != nil {
		return planMultipartBody{}, err
	}
	if err := writer.Close(); err != nil {
		return planMultipartBody{}, err
	}
	return planMultipartBody{contentType: writer.FormDataContentType(), body: buffer.Bytes()}, nil
}

func planEscape(id string) string { return url.PathEscape(id) }

// planFound turns a 404 into found=false.
func planFound(err error) (bool, error) {
	if err == nil {
		return true, nil
	}
	if planAPIStatus(err) == http.StatusNotFound {
		return false, nil
	}
	return false, err
}

func (a *planAPI) requirement(ctx context.Context, id string) (core.Requirement, bool, error) {
	var view struct {
		Requirement core.Requirement `json:"requirement"`
	}
	found, err := planFound(a.get(ctx, "/v1/requirements/"+planEscape(id), &view))
	return view.Requirement, found, err
}

func (a *planAPI) requirementVersions(ctx context.Context, id string) ([]core.RequirementVersion, error) {
	var versions []core.RequirementVersion
	err := a.get(ctx, "/v1/requirements/"+planEscape(id)+"/versions", &versions)
	return versions, err
}

func (a *planAPI) requirements(ctx context.Context) ([]core.Requirement, error) {
	var summaries []struct {
		Requirement core.Requirement `json:"requirement"`
	}
	if err := a.get(ctx, "/v1/requirements?include_archived=true", &summaries); err != nil {
		return nil, err
	}
	result := make([]core.Requirement, 0, len(summaries))
	for _, summary := range summaries {
		result = append(result, summary.Requirement)
	}
	return result, nil
}

func (a *planAPI) systemDesign(ctx context.Context, id string) (core.SystemDesign, bool, error) {
	var view struct {
		Document core.SystemDesign `json:"document"`
	}
	found, err := planFound(a.get(ctx, "/v1/system-designs/"+planEscape(id), &view))
	return view.Document, found, err
}

func (a *planAPI) systemDesignVersions(ctx context.Context, id string) ([]core.SystemDesignVersion, error) {
	var versions []core.SystemDesignVersion
	err := a.get(ctx, "/v1/system-designs/"+planEscape(id)+"/versions", &versions)
	return versions, err
}

func (a *planAPI) systemDesigns(ctx context.Context) ([]core.SystemDesign, error) {
	var summaries []struct {
		Document core.SystemDesign `json:"document"`
	}
	if err := a.get(ctx, "/v1/system-designs?include_archived=true", &summaries); err != nil {
		return nil, err
	}
	result := make([]core.SystemDesign, 0, len(summaries))
	for _, summary := range summaries {
		result = append(result, summary.Document)
	}
	return result, nil
}

func (a *planAPI) decision(ctx context.Context, id string) (core.Decision, bool, error) {
	var decision core.Decision
	found, err := planFound(a.get(ctx, "/v1/decisions/"+planEscape(id), &decision))
	return decision, found, err
}

func (a *planAPI) decisions(ctx context.Context) ([]core.Decision, error) {
	var decisions []core.Decision
	err := a.get(ctx, "/v1/decisions", &decisions)
	return decisions, err
}

func (a *planAPI) task(ctx context.Context, id string) (core.Task, bool, error) {
	var task core.Task
	found, err := planFound(a.get(ctx, "/v1/tasks/"+planEscape(id), &task))
	return task, found, err
}

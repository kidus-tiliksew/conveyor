package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// memoReadScope resolves mutable admission inputs before looking up a response.
// Its returned context carries the exact configuration used to partition it.
func (s *Server) memoReadScope(ctx context.Context, kind string, query any) (context.Context, store.NextActionOptions, string, error) {
	ctx, cfg, err := s.requestConfiguration(ctx)
	if err != nil {
		return ctx, store.NextActionOptions{}, "", err
	}
	opts, err := s.nextActionOptionsFor(ctx)
	if err != nil {
		return ctx, opts, "", err
	}
	credential, _ := store.CredentialFromContext(ctx)
	workspace, _ := store.WorkspaceFromContext(ctx)
	encoded, err := json.Marshal([]any{kind, workspace, credential, opts.Capabilities, cfg, query})
	if err != nil {
		return ctx, opts, "", err
	}
	return ctx, opts, fmt.Sprintf("%x", sha256.Sum256(encoded)), nil
}

func memoReadUntil(now time.Time) time.Time {
	until := now.Add(readMemoLifetime)
	if boundary := now.Truncate(time.Minute).Add(time.Minute); boundary.Before(until) {
		until = boundary
	}
	return until
}

type memoResponse struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (w *memoResponse) Header() http.Header { return w.header }
func (w *memoResponse) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *memoResponse) Write(body []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	return w.body.Write(body)
}
func (w *memoResponse) Error() string { return fmt.Sprintf("read returned HTTP %d", w.status) }
func (w *memoResponse) send(dst http.ResponseWriter) {
	for key, values := range w.header {
		dst.Header()[key] = append([]string(nil), values...)
	}
	dst.WriteHeader(w.status)
	_, _ = dst.Write(w.body.Bytes())
}

// memoizedListTasks is the GET /v1/tasks route target. listTasks remains the
// uncached renderer so response envelopes and failure handling stay identical.
func (s *Server) memoizedListTasks(w http.ResponseWriter, r *http.Request) {
	query, _, err := parseTaskOperationsQuery(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if query.Limit == 0 {
		query.Limit = defaultTaskListLimit
	}
	s.memoizedRead(w, r, "tasks", query, s.listTasks)
}

func (s *Server) memoizedRead(w http.ResponseWriter, r *http.Request, kind string, query any, render http.HandlerFunc) {
	ctx, opts, key, err := s.memoReadScope(r.Context(), kind, query)
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	r = r.WithContext(ctx)
	if s.readMemo == nil {
		render(w, r)
		return
	}
	value, err := s.readMemo.get(ctx, key, func(ctx context.Context, now time.Time) (readMemoValue, error) {
		validation, err := s.Store.ReadActivityValidation(ctx, now)
		if err != nil {
			return readMemoValue{}, err
		}
		horizon := &readMemoHorizon{now: now, until: memoReadUntil(now)}
		ctx = context.WithValue(ctx, readMemoHorizonKey{}, horizon)
		ctx = context.WithValue(ctx, memoReadNowKey{}, now)
		ctx = context.WithValue(ctx, memoReadValidationKey{}, validation)
		response := &memoResponse{header: make(http.Header)}
		request := r.Clone(ctx)
		request.Header.Del("If-None-Match")
		render(response, request)
		if response.status != http.StatusOK {
			return readMemoValue{}, response
		}
		conditional := activityConditional{now: now, opts: opts, scope: []byte(key), validation: validation}
		value := readMemoValue{body: response.body.Bytes(), validUntil: horizon.until, header: map[string]string{}}
		value.etag = conditional.etag(value.validUntil, "")
		for key := range response.header {
			value.header[key] = response.header.Get(key)
		}
		return value, nil
	})
	if err != nil {
		if response, ok := err.(*memoResponse); ok {
			response.send(w)
		} else {
			writeStoreFailure(w, r, err)
		}
		return
	}
	for key, value := range value.header {
		w.Header().Set(key, value)
	}
	w.Header().Set("ETag", value.etag)
	if r.Header.Get("If-None-Match") == value.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(value.body)
}

type memoReadNowKey struct{}

type memoReadValidationKey struct{}

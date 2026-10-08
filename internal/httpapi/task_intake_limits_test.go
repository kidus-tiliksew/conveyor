package httpapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/testimage"
)

const intakeBoundary = "conveyor-intake-boundary"

// multipartIntakeBody streams a multipart task intake request without
// holding the filler in memory. The filler sits in a file part named field,
// so it is either an attachment or an ignored padding part.
type multipartIntakeBody struct {
	prefix, suffix string
	filler         int64
}

func newMultipartIntakeBody(intakeKey string, attachments map[string][]byte, fillerField string, filler int64, epilogue string) multipartIntakeBody {
	var prefix strings.Builder
	prefix.WriteString("--" + intakeBoundary + "\r\nContent-Disposition: form-data; name=\"task\"\r\n\r\n")
	prefix.WriteString(`{"body":"Bounded intake","repo":"api","hold":true}` + "\r\n")
	if intakeKey != "" {
		prefix.WriteString("--" + intakeBoundary + "\r\nContent-Disposition: form-data; name=\"idempotency_key\"\r\n\r\n" + intakeKey + "\r\n")
	}
	for name, content := range attachments {
		prefix.WriteString("--" + intakeBoundary + "\r\nContent-Disposition: form-data; name=\"attachments\"; filename=\"" + name + "\"\r\nContent-Type: application/octet-stream\r\n\r\n")
		prefix.Write(content)
		prefix.WriteString("\r\n")
	}
	prefix.WriteString("--" + intakeBoundary + "\r\nContent-Disposition: form-data; name=\"" + fillerField + "\"; filename=\"filler.txt\"\r\nContent-Type: text/plain\r\n\r\n")
	return multipartIntakeBody{prefix: prefix.String(), filler: filler, suffix: "\r\n--" + intakeBoundary + "--\r\n" + epilogue}
}

// overhead is the request size without filler bytes.
func (b multipartIntakeBody) overhead() int64 { return int64(len(b.prefix) + len(b.suffix)) }

func (b multipartIntakeBody) size() int64 { return b.overhead() + b.filler }

// withSize returns the same body with filler chosen so the request is exactly
// total bytes.
func (b multipartIntakeBody) withSize(t *testing.T, total int64) multipartIntakeBody {
	t.Helper()
	b.filler = total - b.overhead()
	if b.filler < 0 {
		t.Fatalf("request overhead %d exceeds %d", b.overhead(), total)
	}
	return b
}

type countingReader struct {
	reader io.Reader
	read   atomic.Int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.read.Add(int64(n))
	return n, err
}

type fillerReader struct{ remaining int64 }

func (r *fillerReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	for i := range p {
		p[i] = 'a'
	}
	r.remaining -= int64(len(p))
	return len(p), nil
}

// request builds the streamed request. knownLength false sends it with an
// unknown length, as a chunked upload arrives.
func (b multipartIntakeBody) request(knownLength bool) (*http.Request, *countingReader) {
	body := &countingReader{reader: io.MultiReader(strings.NewReader(b.prefix), &fillerReader{remaining: b.filler}, strings.NewReader(b.suffix))}
	request := httptest.NewRequest(http.MethodPost, "/v1/tasks", body)
	request.Header.Set("Authorization", "Bearer token")
	request.Header.Set("Content-Type", "multipart/form-data; boundary="+intakeBoundary)
	request.ContentLength = -1
	if knownLength {
		request.ContentLength = b.size()
	}
	return request, body
}

type intakeFixture struct {
	server   *Server
	store    store.Store
	titles   atomic.Int32
	enqueued atomic.Int32
	tempDir  string
}

// newIntakeFixture points the multipart parser's temporary files at an owned
// directory so a test can prove none remain.
func newIntakeFixture(t *testing.T, limit int64) *intakeFixture {
	t.Helper()
	tempDir := t.TempDir()
	t.Setenv("TMPDIR", tempDir)
	fixture := &intakeFixture{store: store.NewMemory(), tempDir: tempDir}
	fixture.server = NewServer(fixture.store)
	fixture.server.BearerToken, fixture.server.Workspace, fixture.server.Repos = "token", "demo", []string{"api"}
	fixture.server.multipartTaskIntakeLimit = limit
	fixture.server.GenerateTaskTitle = func(context.Context, core.Task) (string, error) {
		fixture.titles.Add(1)
		return "Bounded intake", nil
	}
	fixture.server.OnCreate = func(context.Context, string) { fixture.enqueued.Add(1) }
	return fixture
}

func (f *intakeFixture) serve(request *http.Request) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	f.server.Handler().ServeHTTP(response, request)
	return response
}

func (f *intakeFixture) assertNoTempFiles(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(f.tempDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("multipart parser left temporary files: %v", names)
	}
}

// assertNoSideEffects proves a refusal opened, titled, and persisted nothing.
func (f *intakeFixture) assertNoSideEffects(t *testing.T) {
	t.Helper()
	ctx := store.WithWorkspace(t.Context(), "demo")
	tasks, err := f.store.ListTasks(ctx)
	if err != nil || len(tasks) != 0 {
		t.Fatalf("refused intake created tasks: %+v %v", tasks, err)
	}
	artifacts, err := f.store.ListArtifacts(ctx)
	if err != nil || len(artifacts) != 0 {
		t.Fatalf("refused intake created artifacts: %+v %v", artifacts, err)
	}
	if f.titles.Load() != 0 || f.enqueued.Load() != 0 {
		t.Fatalf("refused intake called title generation %d time(s) and notification %d time(s)", f.titles.Load(), f.enqueued.Load())
	}
	f.assertNoTempFiles(t)
}

// component-artifacts "Atomic multipart intake": a request of exactly
// 268,435,456 bytes is admitted, and one byte more with a known length is
// refused before any body byte is read.
func TestMultipartTaskIntakeAggregateBoundary(t *testing.T) {
	if maxMultipartTaskIntakeBytes != 268435456 {
		t.Fatalf("maxMultipartTaskIntakeBytes = %d, want 268435456", maxMultipartTaskIntakeBytes)
	}
	attachment := map[string][]byte{"design.png": testimage.PNG("bounded")}

	t.Run("exact cap admitted", func(t *testing.T) {
		fixture := newIntakeFixture(t, 0)
		body := newMultipartIntakeBody("exact-cap", attachment, "padding", 0, "").withSize(t, maxMultipartTaskIntakeBytes)
		request, reader := body.request(true)
		response := fixture.serve(request)
		if response.Code != http.StatusCreated {
			t.Fatalf("exact-cap status=%d body=%s", response.Code, response.Body)
		}
		if reader.read.Load() != maxMultipartTaskIntakeBytes {
			t.Fatalf("handler read %d bytes, want the whole %d-byte request", reader.read.Load(), maxMultipartTaskIntakeBytes)
		}
		artifacts, err := fixture.store.ListArtifacts(store.WithWorkspace(t.Context(), "demo"))
		if err != nil || len(artifacts) != 1 {
			t.Fatalf("exact-cap attachments=%+v err=%v", artifacts, err)
		}
		fixture.assertNoTempFiles(t)
	})

	t.Run("exact cap with unknown length admitted", func(t *testing.T) {
		fixture := newIntakeFixture(t, 0)
		body := newMultipartIntakeBody("exact-cap-chunked", attachment, "padding", 0, "").withSize(t, maxMultipartTaskIntakeBytes)
		request, _ := body.request(false)
		if response := fixture.serve(request); response.Code != http.StatusCreated {
			t.Fatalf("exact-cap unknown-length status=%d body=%s", response.Code, response.Body)
		}
		fixture.assertNoTempFiles(t)
	})

	t.Run("cap plus one refused before reading", func(t *testing.T) {
		fixture := newIntakeFixture(t, 0)
		body := newMultipartIntakeBody("over-cap", attachment, "padding", 0, "").withSize(t, maxMultipartTaskIntakeBytes+1)
		request, reader := body.request(true)
		response := fixture.serve(request)
		if response.Code != http.StatusRequestEntityTooLarge || !strings.Contains(response.Body.String(), "268435456-byte multipart intake limit") {
			t.Fatalf("cap+1 status=%d body=%s", response.Code, response.Body)
		}
		if reader.read.Load() != 0 {
			t.Fatalf("known oversized request read %d body bytes before refusal", reader.read.Load())
		}
		fixture.assertNoSideEffects(t)
	})
}

// Overflow without a usable length is detected by the bounded reader, whether
// it lands in padding, inside an attachment, or after the closing boundary,
// and always before an attachment is opened, titled, or persisted.
func TestMultipartTaskIntakeOversizeBeforeAttachmentRead(t *testing.T) {
	const limit int64 = 64 << 10
	attachment := map[string][]byte{"design.png": testimage.PNG("bounded")}
	for name, test := range map[string]struct {
		body multipartIntakeBody
	}{
		"unknown length overflow in padding": {
			body: newMultipartIntakeBody("padding-overflow", attachment, "padding", 0, "").withSize(t, limit+1),
		},
		"overflow inside an attachment": {
			body: newMultipartIntakeBody("attachment-overflow", nil, "attachments", 0, "").withSize(t, limit+4096),
		},
		"one-byte epilogue excess": {
			body: newMultipartIntakeBody("epilogue-overflow", attachment, "padding", 0, strings.Repeat("e", 11)).withSize(t, limit+1),
		},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newIntakeFixture(t, limit)
			request, reader := test.body.request(false)
			response := fixture.serve(request)
			if response.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("status=%d body=%s", response.Code, response.Body)
			}
			if reader.read.Load() > limit+1 {
				t.Fatalf("handler read %d bytes past the %d-byte limit", reader.read.Load(), limit)
			}
			fixture.assertNoSideEffects(t)
		})
	}
	t.Run("epilogue within the limit admitted", func(t *testing.T) {
		fixture := newIntakeFixture(t, limit)
		body := newMultipartIntakeBody("epilogue-within", attachment, "padding", 0, strings.Repeat("e", 11)).withSize(t, limit)
		request, reader := body.request(false)
		if response := fixture.serve(request); response.Code != http.StatusCreated {
			t.Fatalf("status=%d body=%s", response.Code, response.Body)
		}
		if reader.read.Load() != limit {
			t.Fatalf("handler read %d bytes, want the whole %d-byte request including the epilogue", reader.read.Load(), limit)
		}
		if fixture.titles.Load() != 1 || fixture.enqueued.Load() != 1 {
			t.Fatalf("titles=%d enqueued=%d", fixture.titles.Load(), fixture.enqueued.Load())
		}
		fixture.assertNoTempFiles(t)
	})
}

// The bound changes no other intake rule: a malformed form within it answers
// 400, invalid media fails the whole request, and an idempotent replay
// answers 200 without a second task.
func TestMultipartTaskIntakeBoundPreservesValidation(t *testing.T) {
	t.Run("malformed form within the bound", func(t *testing.T) {
		fixture := newIntakeFixture(t, 0)
		request := httptest.NewRequest(http.MethodPost, "/v1/tasks", strings.NewReader("--"+intakeBoundary+"\r\nContent-Disposition: form-data; name=\"task\"\r\n\r\n{}"))
		request.Header.Set("Authorization", "Bearer token")
		request.Header.Set("Content-Type", "multipart/form-data; boundary="+intakeBoundary)
		response := fixture.serve(request)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "invalid attachment task form") {
			t.Fatalf("status=%d body=%s", response.Code, response.Body)
		}
		fixture.assertNoSideEffects(t)
	})
	t.Run("invalid media fails the whole request", func(t *testing.T) {
		fixture := newIntakeFixture(t, 0)
		body := newMultipartIntakeBody("bad-media", map[string][]byte{"fake.png": []byte("\x89PNG\r\n\x1a\nnot an image")}, "padding", 16, "")
		request, _ := body.request(true)
		response := fixture.serve(request)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "attachment fake.png") {
			t.Fatalf("status=%d body=%s", response.Code, response.Body)
		}
		fixture.assertNoSideEffects(t)
	})
	t.Run("idempotent replay", func(t *testing.T) {
		fixture := newIntakeFixture(t, 0)
		body := newMultipartIntakeBody("replay", map[string][]byte{"design.png": testimage.PNG("replay")}, "padding", 1024, "")
		first, _ := body.request(true)
		if response := fixture.serve(first); response.Code != http.StatusCreated {
			t.Fatalf("first status=%d body=%s", response.Code, response.Body)
		}
		replay, _ := body.request(true)
		if response := fixture.serve(replay); response.Code != http.StatusOK {
			t.Fatalf("replay status=%d body=%s", response.Code, response.Body)
		}
		ctx := store.WithWorkspace(t.Context(), "demo")
		tasks, err := fixture.store.ListTasks(ctx)
		if err != nil || len(tasks) != 1 {
			t.Fatalf("tasks=%+v err=%v", tasks, err)
		}
		created, err := fixture.store.CountEvents(ctx, tasks[0].ID, "task.created")
		if err != nil || created != 1 || fixture.enqueued.Load() != 1 {
			t.Fatalf("task.created=%d enqueued=%d err=%v", created, fixture.enqueued.Load(), err)
		}
		fixture.assertNoTempFiles(t)
	})
}

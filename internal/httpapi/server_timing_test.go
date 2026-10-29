package httpapi

import (
	"bufio"
	"bytes"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
)

const allMetrics = dbmetrics.QueryCount | dbmetrics.QueryDuration | dbmetrics.AcquireDuration

// measuredStore reports chosen request metric capabilities.
type measuredStore struct {
	store.Backend
	capability dbmetrics.Capability
}

func (m measuredStore) RequestMetrics() dbmetrics.Capability { return m.capability }

func timingServer(capability dbmetrics.Capability) *Server {
	return NewServer(measuredStore{Backend: store.NewVolatileBackend(), capability: capability})
}

// feed records two queries and one acquisition into the request collector.
func feed(r *http.Request) {
	collector := dbmetrics.FromContext(r.Context())
	collector.AddQuery(1500 * time.Microsecond)
	collector.AddQuery(250 * time.Microsecond)
	collector.AddAcquire(125 * time.Microsecond)
}

const fedValue = `db;dur=1.750, db_queries;desc="2", db_acquire;dur=0.125`

func serveTiming(t *testing.T, capability dbmetrics.Capability, path string, handler http.HandlerFunc) *http.Response {
	t.Helper()
	recorder := httptest.NewRecorder()
	timingServer(capability).serverTiming(handler).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	// Result reports the header as it was when the status was written.
	return recorder.Result()
}

func TestFormatServerTiming(t *testing.T) {
	snapshot := dbmetrics.Snapshot{Queries: 3, QueryDuration: 1234567 * time.Nanosecond, AcquireDuration: 125 * time.Microsecond}
	cases := []struct {
		capability dbmetrics.Capability
		want       string
	}{
		{allMetrics, `db;dur=1.234, db_queries;desc="3", db_acquire;dur=0.125`},
		{dbmetrics.QueryCount | dbmetrics.QueryDuration, `db;dur=1.234, db_queries;desc="3"`},
		{dbmetrics.QueryCount, `db_queries;desc="3"`},
		{dbmetrics.AcquireDuration, `db_acquire;dur=0.125`},
	}
	for _, tc := range cases {
		if got := formatServerTiming(snapshot, tc.capability); got != tc.want {
			t.Fatalf("capability %b: %q, want %q", tc.capability, got, tc.want)
		}
	}
	if got := formatServerTiming(dbmetrics.Snapshot{}, allMetrics); got != `db;dur=0.000, db_queries;desc="0", db_acquire;dur=0.000` {
		t.Fatalf("zero metrics = %q", got)
	}
	if got := formatServerTiming(dbmetrics.Snapshot{QueryDuration: 5 * time.Millisecond, AcquireDuration: 12*time.Second + 34*time.Microsecond}, allMetrics); got != `db;dur=5.000, db_queries;desc="0", db_acquire;dur=12000.034` {
		t.Fatalf("padding = %q", got)
	}
	saturated := formatServerTiming(dbmetrics.Snapshot{Queries: math.MaxUint64, QueryDuration: math.MaxInt64, AcquireDuration: math.MaxInt64}, allMetrics)
	if saturated != `db;dur=9223372036854.775, db_queries;desc="18446744073709551615", db_acquire;dur=9223372036854.775` || len(saturated) > 256 {
		t.Fatalf("saturated = %q (%d bytes)", saturated, len(saturated))
	}
	for _, b := range []byte(saturated) {
		if b > 0x7e || b < 0x20 {
			t.Fatalf("non-ASCII byte %q", b)
		}
	}
}

func TestServerTimingCommitPoints(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		status  int
		body    string
	}{
		{name: "explicit WriteHeader", status: http.StatusCreated, body: "made", handler: func(w http.ResponseWriter, r *http.Request) {
			feed(r)
			w.WriteHeader(http.StatusCreated)
			dbmetrics.FromContext(r.Context()).AddQuery(time.Second)
			_, _ = w.Write([]byte("made"))
		}},
		{name: "implicit Write", status: http.StatusOK, body: "body", handler: func(w http.ResponseWriter, r *http.Request) {
			feed(r)
			_, _ = w.Write([]byte("body"))
			dbmetrics.FromContext(r.Context()).AddQuery(time.Second)
		}},
		{name: "no-body response", status: http.StatusAccepted, handler: func(w http.ResponseWriter, r *http.Request) {
			feed(r)
			w.WriteHeader(http.StatusAccepted)
		}},
		{name: "error status", status: http.StatusConflict, body: "conflict\n", handler: func(w http.ResponseWriter, r *http.Request) {
			feed(r)
			http.Error(w, "conflict", http.StatusConflict)
		}},
		{name: "204", status: http.StatusNoContent, handler: func(w http.ResponseWriter, r *http.Request) {
			feed(r)
			w.WriteHeader(http.StatusNoContent)
		}},
		{name: "304", status: http.StatusNotModified, handler: func(w http.ResponseWriter, r *http.Request) {
			feed(r)
			w.Header().Set("ETag", `"v1"`)
			w.WriteHeader(http.StatusNotModified)
		}},
		{name: "ReadFrom", status: http.StatusOK, body: "streamed", handler: func(w http.ResponseWriter, r *http.Request) {
			feed(r)
			_, _ = io.Copy(w, strings.NewReader("streamed"))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response := serveTiming(t, allMetrics, "/v1/tasks", tc.handler)
			body, _ := io.ReadAll(response.Body)
			if response.StatusCode != tc.status || string(body) != tc.body {
				t.Fatalf("status %d body %q", response.StatusCode, body)
			}
			if got := response.Header.Values("Server-Timing"); len(got) != 1 || got[0] != fedValue {
				t.Fatalf("Server-Timing = %q", got)
			}
		})
	}
}

func TestServerTimingRecoveredPanicKeepsOrdinaryResponse(t *testing.T) {
	recorder := httptest.NewRecorder()
	server := timingServer(allMetrics)
	handler := server.serverTiming(middleware.Recoverer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		feed(r)
		panic("handler failed")
	})))
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/tasks", nil))
	response := recorder.Result()
	if response.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d", response.StatusCode)
	}
	if got := response.Header.Values("Server-Timing"); len(got) != 1 || got[0] != fedValue {
		t.Fatalf("Server-Timing = %q", got)
	}
}

// headerLog records what each WriteHeader call would have sent.
type headerLog struct {
	header http.Header
	sent   []string
}

func (h *headerLog) Header() http.Header { return h.header }
func (h *headerLog) Write(body []byte) (int, error) {
	h.WriteHeader(http.StatusOK)
	return len(body), nil
}
func (h *headerLog) WriteHeader(code int) {
	h.sent = append(h.sent, strings.Join(append([]string{http.StatusText(code)}, h.header.Values("Server-Timing")...), "|"))
}

func TestServerTimingInformationalDoesNotCommit(t *testing.T) {
	log := &headerLog{header: http.Header{}}
	timingServer(allMetrics).serverTiming(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusEarlyHints)
		feed(r)
		w.WriteHeader(http.StatusOK)
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(log, httptest.NewRequest(http.MethodGet, "/v1/tasks", nil))
	want := []string{"Early Hints", "OK|" + fedValue, "OK|" + fedValue}
	if strings.Join(log.sent, "\n") != strings.Join(want, "\n") {
		t.Fatalf("sent = %q, want %q", log.sent, want)
	}
}

func TestServerTimingCapabilitiesAndScope(t *testing.T) {
	response := serveTiming(t, allMetrics, "/v1", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	if got := response.Header.Get("Server-Timing"); got != `db;dur=0.000, db_queries;desc="0", db_acquire;dur=0.000` {
		t.Fatalf("zero metrics on /v1 = %q", got)
	}
	response = serveTiming(t, dbmetrics.QueryCount|dbmetrics.QueryDuration, "/v1/tasks", func(w http.ResponseWriter, r *http.Request) {
		feed(r)
		w.WriteHeader(http.StatusOK)
	})
	if got := response.Header.Get("Server-Timing"); got != `db;dur=1.750, db_queries;desc="2"` {
		t.Fatalf("without acquire = %q", got)
	}
	for _, path := range []string{"/healthz", "/mcp", "/v1x", "/v10/tasks", "/"} {
		response = serveTiming(t, allMetrics, path, func(w http.ResponseWriter, r *http.Request) {
			if dbmetrics.FromContext(r.Context()) != nil {
				t.Errorf("%s: a collector was attached", path)
			}
			w.WriteHeader(http.StatusOK)
		})
		if got := response.Header.Values("Server-Timing"); len(got) != 0 {
			t.Fatalf("%s: Server-Timing = %q", path, got)
		}
	}
}

func TestServerTimingThroughRouter(t *testing.T) {
	handler := timingServer(allMetrics).Handler()
	for _, tc := range []struct {
		path string
		want bool
	}{{"/v1/version", true}, {"/v1/tasks", true}, {"/healthz", false}, {"/mcp", false}} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, tc.path, nil))
		got := recorder.Result().Header.Values("Server-Timing")
		if tc.want && (len(got) != 1 || got[0] != `db;dur=0.000, db_queries;desc="0", db_acquire;dur=0.000`) {
			t.Fatalf("%s: status %d Server-Timing = %q", tc.path, recorder.Code, got)
		}
		if !tc.want && len(got) != 0 {
			t.Fatalf("%s: Server-Timing = %q", tc.path, got)
		}
	}
	// The volatile store measures nothing, so no header is added.
	recorder := httptest.NewRecorder()
	NewServer(store.NewVolatileBackend()).Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/version", nil))
	if recorder.Code != http.StatusOK || len(recorder.Result().Header.Values("Server-Timing")) != 0 {
		t.Fatalf("volatile store: %d %q", recorder.Code, recorder.Result().Header.Values("Server-Timing"))
	}
}

func TestServerTimingKeepsUnrelatedValue(t *testing.T) {
	response := serveTiming(t, allMetrics, "/v1/tasks", func(w http.ResponseWriter, r *http.Request) {
		feed(r)
		w.Header().Set("Server-Timing", "cache;desc=hit")
		_, _ = w.Write([]byte("a"))
		_, _ = w.Write([]byte("b"))
		w.WriteHeader(http.StatusOK)
	})
	if got := response.Header.Values("Server-Timing"); len(got) != 2 || got[0] != "cache;desc=hit" || got[1] != fedValue {
		t.Fatalf("Server-Timing = %q", got)
	}
}

func TestServerTimingSilentHandlerThroughRealServer(t *testing.T) {
	server := httptest.NewServer(timingServer(allMetrics).serverTiming(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		feed(r)
	})))
	defer server.Close()
	response, err := http.Get(server.URL + "/v1/tasks")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("Server-Timing") != fedValue {
		t.Fatalf("status %d Server-Timing %q", response.StatusCode, response.Header.Get("Server-Timing"))
	}
}

func TestServerTimingStreamsHeaderAtFirstFlush(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	// A failed assertion must not leave the handler blocked, or the deferred
	// server.Close would wait for it.
	releaseHandler := func() { releaseOnce.Do(func() { close(release) }) }
	finished := make(chan struct{})
	server := httptest.NewServer(timingServer(allMetrics).serverTiming(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(finished)
		feed(r)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("the wrapper hid http.Flusher")
			return
		}
		_, _ = w.Write([]byte("first\n"))
		flusher.Flush()
		<-release
		// Activity after the header was committed cannot reach it.
		dbmetrics.FromContext(r.Context()).AddQuery(time.Second)
		_, _ = w.Write([]byte("second\n"))
	})))
	defer server.Close()
	defer releaseHandler()
	response, err := http.Get(server.URL + "/v1/events")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	// The client has the header while the handler is still blocked.
	if got := response.Header.Get("Server-Timing"); got != fedValue {
		t.Fatalf("streamed Server-Timing = %q", got)
	}
	reader := bufio.NewReader(response.Body)
	if line, err := reader.ReadString('\n'); err != nil || line != "first\n" {
		t.Fatalf("first chunk %q %v", line, err)
	}
	releaseHandler()
	<-finished
	rest, err := io.ReadAll(reader)
	if err != nil || string(rest) != "second\n" {
		t.Fatalf("rest %q %v", rest, err)
	}
}

type plainWriter struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (p *plainWriter) Header() http.Header { return p.header }
func (p *plainWriter) Write(b []byte) (int, error) {
	if p.status == 0 {
		p.status = http.StatusOK
	}
	return p.body.Write(b)
}
func (p *plainWriter) WriteHeader(code int) { p.status = code }

type readerFromWriter struct {
	plainWriter
	readFrom int
}

func (r *readerFromWriter) ReadFrom(source io.Reader) (int64, error) {
	r.readFrom++
	return r.body.ReadFrom(source)
}

func TestServerTimingWriterCapabilities(t *testing.T) {
	check := func(name string, base http.ResponseWriter, wantFlusher, wantHijacker bool) {
		t.Helper()
		wrapped := (&timingWriter{base: base, collector: dbmetrics.NewCollector(), capability: allMetrics}).wrapped()
		_, flusher := wrapped.(http.Flusher)
		_, hijacker := wrapped.(http.Hijacker)
		if flusher != wantFlusher || hijacker != wantHijacker {
			t.Fatalf("%s: flusher %v hijacker %v", name, flusher, hijacker)
		}
		if _, ok := wrapped.(io.ReaderFrom); !ok {
			t.Fatalf("%s: ReadFrom missing", name)
		}
		if unwrapped := wrapped.(interface{ Unwrap() http.ResponseWriter }).Unwrap(); unwrapped != base {
			t.Fatalf("%s: Unwrap = %T", name, unwrapped)
		}
	}
	check("plain", &plainWriter{header: http.Header{}}, false, false)
	check("recorder", httptest.NewRecorder(), true, false)

	// ReadFrom delegates when the underlying writer has it and copies
	// through Write otherwise; both commit the header first.
	delegate := &readerFromWriter{plainWriter: plainWriter{header: http.Header{}}}
	timing := &timingWriter{base: delegate, collector: dbmetrics.NewCollector(), capability: dbmetrics.QueryCount}
	if _, err := timing.wrapped().(io.ReaderFrom).ReadFrom(strings.NewReader("abc")); err != nil || delegate.readFrom != 1 || delegate.body.String() != "abc" || delegate.header.Get("Server-Timing") != `db_queries;desc="0"` {
		t.Fatalf("delegated ReadFrom: %d %q %q %v", delegate.readFrom, delegate.body.String(), delegate.header.Get("Server-Timing"), err)
	}
	plain := &plainWriter{header: http.Header{}}
	timing = &timingWriter{base: plain, collector: dbmetrics.NewCollector(), capability: dbmetrics.QueryCount}
	if _, err := timing.wrapped().(io.ReaderFrom).ReadFrom(strings.NewReader("xyz")); err != nil || plain.body.String() != "xyz" || plain.status != http.StatusOK || plain.header.Get("Server-Timing") == "" {
		t.Fatalf("copied ReadFrom: %q %d %v", plain.body.String(), plain.status, err)
	}

	// A real HTTP/1 server's writer is both; hijacking and the response
	// controller reach it through the wrapper.
	server := httptest.NewServer(timingServer(allMetrics).serverTiming(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := w.(http.Flusher); !ok {
			t.Error("real writer lost http.Flusher")
		}
		if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(time.Minute)); err != nil {
			t.Errorf("response controller: %v", err)
		}
		if r.URL.Path != "/v1/hijack" {
			_, _ = w.Write([]byte("ok"))
			return
		}
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Error("real writer lost http.Hijacker")
			return
		}
		conn, buffered, err := hijacker.Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		_, _ = buffered.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 8\r\nConnection: close\r\n\r\nhijacked")
		_ = buffered.Flush()
	})))
	defer server.Close()
	response, err := http.Get(server.URL + "/v1/hijack")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if string(body) != "hijacked" || response.Header.Get("Server-Timing") != "" {
		t.Fatalf("hijacked response %q Server-Timing %q", body, response.Header.Get("Server-Timing"))
	}
	response, err = http.Get(server.URL + "/v1/plain")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.Header.Get("Server-Timing") == "" {
		t.Fatal("real server response lacks Server-Timing")
	}
}

package httpapi

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
)

// serverTiming attaches a request collector to every /v1 request and adds
// one Server-Timing value with the store's request database metrics just
// before the response header is committed. A store that measures nothing
// gets a pass-through (component-http-api, Database timing header).
func (s *Server) serverTiming(next http.Handler) http.Handler {
	capability := dbmetrics.CapabilityOf(s.Store)
	if capability == 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1" && !strings.HasPrefix(r.URL.Path, "/v1/") {
			next.ServeHTTP(w, r)
			return
		}
		collector := dbmetrics.NewCollector()
		timing := &timingWriter{base: w, collector: collector, capability: capability}
		next.ServeHTTP(timing.wrapped(), r.WithContext(dbmetrics.WithCollector(r.Context(), collector)))
		// A handler that wrote nothing gets its implicit 200 from net/http
		// after returning; add the header before that.
		timing.commit()
	})
}

// formatServerTiming renders the fields the capability allows, in a fixed
// order: at most three metrics and well under 256 bytes.
func formatServerTiming(snapshot dbmetrics.Snapshot, capability dbmetrics.Capability) string {
	value := make([]byte, 0, 96)
	separate := func() {
		if len(value) != 0 {
			value = append(value, ", "...)
		}
	}
	if capability&dbmetrics.QueryDuration != 0 {
		value = appendDurationMetric(value, "db", snapshot.QueryDuration)
	}
	if capability&dbmetrics.QueryCount != 0 {
		separate()
		value = append(value, `db_queries;desc="`...)
		value = strconv.AppendUint(value, snapshot.Queries, 10)
		value = append(value, '"')
	}
	if capability&dbmetrics.AcquireDuration != 0 {
		separate()
		value = appendDurationMetric(value, "db_acquire", snapshot.AcquireDuration)
	}
	return string(value)
}

// appendDurationMetric writes milliseconds with exactly three decimals from
// integer nanoseconds, so formatting never rounds through floating point.
func appendDurationMetric(value []byte, name string, d time.Duration) []byte {
	if d < 0 {
		d = 0
	}
	micros := int64(d / time.Microsecond)
	value = append(value, name...)
	value = append(value, ";dur="...)
	value = strconv.AppendInt(value, micros/1000, 10)
	value = append(value, '.')
	fraction := micros % 1000
	if fraction < 100 {
		value = append(value, '0')
	}
	if fraction < 10 {
		value = append(value, '0')
	}
	return strconv.AppendInt(value, fraction, 10)
}

// timingWriter adds the header once, at the first final WriteHeader, Write,
// ReadFrom, or Flush. It never buffers and passes everything else through.
type timingWriter struct {
	base       http.ResponseWriter
	collector  *dbmetrics.Collector
	capability dbmetrics.Capability
	committed  bool
}

// wrapped exposes Flusher and Hijacker only when the underlying writer does,
// so a handler's type assertion sees the capabilities it saw before.
func (w *timingWriter) wrapped() http.ResponseWriter {
	_, flusher := w.base.(http.Flusher)
	_, hijacker := w.base.(http.Hijacker)
	switch {
	case flusher && hijacker:
		return timingFlushHijacker{w}
	case flusher:
		return timingFlusher{w}
	case hijacker:
		return timingHijacker{w}
	default:
		return w
	}
}

func (w *timingWriter) commit() {
	if w.committed {
		return
	}
	w.committed = true
	w.base.Header().Add("Server-Timing", formatServerTiming(w.collector.Snapshot(), w.capability))
}

func (w *timingWriter) Header() http.Header { return w.base.Header() }

func (w *timingWriter) WriteHeader(code int) {
	// 1xx informational responses other than 101 do not commit the header.
	if code < 100 || code > 199 || code == http.StatusSwitchingProtocols {
		w.commit()
	}
	w.base.WriteHeader(code)
}

func (w *timingWriter) Write(body []byte) (int, error) {
	w.commit()
	return w.base.Write(body)
}

func (w *timingWriter) ReadFrom(source io.Reader) (int64, error) {
	w.commit()
	if readerFrom, ok := w.base.(io.ReaderFrom); ok {
		return readerFrom.ReadFrom(source)
	}
	return io.Copy(writerOnly{w.base}, source)
}

// Unwrap lets http.NewResponseController reach the underlying writer.
func (w *timingWriter) Unwrap() http.ResponseWriter { return w.base }

func (w *timingWriter) flush() {
	w.commit()
	w.base.(http.Flusher).Flush()
}

func (w *timingWriter) hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.base.(http.Hijacker).Hijack()
}

// writerOnly hides ReadFrom so io.Copy cannot recurse.
type writerOnly struct{ io.Writer }

type timingFlusher struct{ *timingWriter }

func (w timingFlusher) Flush() { w.flush() }

type timingHijacker struct{ *timingWriter }

func (w timingHijacker) Hijack() (net.Conn, *bufio.ReadWriter, error) { return w.hijack() }

type timingFlushHijacker struct{ *timingWriter }

func (w timingFlushHijacker) Flush() { w.flush() }
func (w timingFlushHijacker) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.hijack()
}

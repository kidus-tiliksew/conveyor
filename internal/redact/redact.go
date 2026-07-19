// Package redact removes credentials from transcripts before they cross the
// control-plane persistence boundary (spec §10.3, §21.4).
package redact

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

const (
	exactPlaceholder   = "[REDACTED:exact]"
	encodedPlaceholder = "[REDACTED:encoded]"
	patternPlaceholder = "[REDACTED:pattern]"
	entropyPlaceholder = "[REDACTED:entropy]"
)

// Stats is the wire-level count contract shared with persisted transcripts.
// It contains only match counts, never values.
type Stats = core.RedactionStats

type needle struct {
	value       string
	placeholder string
	class       string
}

// Redactor combines caller-known sensitive values with conservative
// well-known credential patterns. Values are held in memory only; callers
// persist only Stats.
type Redactor struct {
	needles []needle
}

func New(values []string) *Redactor {
	byValue := make(map[string]needle)
	add := func(value, placeholder, class string) {
		if value == "" {
			return
		}
		if existing, ok := byValue[value]; ok && existing.class == "exact" {
			return
		}
		byValue[value] = needle{value: value, placeholder: placeholder, class: class}
	}
	for _, value := range values {
		add(value, exactPlaceholder, "exact")
		encoded := []string{
			base64.StdEncoding.EncodeToString([]byte(value)),
			base64.RawStdEncoding.EncodeToString([]byte(value)),
			base64.URLEncoding.EncodeToString([]byte(value)),
			base64.RawURLEncoding.EncodeToString([]byte(value)),
			url.QueryEscape(value),
			url.PathEscape(value),
		}
		for _, candidate := range encoded {
			if candidate != value {
				add(candidate, encodedPlaceholder, "encoded")
			}
		}
	}
	needles := make([]needle, 0, len(byValue))
	for _, item := range byValue {
		needles = append(needles, item)
	}
	sort.Slice(needles, func(i, j int) bool { return len(needles[i].value) > len(needles[j].value) })
	return &Redactor{needles: needles}
}

var credentialPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`),
	regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{20,}\b`),
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}\b`),
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}\b`),
	regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`),
}

var assignedSecret = regexp.MustCompile(`(?i)((?:token|secret|password|api[_-]?key)\s*[:=]\s*["']?)([A-Za-z0-9+/=_-]{20,})`)

func (r *Redactor) Redact(text string) (string, Stats) {
	var stats Stats
	for _, item := range r.needles {
		count := strings.Count(text, item.value)
		if count == 0 {
			continue
		}
		text = strings.ReplaceAll(text, item.value, item.placeholder)
		if item.class == "exact" {
			stats.Exact += int64(count)
		} else {
			stats.Encoded += int64(count)
		}
	}
	for _, pattern := range credentialPatterns {
		matches := pattern.FindAllStringIndex(text, -1)
		if len(matches) == 0 {
			continue
		}
		stats.Pattern += int64(len(matches))
		text = pattern.ReplaceAllString(text, patternPlaceholder)
	}
	matches := assignedSecret.FindAllStringSubmatchIndex(text, -1)
	if len(matches) > 0 {
		stats.Entropy += int64(len(matches))
		text = assignedSecret.ReplaceAllString(text, `${1}`+entropyPlaceholder)
	}
	return text, stats
}

// RedactJSON walks every JSON string so values containing quotes or control
// characters are redacted before JSON escaping can obscure the match.
func (r *Redactor) RedactJSON(data []byte) ([]byte, Stats, error) {
	var value any
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, Stats{}, err
	}
	var stats Stats
	value = r.walk(value, &stats)
	out, err := json.Marshal(value)
	return out, stats, err
}

func (r *Redactor) walk(value any, stats *Stats) any {
	switch item := value.(type) {
	case string:
		clean, found := r.Redact(item)
		stats.Add(found)
		return clean
	case []any:
		for i := range item {
			item[i] = r.walk(item[i], stats)
		}
		return item
	case map[string]any:
		cleaned := make(map[string]any, len(item))
		for key, child := range item {
			cleanKey, found := r.Redact(key)
			stats.Add(found)
			cleaned[cleanKey] = r.walk(child, stats)
		}
		return cleaned
	default:
		return value
	}
}

// Writer applies the same boundary to shim diagnostics written to stderr.
type Writer struct {
	Destination io.Writer
	Redactor    *Redactor

	mu      sync.Mutex
	pending string
}

func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pending += string(p)
	boundary := strings.LastIndexByte(w.pending, '\n')
	if boundary < 0 {
		return len(p), nil
	}
	ready := w.pending[:boundary+1]
	w.pending = w.pending[boundary+1:]
	clean, _ := w.Redactor.Redact(ready)
	if _, err := io.WriteString(w.Destination, clean); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Flush emits a final unterminated line. Callers invoke it only after the
// source process has stopped writing, so exact values split across writes are
// still redacted without persisting a buffer of runtime values.
func (w *Writer) Flush() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.pending == "" {
		return nil
	}
	clean, _ := w.Redactor.Redact(w.pending)
	w.pending = ""
	_, err := io.WriteString(w.Destination, clean)
	return err
}

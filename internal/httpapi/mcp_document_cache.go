package httpapi

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// A search must not read every document body from the database on each call,
// and a version's content never changes (req-design-delivery REQ-2; DEC-103).
// This process-local cache therefore holds parsed immutable versions. It never
// holds membership, archive state, or current-version selection, which every
// call reads afresh. Of a version's status it keeps only a settled outcome:
// confirmed, dismissed, and retired are terminal, while a pending proposal is
// reread on every use, so a confirmation or dismissal shows immediately.
const (
	mcpDocumentCacheMaxBytes   = 16 << 20
	mcpDocumentCacheMaxEntries = 128
	// mcpDocumentCacheOverhead charges each parsed section and entry for its
	// bookkeeping, so many tiny sections cannot escape the byte bound.
	mcpDocumentCacheOverhead = 128
)

type mcpDocumentVersionKey struct {
	workspace, kind, id string
	version             int
}

// documentVersionStatus is a version row's confirmation outcome.
type documentVersionStatus struct {
	confirmed, retired bool
}

// parsedDocumentVersion is immutable once built.
type parsedDocumentVersion struct {
	content  string
	sections []documentSection
	// lowered holds each section's text in lower case for literal matching.
	lowered []string
	// status is meaningful for any entry; settled says it can never change.
	status  documentVersionStatus
	settled bool
	// cost is the retained size: the body plus its derived text.
	cost int64
}

func newParsedDocumentVersion(content string, status documentVersionStatus, settled bool) *parsedDocumentVersion {
	content = strings.ToValidUTF8(content, "�")
	parsed := &parsedDocumentVersion{content: content, sections: parseDocumentSections(content), status: status, settled: settled}
	parsed.lowered = make([]string, len(parsed.sections))
	parsed.cost = int64(len(content)) + mcpDocumentCacheOverhead
	for i, section := range parsed.sections {
		parsed.lowered[i] = strings.ToLower(parsed.sectionText(section))
		parsed.cost += int64(len(parsed.lowered[i])+len(section.Heading)+len(section.ID)) + mcpDocumentCacheOverhead
	}
	return parsed
}

func (p *parsedDocumentVersion) sectionText(section documentSection) string {
	return p.content[section.Start:section.End]
}

func (p *parsedDocumentVersion) section(id string) (documentSection, bool) {
	for _, section := range p.sections {
		if section.ID == id {
			return section, true
		}
	}
	return documentSection{}, false
}

type mcpDocumentCacheEntry struct {
	key    mcpDocumentVersionKey
	parsed *parsedDocumentVersion
}

type mcpDocumentFlight struct {
	done   chan struct{}
	parsed *parsedDocumentVersion
	err    error
}

// mcpDocumentCache is a byte- and entry-bounded LRU that deduplicates
// concurrent misses per key. Its zero value is ready for use.
type mcpDocumentCache struct {
	mu      sync.Mutex
	entries map[mcpDocumentVersionKey]*list.Element
	order   list.List
	bytes   int64
	flights map[mcpDocumentVersionKey]*mcpDocumentFlight
	// maxBytes and maxEntries are test seams; zero selects the defaults.
	maxBytes   int64
	maxEntries int
}

func (c *mcpDocumentCache) limits() (int64, int) {
	maxBytes, maxEntries := c.maxBytes, c.maxEntries
	if maxBytes == 0 {
		maxBytes = mcpDocumentCacheMaxBytes
	}
	if maxEntries == 0 {
		maxEntries = mcpDocumentCacheMaxEntries
	}
	return maxBytes, maxEntries
}

// load returns the resident version or runs loader once for every concurrent
// caller of the key. fresh reports that this caller's own loader produced the
// result. Authorization always precedes load, and a joined load whose leader's
// request was cancelled is retried under the joining caller's live context.
func (c *mcpDocumentCache) load(ctx context.Context, key mcpDocumentVersionKey, loader func() (*parsedDocumentVersion, error)) (*parsedDocumentVersion, bool, error) {
	for {
		c.mu.Lock()
		if element, resident := c.entries[key]; resident {
			c.order.MoveToFront(element)
			parsed := element.Value.(*mcpDocumentCacheEntry).parsed
			c.mu.Unlock()
			return parsed, false, nil
		}
		flight, joined := c.flights[key]
		if !joined {
			flight = &mcpDocumentFlight{done: make(chan struct{}), err: errors.New("document version load aborted")}
			if c.flights == nil {
				c.flights = map[mcpDocumentVersionKey]*mcpDocumentFlight{}
			}
			c.flights[key] = flight
		}
		c.mu.Unlock()
		if !joined {
			c.lead(key, flight, loader)
			return flight.parsed, true, flight.err
		}
		select {
		case <-flight.done:
		case <-ctx.Done():
			return nil, false, ctx.Err()
		}
		if flight.err == nil {
			return flight.parsed, false, nil
		}
		if !errors.Is(flight.err, context.Canceled) && !errors.Is(flight.err, context.DeadlineExceeded) {
			return nil, false, flight.err
		}
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
	}
}

func (c *mcpDocumentCache) lead(key mcpDocumentVersionKey, flight *mcpDocumentFlight, loader func() (*parsedDocumentVersion, error)) {
	defer func() {
		c.mu.Lock()
		delete(c.flights, key)
		if flight.err == nil {
			c.retainLocked(key, flight.parsed)
		}
		c.mu.Unlock()
		close(flight.done)
	}()
	flight.parsed, flight.err = loader()
}

// replace publishes a reread version with a newer status.
func (c *mcpDocumentCache) replace(key mcpDocumentVersionKey, parsed *parsedDocumentVersion) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.retainLocked(key, parsed)
}

// retainLocked admits parsed, evicting least-recently-used entries past either
// bound. An entry over the whole byte bound is served but never retained.
func (c *mcpDocumentCache) retainLocked(key mcpDocumentVersionKey, parsed *parsedDocumentVersion) {
	maxBytes, maxEntries := c.limits()
	if element, resident := c.entries[key]; resident {
		c.removeLocked(element)
	}
	if parsed.cost > maxBytes {
		return
	}
	if c.entries == nil {
		c.entries = map[mcpDocumentVersionKey]*list.Element{}
	}
	c.entries[key] = c.order.PushFront(&mcpDocumentCacheEntry{key: key, parsed: parsed})
	c.bytes += parsed.cost
	for c.bytes > maxBytes || len(c.entries) > maxEntries {
		c.removeLocked(c.order.Back())
	}
}

func (c *mcpDocumentCache) removeLocked(element *list.Element) {
	entry := element.Value.(*mcpDocumentCacheEntry)
	c.order.Remove(element)
	delete(c.entries, entry.key)
	c.bytes -= entry.parsed.cost
}

// documentVersion returns a version's parsed content and its current status.
// current means the document names this version as current, which the caller
// has just read and which implies confirmation for requirements and designs,
// so a warmed search of current versions reads no body. Only a pending version
// named explicitly rereads its row.
func (s *Server) documentVersion(ctx context.Context, kind, id string, version int, current bool) (*parsedDocumentVersion, documentVersionStatus, error) {
	workspace, _ := store.WorkspaceFromContext(ctx)
	key := mcpDocumentVersionKey{workspace: workspace, kind: kind, id: id, version: version}
	parsed, fresh, err := s.documentVersions.load(ctx, key, func() (*parsedDocumentVersion, error) {
		return s.readDocumentVersion(ctx, kind, id, version)
	})
	if err != nil {
		return nil, documentVersionStatus{}, err
	}
	switch {
	case kind == "reference":
		return parsed, documentVersionStatus{}, nil
	case current:
		return parsed, documentVersionStatus{confirmed: true}, nil
	case parsed.settled || fresh:
		return parsed, parsed.status, nil
	}
	if parsed, err = s.readDocumentVersion(ctx, kind, id, version); err != nil {
		return nil, documentVersionStatus{}, err
	}
	s.documentVersions.replace(key, parsed)
	return parsed, parsed.status, nil
}

// readDocumentVersion is the one body read behind the cache. A backend failure
// keeps its own error; every other failure reads as a missing version.
func (s *Server) readDocumentVersion(ctx context.Context, kind, id string, version int) (*parsedDocumentVersion, error) {
	var content string
	var status documentVersionStatus
	var err error
	settled := false
	switch kind {
	case "requirement":
		v, e := s.Store.GetRequirementVersion(ctx, id, version)
		content, status, settled, err = v.Content, documentVersionStatus{confirmed: v.Confirmed, retired: v.Retired}, v.Confirmed || v.Retired, e
	case "system_design":
		v, e := s.Store.GetSystemDesignVersion(ctx, id, version)
		content, status, settled, err = v.Content, documentVersionStatus{confirmed: v.Confirmed, retired: v.Dismissed}, v.Confirmed || v.Dismissed, e
	case "reference":
		v, e := s.Store.GetReferenceDocumentVersion(ctx, id, version)
		content, settled, err = v.Content, true, e
	default:
		return nil, fmt.Errorf("invalid document kind")
	}
	if err != nil {
		if errors.Is(err, store.ErrBackendOperation) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, store.ErrNotFound
	}
	return newParsedDocumentVersion(content, status, settled), nil
}

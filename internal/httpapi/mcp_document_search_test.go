package httpapi

// Real-server MCP coverage for search_documents (req-design-delivery AC-2.1 to
// AC-2.3, AC-4.2; DEC-103) and read-count coverage for its parsed-version cache.
// Search runs under a user credential and an agent credential with a live claim.

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

type documentSearchResult struct {
	Items             []map[string]any           `json:"items"`
	Total             int                        `json:"total"`
	Limit             int                        `json:"limit"`
	DocumentsSearched int                        `json:"documents_searched"`
	Truncated         bool                       `json:"truncated"`
	TruncatedBy       string                     `json:"truncated_by"`
	Advice            string                     `json:"advice"`
	ExcerptsTruncated int                        `json:"excerpts_truncated"`
	SkippedDocuments  []mcpDocumentSearchSkipped `json:"skipped_documents"`
	Evidence          string                     `json:"evidence"`
}

func (f *documentReadFixture) search(c documentCaller, query string, extra ...any) documentSearchResult {
	f.t.Helper()
	args := map[string]any{"workspace_id": governanceWorkspace, "query": query}
	for i := 0; i+1 < len(extra); i += 2 {
		args[extra[i].(string)] = extra[i+1]
	}
	text, isErr := f.call(c.token, "search_documents", c.args(args))
	if isErr {
		f.t.Fatalf("%s search %q: %s", c.name, query, text)
	}
	var result documentSearchResult
	if err := json.Unmarshal([]byte(text), &result); err != nil {
		f.t.Fatal(err)
	}
	return result
}

func searchMatchDocuments(result documentSearchResult) []string {
	documents := []string{}
	for _, item := range result.Items {
		documents = append(documents, item["document_id"].(string))
	}
	return documents
}

func (f *documentReadFixture) requirement(id, body string, confirm bool) {
	f.t.Helper()
	_, version, err := f.store.CreateRequirement(f.ctx, core.Requirement{ID: id, Title: id}, core.RequirementVersion{Content: "# " + id + "\n" + body, Origin: core.RequirementOriginOperator, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Hold."}}})
	if err != nil {
		f.t.Fatal(err)
	}
	if confirm {
		if _, _, err = f.store.ConfirmRequirementVersion(f.ctx, id, version.Version); err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f *documentReadFixture) reference(id, body string) {
	f.t.Helper()
	if _, _, err := f.store.CreateReferenceDocument(f.ctx, core.ReferenceDocument{ID: id, Name: id}, core.ReferenceDocumentVersion{Content: body, Filename: id + ".md", ContentType: "text/markdown"}); err != nil {
		f.t.Fatal(err)
	}
}

func TestMCPDocumentSearchReturnsCurrentMatchesWithSections(t *testing.T) {
	f := newDocumentReadFixture(t)
	f.design("design-a", designBody("intro needle one\n## First\nthe needle two\nno match\n### Nested\nNEEDLE three\n"), designBody("preamble\n## Second\ncurrent Needle only\n"))
	pendingVersion := f.proposeDesignVersion("design-a", designBody("## Third\nneedle in a proposal\n"))
	f.design("design-b", designBody("## Other\nanother needle\n"))
	f.requirement("req-a", "## Part\nrequirement needle\n", true)
	f.requirement("req-unconfirmed", "## Part\nneedle in an unconfirmed requirement\n", false)
	f.reference("ref-a", "## Notes\nreference needle\n")
	for _, caller := range f.callers() {
		t.Run(caller.name, func(t *testing.T) {
			// AC-2.1: fields, current versions only, deterministic order.
			result := f.search(caller, "needle")
			if got, want := searchMatchDocuments(result), []string{"design-a", "design-b", "ref-a", "req-a"}; !slices.Equal(got, want) {
				t.Fatalf("documents=%v, want %v", got, want)
			}
			if result.Truncated || result.Total != 4 || result.DocumentsSearched != 4 || result.Advice != "" {
				t.Fatalf("result=%+v", result)
			}
			match := result.Items[0]
			for field, want := range map[string]any{
				"document_id": "design-a", "kind": "system_design", "version": float64(2), "section_id": "s1", "heading": "Second",
				"line": "current Needle only", "section_size_bytes": float64(len(designBody("## Second\ncurrent Needle only\n"))),
				"selection": "current", "version_status": "confirmed", "active_authority": true, "informative": false, "historical": false, "archived": false,
			} {
				if match[field] != want {
					t.Fatalf("%s=%v, want %v in %v", field, match[field], want, match)
				}
			}
			reference := result.Items[2]
			if reference["kind"] != "reference" || reference["informative"] != true || reference["active_authority"] != false || reference["version_status"] != "informative" {
				t.Fatalf("reference labels=%v", reference)
			}
			// AC-4.2 and AC-2.1: a named version is searched instead.
			historical := f.search(caller, "needle", "version", 1, "document_ids", []string{"design-a"})
			if historical.Total != 3 {
				t.Fatalf("historical=%+v", historical)
			}
			for i, want := range []struct{ section, heading, line string }{
				{"s0", "", "intro needle one"}, {"s1", "First", "the needle two"}, {"s2", "Nested", "NEEDLE three"},
			} {
				item := historical.Items[i]
				if item["section_id"] != want.section || item["heading"] != want.heading || item["line"] != want.line || item["version"] != float64(1) || item["historical"] != true || item["active_authority"] != false || item["selection"] != "explicit_version" {
					t.Fatalf("historical match %d=%v", i, item)
				}
			}
			proposed := f.search(caller, "proposal", "version", pendingVersion, "kind", "system_design")
			if proposed.Total != 1 || proposed.Items[0]["version_status"] != "proposed" || proposed.Items[0]["active_authority"] != false {
				t.Fatalf("proposed=%+v", proposed)
			}
			unconfirmed := f.search(caller, "in an unconfirmed", "version", 1, "kind", "requirement")
			if unconfirmed.Total != 1 || unconfirmed.Items[0]["active_authority"] != false {
				t.Fatalf("unconfirmed=%+v", unconfirmed)
			}
		})
	}
}

func TestMCPDocumentSearchLimitsToNamedDocumentsAndKinds(t *testing.T) {
	f := newDocumentReadFixture(t)
	f.design("design-a", designBody("## A\nneedle\n"))
	f.design("design-b", designBody("## B\nneedle\n"))
	f.requirement("req-a", "## R\nneedle\n", true)
	f.reference("ref-a", "## N\nneedle\n")
	f.design("design-archived", designBody("## Old\nneedle\n"))
	if err := f.store.ArchiveSystemDesign(f.ctx, "design-archived", "user:operator", nil); err != nil {
		t.Fatal(err)
	}
	f.requirement("req-unconfirmed", "## R\nneedle\n", false)
	for _, caller := range f.callers() {
		t.Run(caller.name, func(t *testing.T) {
			// AC-2.2: named documents, a kind, and their intersection.
			for name, tc := range map[string]struct {
				extra []any
				want  []string
			}{
				"named":              {[]any{"document_ids", []string{"design-b", "ref-a"}}, []string{"design-b", "ref-a"}},
				"kind":               {[]any{"kind", "system_design"}, []string{"design-a", "design-b"}},
				"requirement kind":   {[]any{"kind", "requirement"}, []string{"req-a"}},
				"reference kind":     {[]any{"kind", "reference"}, []string{"ref-a"}},
				"intersection":       {[]any{"kind", "system_design", "document_ids", []string{"design-a", "req-a"}}, []string{"design-a"}},
				"archived included":  {[]any{"kind", "system_design", "include_archived", true}, []string{"design-a", "design-archived", "design-b"}},
				"archived named":     {[]any{"document_ids", []string{"design-archived"}, "include_archived", true}, []string{"design-archived"}},
				"duplicate IDs once": {[]any{"document_ids", []string{"design-a", "design-a"}}, []string{"design-a"}},
			} {
				result := f.search(caller, "needle", tc.extra...)
				if got := searchMatchDocuments(result); !slices.Equal(got, tc.want) {
					t.Fatalf("%s: documents=%v, want %v", name, got, tc.want)
				}
			}
			archived := f.search(caller, "needle", "include_archived", true, "document_ids", []string{"design-archived"})
			if archived.Items[0]["archived"] != true || archived.Items[0]["active_authority"] != false {
				t.Fatalf("archived labels=%v", archived.Items[0])
			}
			// Empty results are explicit and say why a named document was skipped.
			skipped := f.search(caller, "needle", "document_ids", []string{"design-archived", "design-absent", "req-unconfirmed"})
			if skipped.Total != 0 || skipped.Truncated || !strings.Contains(skipped.Advice, "No line matched") || len(skipped.Items) != 0 {
				t.Fatalf("skipped=%+v", skipped)
			}
			want := []mcpDocumentSearchSkipped{
				{DocumentID: "design-absent", Reason: searchSkipNotFound},
				{DocumentID: "design-archived", Kind: "system_design", Reason: searchSkipArchived},
				{DocumentID: "req-unconfirmed", Kind: "requirement", Reason: searchSkipNoConfirmed},
			}
			if !slices.Equal(skipped.SkippedDocuments, want) {
				t.Fatalf("skipped documents=%+v", skipped.SkippedDocuments)
			}
			missing := f.search(caller, "needle", "version", 9, "document_ids", []string{"design-a"})
			if missing.Total != 0 || !slices.Equal(missing.SkippedDocuments, []mcpDocumentSearchSkipped{{DocumentID: "design-a", Kind: "system_design", Reason: searchSkipVersionNotFound}}) {
				t.Fatalf("missing version=%+v", missing)
			}
			if empty := f.search(caller, "nothing-matches-this"); empty.Total != 0 || empty.Advice == "" || empty.SkippedDocuments != nil || empty.DocumentsSearched != 4 {
				t.Fatalf("empty=%+v", empty)
			}
			for name, tc := range map[string]struct {
				args map[string]any
				want string
			}{
				"missing query":    {map[string]any{"workspace_id": governanceWorkspace}, "query is required"},
				"blank query":      {map[string]any{"workspace_id": governanceWorkspace, "query": "   "}, "invalid query"},
				"long query":       {map[string]any{"workspace_id": governanceWorkspace, "query": strings.Repeat("q", 257)}, "invalid query"},
				"string IDs":       {map[string]any{"workspace_id": governanceWorkspace, "query": "q", "document_ids": "design-a"}, "invalid document_ids"},
				"empty IDs":        {map[string]any{"workspace_id": governanceWorkspace, "query": "q", "document_ids": []string{}}, "invalid document_ids"},
				"non-string IDs":   {map[string]any{"workspace_id": governanceWorkspace, "query": "q", "document_ids": []any{7}}, "invalid document_ids"},
				"blank ID":         {map[string]any{"workspace_id": governanceWorkspace, "query": "q", "document_ids": []string{" "}}, "invalid document_ids"},
				"too many IDs":     {map[string]any{"workspace_id": governanceWorkspace, "query": "q", "document_ids": make([]string, mcpDocumentSearchMaxIDs+1)}, "invalid document_ids"},
				"bad kind":         {map[string]any{"workspace_id": governanceWorkspace, "query": "q", "kind": "decision"}, "invalid kind"},
				"bad version":      {map[string]any{"workspace_id": governanceWorkspace, "query": "q", "version": 0}, "invalid version"},
				"no offset":        {map[string]any{"workspace_id": governanceWorkspace, "query": "q", "offset": 1}, "unknown read argument offset"},
				"no snapshot":      {map[string]any{"workspace_id": governanceWorkspace, "query": "q", "snapshot": strings.Repeat("a", 32)}, "unknown read argument snapshot"},
				"over limit":       {map[string]any{"workspace_id": governanceWorkspace, "query": "q", "limit": 101}, "invalid limit"},
				"unknown argument": {map[string]any{"workspace_id": governanceWorkspace, "query": "q", "regex": true}, "unknown read argument regex"},
			} {
				if text, isErr := f.call(caller.token, "search_documents", caller.args(tc.args)); !isErr || !strings.Contains(text, tc.want) {
					t.Fatalf("%s: isError=%v text=%q, want %q", name, isErr, text, tc.want)
				}
			}
		})
	}
}

func TestMCPDocumentSearchMatchesLiteralTextCaseInsensitively(t *testing.T) {
	f := newDocumentReadFixture(t)
	f.design("design-literal", designBody("## Part\nprefix a.c(d)* suffix\nabcd\nCRLF line\r\nÉCOLE Unicode\n## Fence\n```\n## fenced heading text\n```\n"))
	for _, caller := range f.callers() {
		t.Run(caller.name, func(t *testing.T) {
			if result := f.search(caller, "A.C(D)*"); result.Total != 1 || result.Items[0]["line"] != "prefix a.c(d)* suffix" {
				t.Fatalf("literal=%+v", result)
			}
			if result := f.search(caller, "a.c"); result.Total != 1 {
				t.Fatalf("a metacharacter matched as a pattern: %+v", result)
			}
			if result := f.search(caller, "école"); result.Total != 1 || result.Items[0]["line"] != "ÉCOLE Unicode" {
				t.Fatalf("unicode case=%+v", result)
			}
			if result := f.search(caller, "crlf line"); result.Total != 1 || result.Items[0]["line"] != "CRLF line" {
				t.Fatalf("line ending=%+v", result)
			}
			if result := f.search(caller, "fenced heading"); result.Total != 1 || result.Items[0]["heading"] != "Fence" || result.Items[0]["section_id"] != "s1" {
				t.Fatalf("fenced text=%+v", result)
			}
			if result := f.search(caller, "suffix\nabcd"); result.Total != 0 {
				t.Fatalf("a query spanned lines: %+v", result)
			}
		})
	}
}

// TestMCPDocumentSearchStatesTruncation covers AC-2.3 at both stops, and
// oversized lines reported separately from truncation.
func TestMCPDocumentSearchStatesTruncation(t *testing.T) {
	f := newDocumentReadFixture(t)
	var lines strings.Builder
	for i := range 120 {
		fmt.Fprintf(&lines, "needle line %03d\n", i)
	}
	f.design("design-count", designBody("## Lines\n"+lines.String()))
	// Long matching lines escape heavily, so the byte budget stops first.
	var long strings.Builder
	for range 100 {
		long.WriteString(strings.Repeat("\x01", 2000) + " bytes-needle\n")
	}
	f.design("design-bytes", designBody("## Long\n"+long.String()))
	// One line far over the limit: the K sign shrinks when lowered, so the
	// match position must be mapped back to the original line.
	f.design("design-excerpt", designBody("## Huge\n"+strings.Repeat("K", 40000)+" excerpt-needle "+strings.Repeat("y", 200000)+"\n"))
	f.design("design-exact", designBody("## Five\nexact-needle\nexact-needle\nexact-needle\n"))
	for _, caller := range f.callers() {
		t.Run(caller.name, func(t *testing.T) {
			first := f.search(caller, "needle line", "document_ids", []string{"design-count"})
			if first.Total != 25 || first.Limit != 25 || !first.Truncated || first.TruncatedBy != "limit" {
				t.Fatalf("limit=%+v", first)
			}
			for _, hint := range []string{"document_ids", "kind", "version", "query", "limit"} {
				if !strings.Contains(first.Advice, hint) {
					t.Fatalf("advice %q lacks %s", first.Advice, hint)
				}
			}
			for i, item := range first.Items {
				if item["line"] != fmt.Sprintf("needle line %03d", i) {
					t.Fatalf("order at %d: %v", i, item)
				}
			}
			if most := f.search(caller, "needle line", "limit", 100); most.Total != 100 || !most.Truncated {
				t.Fatalf("limit 100=%+v", most)
			}
			// A response holding every match is not truncated.
			exact := f.search(caller, "exact-needle", "limit", 3)
			if exact.Total != 3 || exact.Truncated || exact.Advice != "" {
				t.Fatalf("exact=%+v", exact)
			}
			bytes := f.search(caller, "bytes-needle", "limit", 100)
			if !bytes.Truncated || bytes.TruncatedBy != "output_bytes" || bytes.Total == 0 || bytes.Total >= 100 || bytes.Advice == "" {
				t.Fatalf("bytes: total=%d truncated=%v by=%q", bytes.Total, bytes.Truncated, bytes.TruncatedBy)
			}
			excerpt := f.search(caller, "excerpt-needle")
			if excerpt.Total != 1 || excerpt.Truncated || excerpt.ExcerptsTruncated != 1 {
				t.Fatalf("excerpt: total=%d truncated=%v excerpts=%d", excerpt.Total, excerpt.Truncated, excerpt.ExcerptsTruncated)
			}
			item := excerpt.Items[0]
			line := item["line"].(string)
			if !strings.Contains(line, "excerpt-needle") || len(line) > 2*mcpDocumentSearchExcerptContext+len("excerpt-needle")+8 || item["line_truncated"] != true || item["line_bytes"] != float64(3*40000+len(" excerpt-needle ")+200000) {
				t.Fatalf("excerpt of %d bytes: truncated=%v line_bytes=%v", len(line), item["line_truncated"], item["line_bytes"])
			}
			if whole := f.search(caller, "exact-needle", "limit", 1); whole.ExcerptsTruncated != 0 || whole.Items[0]["line_truncated"] != nil {
				t.Fatalf("short line labeled: %+v", whole)
			}
		})
	}
}

func TestMCPDocumentSearchCannotProbeRedactedSecrets(t *testing.T) {
	f := newDocumentReadFixture(t)
	f.server.WorkOrders.RedactionSecrets = mcpReadSecretFixture{}
	secret := "private-value-for-test"
	f.design("design-secret", designBody("## Heading "+secret+"\nvisible needle "+secret+" end\nsecond line\n"))
	for _, caller := range f.callers() {
		t.Run(caller.name, func(t *testing.T) {
			if probe := f.search(caller, secret); probe.Total != 0 {
				t.Fatalf("query matched a redacted secret: %+v", probe)
			}
			result := f.search(caller, "visible needle")
			if result.Total != 1 {
				t.Fatalf("result=%+v", result)
			}
			data, _ := json.Marshal(result)
			if strings.Contains(string(data), secret) || !strings.Contains(result.Items[0]["line"].(string), "REDACTED") || !strings.Contains(result.Items[0]["heading"].(string), "REDACTED") {
				t.Fatalf("secret leaked: %s", data)
			}
		})
	}
}

// documentReadCounter counts the store reads that carry a document body.
type documentReadCounter struct {
	store.Store
	reads  atomic.Int64
	onRead func(id string)
}

func (c *documentReadCounter) note(id string) {
	c.reads.Add(1)
	if c.onRead != nil {
		c.onRead(id)
	}
}

func (c *documentReadCounter) GetRequirementVersion(ctx context.Context, id string, version int) (core.RequirementVersion, error) {
	c.note(id)
	return c.Store.GetRequirementVersion(ctx, id, version)
}

func (c *documentReadCounter) GetSystemDesignVersion(ctx context.Context, id string, version int) (core.SystemDesignVersion, error) {
	c.note(id)
	return c.Store.GetSystemDesignVersion(ctx, id, version)
}

func (c *documentReadCounter) GetReferenceDocumentVersion(ctx context.Context, id string, version int) (core.ReferenceDocumentVersion, error) {
	c.note(id)
	return c.Store.GetReferenceDocumentVersion(ctx, id, version)
}

func (c *documentReadCounter) ListRequirementVersions(ctx context.Context, id string) ([]core.RequirementVersion, error) {
	c.note(id)
	return c.Store.ListRequirementVersions(ctx, id)
}

func (c *documentReadCounter) ListSystemDesignVersions(ctx context.Context, id string) ([]core.SystemDesignVersion, error) {
	c.note(id)
	return c.Store.ListSystemDesignVersions(ctx, id)
}

func (c *documentReadCounter) ListReferenceDocumentVersions(ctx context.Context, id string) ([]core.ReferenceDocumentVersion, error) {
	c.note(id)
	return c.Store.ListReferenceDocumentVersions(ctx, id)
}

// countDocumentReads installs a counter in front of the server's store. The
// fixture keeps the raw store, so its setup calls are never counted.
func (f *documentReadFixture) countDocumentReads() *documentReadCounter {
	counter := &documentReadCounter{Store: f.server.Store}
	f.server.Store = counter
	return counter
}

func (f *documentReadFixture) corpus(count int) {
	f.t.Helper()
	for i := range count {
		f.design(fmt.Sprintf("design-%02d", i), designBody(fmt.Sprintf("## S\nneedle %d\n", i)))
	}
	f.requirement("req-corpus", "## R\nneedle r\n", true)
	f.reference("ref-corpus", "## N\nneedle n\n")
}

// TestMCPDocumentSearchDoesNotRereadResidentBodies is the read-count exit check:
// a repeated search reads no document body, under either credential.
func TestMCPDocumentSearchDoesNotRereadResidentBodies(t *testing.T) {
	f := newDocumentReadFixture(t)
	f.corpus(10)
	const documents = 12
	counter := f.countDocumentReads()
	for _, caller := range f.callers() {
		t.Run(caller.name, func(t *testing.T) {
			before := counter.reads.Load()
			for range 3 {
				if result := f.search(caller, "needle", "limit", 100); result.Total != documents {
					t.Fatalf("result=%+v", result)
				}
			}
			reads := counter.reads.Load() - before
			if caller.claim == nil && reads != documents {
				t.Fatalf("cold plus warm searches read %d bodies, want one each of %d", reads, documents)
			}
			if caller.claim != nil && reads != 0 {
				t.Fatalf("warm searches read %d bodies", reads)
			}
			// Outlines and section pages of a resident current version read none either.
			before = counter.reads.Load()
			f.readItem(caller, "get_document", documentReadArgs("design-00", "outline", true))
			f.readItem(caller, "get_document", documentReadArgs("design-00", "section_id", "s0"))
			if got := counter.reads.Load() - before; got != 0 {
				t.Fatalf("resident partial reads read %d bodies", got)
			}
			// A settled named version stays resident too.
			before = counter.reads.Load()
			named := []string{"design-00", "design-01", "req-corpus", "ref-corpus"}
			for range 2 {
				if result := f.search(caller, "needle", "version", 1, "document_ids", named); result.Total != len(named) {
					t.Fatalf("named-version search=%+v", result)
				}
			}
			if got := counter.reads.Load() - before; got != 0 {
				t.Fatalf("named-version searches read %d bodies", got)
			}
		})
	}
}

func TestMCPDocumentSearchDeduplicatesConcurrentMisses(t *testing.T) {
	f := newDocumentReadFixture(t)
	f.corpus(4)
	counter := f.countDocumentReads()
	entered := make(chan struct{}, 64)
	release := make(chan struct{})
	counter.onRead = func(string) {
		entered <- struct{}{}
		<-release
	}
	callers := f.callers()
	const searches = 16
	var group sync.WaitGroup
	errs := make(chan error, searches)
	args := func(i int) (string, map[string]any) {
		caller := callers[i%len(callers)]
		return caller.token, caller.args(map[string]any{"workspace_id": governanceWorkspace, "query": "needle", "document_ids": []string{"design-00"}})
	}
	for i := range searches {
		group.Add(1)
		go func() {
			defer group.Done()
			token, payload := args(i)
			text, isErr := f.call(token, "search_documents", payload)
			if isErr {
				errs <- fmt.Errorf("%s", text)
			}
		}()
	}
	<-entered
	// The first load is held: every other search has joined it or is about to.
	waitForFlightWaiters(t, f.server, 1)
	close(release)
	group.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if got := counter.reads.Load(); got != 1 {
		t.Fatalf("concurrent misses read %d bodies, want 1", got)
	}
}

func waitForFlightWaiters(t *testing.T, s *Server, want int) {
	t.Helper()
	s.documentVersions.mu.Lock()
	defer s.documentVersions.mu.Unlock()
	if len(s.documentVersions.flights) != want {
		t.Fatalf("flights=%d, want %d", len(s.documentVersions.flights), want)
	}
}

func TestMCPDocumentCacheFailedLoadIsNotRetained(t *testing.T) {
	var cache mcpDocumentCache
	key := mcpDocumentVersionKey{workspace: "w", kind: "system_design", id: "d", version: 1}
	if _, _, err := cache.load(t.Context(), key, func() (*parsedDocumentVersion, error) { return nil, store.ErrNotFound }); err != store.ErrNotFound {
		t.Fatalf("err=%v", err)
	}
	if len(cache.entries) != 0 || len(cache.flights) != 0 {
		t.Fatalf("failed load retained: entries=%d flights=%d", len(cache.entries), len(cache.flights))
	}
	parsed := newParsedDocumentVersion("## A\nbody", documentVersionStatus{confirmed: true}, true)
	calls := 0
	for range 2 {
		got, _, err := cache.load(t.Context(), key, func() (*parsedDocumentVersion, error) { calls++; return parsed, nil })
		if err != nil || got != parsed {
			t.Fatalf("got=%v err=%v", got, err)
		}
	}
	if calls != 1 {
		t.Fatalf("loader ran %d times", calls)
	}
}

// A joined load whose leader's request was cancelled retries under the joining
// caller's own live context instead of inheriting the cancellation.
func TestMCPDocumentCacheJoinerSurvivesCancelledLeader(t *testing.T) {
	var cache mcpDocumentCache
	key := mcpDocumentVersionKey{workspace: "w", kind: "system_design", id: "d", version: 1}
	started, release := make(chan struct{}), make(chan struct{})
	leader := make(chan error, 1)
	go func() {
		_, _, err := cache.load(t.Context(), key, func() (*parsedDocumentVersion, error) {
			close(started)
			<-release
			return nil, context.Canceled
		})
		leader <- err
	}()
	<-started
	parsed := newParsedDocumentVersion("## A", documentVersionStatus{}, true)
	joiner := make(chan *parsedDocumentVersion, 1)
	go func() {
		got, _, err := cache.load(t.Context(), key, func() (*parsedDocumentVersion, error) { return parsed, nil })
		if err != nil {
			t.Error(err)
		}
		joiner <- got
	}()
	close(release)
	if err := <-leader; err != context.Canceled {
		t.Fatalf("leader err=%v", err)
	}
	if got := <-joiner; got != parsed {
		t.Fatalf("joiner=%v", got)
	}
}

func TestMCPDocumentCacheBoundsAndEviction(t *testing.T) {
	parsed := func(size int) *parsedDocumentVersion {
		return newParsedDocumentVersion("## A\n"+strings.Repeat("x", size), documentVersionStatus{confirmed: true}, true)
	}
	key := func(i int) mcpDocumentVersionKey {
		return mcpDocumentVersionKey{workspace: "w", kind: "system_design", id: fmt.Sprint("d", i), version: 1}
	}
	load := func(t *testing.T, cache *mcpDocumentCache, k mcpDocumentVersionKey, value *parsedDocumentVersion) bool {
		t.Helper()
		loaded := false
		if _, _, err := cache.load(t.Context(), k, func() (*parsedDocumentVersion, error) { loaded = true; return value, nil }); err != nil {
			t.Fatal(err)
		}
		return loaded
	}
	t.Run("defaults", func(t *testing.T) {
		maxBytes, maxEntries := (&mcpDocumentCache{}).limits()
		if maxBytes != 16<<20 || maxEntries != 128 {
			t.Fatalf("bytes=%d entries=%d", maxBytes, maxEntries)
		}
	})
	t.Run("entries", func(t *testing.T) {
		cache := &mcpDocumentCache{maxEntries: 3}
		for i := range 3 {
			load(t, cache, key(i), parsed(1))
		}
		// d0 becomes most recently used, so d1 is the eviction candidate.
		load(t, cache, key(0), parsed(1))
		load(t, cache, key(3), parsed(1))
		if len(cache.entries) != 3 {
			t.Fatalf("entries=%d", len(cache.entries))
		}
		if load(t, cache, key(0), parsed(1)) {
			t.Fatal("recently used entry evicted")
		}
		if !load(t, cache, key(1), parsed(1)) {
			t.Fatal("least recently used entry retained")
		}
	})
	t.Run("bytes count derived text", func(t *testing.T) {
		one := parsed(1000)
		if one.cost < 2*int64(len(one.content)) {
			t.Fatalf("cost %d excludes derived text of %d bytes", one.cost, len(one.content))
		}
		cache := &mcpDocumentCache{maxBytes: 2*one.cost + 1}
		for i := range 5 {
			load(t, cache, key(i), parsed(1000))
			if cache.bytes > cache.maxBytes {
				t.Fatalf("bytes %d over bound %d", cache.bytes, cache.maxBytes)
			}
		}
		if len(cache.entries) != 2 || cache.bytes != 2*one.cost {
			t.Fatalf("entries=%d bytes=%d", len(cache.entries), cache.bytes)
		}
	})
	t.Run("oversized entry is served but not retained", func(t *testing.T) {
		cache := &mcpDocumentCache{maxBytes: 100}
		value := parsed(1000)
		got, fresh, err := cache.load(t.Context(), key(0), func() (*parsedDocumentVersion, error) { return value, nil })
		if err != nil || got != value || !fresh {
			t.Fatalf("got=%v fresh=%v err=%v", got, fresh, err)
		}
		if len(cache.entries) != 0 || cache.bytes != 0 {
			t.Fatalf("entries=%d bytes=%d", len(cache.entries), cache.bytes)
		}
	})
	t.Run("many tiny sections are charged", func(t *testing.T) {
		tiny := newParsedDocumentVersion(strings.Repeat("## A\n", 100), documentVersionStatus{}, true)
		if tiny.cost < 100*mcpDocumentCacheOverhead {
			t.Fatalf("cost=%d", tiny.cost)
		}
	})
}

// An evicted version is read once more and then stays resident.
func TestMCPDocumentSearchRereadsEvictedVersionsOnly(t *testing.T) {
	f := newDocumentReadFixture(t)
	f.corpus(3)
	counter := f.countDocumentReads()
	f.server.documentVersions.maxEntries = 100
	caller := f.callers()[1]
	f.search(caller, "needle", "limit", 100)
	base := counter.reads.Load()
	f.server.documentVersions.mu.Lock()
	f.server.documentVersions.removeLocked(f.server.documentVersions.order.Back())
	f.server.documentVersions.mu.Unlock()
	f.search(caller, "needle", "limit", 100)
	if got := counter.reads.Load() - base; got != 1 {
		t.Fatalf("eviction reread %d bodies, want 1", got)
	}
	base = counter.reads.Load()
	f.search(caller, "needle", "limit", 100)
	if got := counter.reads.Load() - base; got != 0 {
		t.Fatalf("resident search read %d bodies", got)
	}
}

// TestMCPDocumentCacheServesNewVersionsAndChangedState proves a resident body
// never carries stale selection, archive, or confirmation state.
func TestMCPDocumentCacheServesNewVersionsAndChangedState(t *testing.T) {
	f := newDocumentReadFixture(t)
	f.design("design-state", designBody("## V1\nstate-needle one\n"))
	counter := f.countDocumentReads()
	caller := f.callers()[1]
	if result := f.search(caller, "state-needle"); result.Total != 1 || result.Items[0]["version"] != float64(1) {
		t.Fatalf("v1=%+v", result)
	}

	t.Run("a new confirmed version replaces the current one", func(t *testing.T) {
		v2 := f.confirmDesignVersion("design-state", designBody("## V2\nstate-needle two\n"))
		base := counter.reads.Load()
		result := f.search(caller, "state-needle")
		if result.Total != 1 || result.Items[0]["version"] != float64(v2) || result.Items[0]["line"] != "state-needle two" || result.Items[0]["active_authority"] != true {
			t.Fatalf("v2=%+v", result)
		}
		if got := counter.reads.Load() - base; got != 1 {
			t.Fatalf("new version read %d bodies, want 1", got)
		}
		// The superseded version stays resident with its labels recomputed.
		old := f.search(caller, "state-needle", "version", 1)
		if old.Total != 1 || old.Items[0]["historical"] != true || old.Items[0]["active_authority"] != false || old.Items[0]["version_status"] != "confirmed" {
			t.Fatalf("v1 now=%+v", old)
		}
	})

	t.Run("a proposal's status is reread until it settles", func(t *testing.T) {
		version := f.proposeDesignVersion("design-state", designBody("## V3\nstate-needle three\n"))
		proposed := f.search(caller, "state-needle", "version", version)
		if proposed.Items[0]["version_status"] != "proposed" || proposed.Items[0]["active_authority"] != false {
			t.Fatalf("proposed=%+v", proposed)
		}
		section := f.readItem(caller, "get_document", documentReadArgs("design-state", "section_id", "s0", "version", version))
		if section["version_status"] != "proposed" {
			t.Fatalf("proposed section=%v", section)
		}
		// Pending versions reread their row, and only their row.
		base := counter.reads.Load()
		f.search(caller, "state-needle", "version", version, "document_ids", []string{"design-state"})
		if got := counter.reads.Load() - base; got != 1 {
			t.Fatalf("pending search read %d rows, want 1", got)
		}
		if _, _, err := f.store.DismissSystemDesignVersion(f.ctx, "design-state", version); err != nil {
			t.Fatal(err)
		}
		dismissed := f.search(caller, "state-needle", "version", version)
		if dismissed.Items[0]["version_status"] != "retired" || dismissed.Items[0]["active_authority"] != false {
			t.Fatalf("dismissed=%+v", dismissed)
		}
		base = counter.reads.Load()
		f.search(caller, "state-needle", "version", version, "document_ids", []string{"design-state"})
		if got := counter.reads.Load() - base; got != 0 {
			t.Fatalf("settled version read %d rows", got)
		}
		confirmed := f.proposeDesignVersion("design-state", designBody("## V4\nstate-needle four\n"))
		if result := f.search(caller, "state-needle", "version", confirmed); result.Items[0]["version_status"] != "proposed" {
			t.Fatalf("v4=%+v", result)
		}
		if _, _, err := f.store.ConfirmSystemDesignVersion(f.ctx, "design-state", confirmed); err != nil {
			t.Fatal(err)
		}
		result := f.search(caller, "state-needle", "version", confirmed)
		if result.Items[0]["version_status"] != "confirmed" || result.Items[0]["active_authority"] != true || result.Items[0]["historical"] != false {
			t.Fatalf("confirmed v4=%+v", result)
		}
	})

	t.Run("archiving refuses a resident body until it is requested", func(t *testing.T) {
		if err := f.store.ArchiveSystemDesign(f.ctx, "design-state", "user:operator", nil); err != nil {
			t.Fatal(err)
		}
		if result := f.search(caller, "state-needle"); result.Total != 0 {
			t.Fatalf("archived document searched: %+v", result)
		}
		if text, isErr := f.call(caller.token, "get_document", caller.args(documentReadArgs("design-state", "section_id", "s0"))); !isErr || !strings.Contains(text, "include_archived") {
			t.Fatalf("archived section: %s", text)
		}
		archived := f.search(caller, "state-needle", "include_archived", true)
		if archived.Total != 1 || archived.Items[0]["archived"] != true || archived.Items[0]["active_authority"] != false {
			t.Fatalf("archived=%+v", archived)
		}
		if err := f.store.RestoreSystemDesign(f.ctx, "design-state", "user:operator"); err != nil {
			t.Fatal(err)
		}
		if restored := f.search(caller, "state-needle"); restored.Total != 1 || restored.Items[0]["active_authority"] != true {
			t.Fatalf("restored=%+v", restored)
		}
	})
}

// The cache key carries the workspace, so one workspace's resident body never
// answers for a same-named document in another.
func TestMCPDocumentCacheIsolatesWorkspaces(t *testing.T) {
	f := newDocumentReadFixture(t)
	f.design("design-shared", designBody("## Demo\nworkspace-needle demo\n"))
	beta := store.WithWorkspace(f.ctx, "beta")
	_, first, err := f.store.CreateSystemDesign(beta, core.SystemDesign{ID: "design-shared", Title: "shared", Category: "Component"}, core.SystemDesignVersion{Content: designBody("## Beta\nworkspace-needle beta\n"), Origin: core.SystemDesignOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = f.store.ConfirmSystemDesignVersion(beta, "design-shared", first.Version); err != nil {
		t.Fatal(err)
	}
	counter := f.countDocumentReads()
	userToken := f.user("usr_both", core.WorkspaceRoleViewer)
	f.members.setRole("usr_both", "beta", core.WorkspaceRoleViewer)
	userOnly := documentCaller{name: "both", token: userToken}
	for workspace, want := range map[string]string{governanceWorkspace: "workspace-needle demo", "beta": "workspace-needle beta"} {
		text, isErr := f.call(userOnly.token, "search_documents", map[string]any{"workspace_id": workspace, "query": "workspace-needle"})
		if isErr || !strings.Contains(text, want) {
			t.Fatalf("%s: isError=%v response=%s", workspace, isErr, text)
		}
	}
	if got := counter.reads.Load(); got != 2 {
		t.Fatalf("two workspaces read %d bodies, want 2", got)
	}
	// A credential scoped to one workspace cannot reach the other's residents.
	otherUser := f.user("usr_demo_only", core.WorkspaceRoleContributor)
	orderID := f.order("demo-only", core.StageImplement)
	f.claimRun(orderID, "usr_demo_only", "demo-only-session")
	text, isErr := f.call(otherUser, "search_documents", map[string]any{"workspace_id": "beta", "query": "workspace-needle", "work_order_id": orderID, "session_id": "demo-only-session"})
	if !isErr || !strings.Contains(text, "workspace_not_found") {
		t.Fatalf("cross-workspace read: isError=%v response=%s", isErr, text)
	}
}

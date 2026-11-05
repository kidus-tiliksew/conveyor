package httpapi

// Parser and real-server MCP coverage for outline and section reads
// (req-design-delivery AC-3.1, AC-4.1 to AC-4.4; DEC-103). Every MCP call goes
// through an HTTP server, under a user credential and under an agent credential
// holding a live claim.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func TestParseDocumentSections(t *testing.T) {
	type want struct {
		id, heading string
		level       int
		text        string
	}
	for name, tc := range map[string]struct {
		content string
		want    []want
	}{
		"empty": {content: "", want: []want{}},
		"heading free": {content: "plain text\nmore\n", want: []want{
			{"s0", "", 0, "plain text\nmore\n"},
		}},
		"preamble and nested": {content: "# Title\nintro\n## One\nbody\n### Two\ndeep\n#### Four\nstill two\n## Three\n", want: []want{
			{"s0", "", 0, "# Title\nintro\n"},
			{"s1", "One", 2, "## One\nbody\n"},
			{"s2", "Two", 3, "### Two\ndeep\n#### Four\nstill two\n"},
			{"s3", "Three", 2, "## Three\n"},
		}},
		"first line heading": {content: "## Only\nbody", want: []want{
			{"s0", "Only", 2, "## Only\nbody"},
		}},
		"whitespace preamble is kept": {content: "\n\n## A\n", want: []want{
			{"s0", "", 0, "\n\n"},
			{"s1", "A", 2, "## A\n"},
		}},
		"crlf": {content: "intro\r\n## One\r\nbody\r\n### Two ##\r\n", want: []want{
			{"s0", "", 0, "intro\r\n"},
			{"s1", "One", 2, "## One\r\nbody\r\n"},
			{"s2", "Two", 3, "### Two ##\r\n"},
		}},
		"closing markers": {content: "## A ##\n## B#\n## C \\#\n## ##\n##\n###   D   ###   \n", want: []want{
			{"s0", "A", 2, "## A ##\n"},
			{"s1", "B#", 2, "## B#\n"},
			{"s2", "C \\#", 2, "## C \\#\n"},
			{"s3", "", 2, "## ##\n"},
			{"s4", "", 2, "##\n"},
			{"s5", "D", 3, "###   D   ###   \n"},
		}},
		"duplicate headings": {content: "## Same\none\n## Same\ntwo\n", want: []want{
			{"s0", "Same", 2, "## Same\none\n"},
			{"s1", "Same", 2, "## Same\ntwo\n"},
		}},
		"not headings": {content: "##nospace\n####### seven\n    ## indented\n\t## tabbed\n\\## escaped\n", want: []want{
			{"s0", "", 0, "##nospace\n####### seven\n    ## indented\n\t## tabbed\n\\## escaped\n"},
		}},
		"indented heading": {content: "x\n   ## Three spaces\n", want: []want{
			{"s0", "", 0, "x\n"},
			{"s1", "Three spaces", 2, "   ## Three spaces\n"},
		}},
		"backtick fence": {content: "## A\n```go\n## fenced\n```\n## B\n", want: []want{
			{"s0", "A", 2, "## A\n```go\n## fenced\n```\n"},
			{"s1", "B", 2, "## B\n"},
		}},
		"tilde fence closes only with tildes": {content: "~~~\n## one\n```\n## two\n~~~\n## C\n", want: []want{
			{"s0", "", 0, "~~~\n## one\n```\n## two\n~~~\n"},
			{"s1", "C", 2, "## C\n"},
		}},
		"longer fence needs an equal close": {content: "````\n```\n## inner\n````\n## D\n", want: []want{
			{"s0", "", 0, "````\n```\n## inner\n````\n"},
			{"s1", "D", 2, "## D\n"},
		}},
		"closing fence takes no info string": {content: "```\n``` info\n## inside\n```\n## E\n", want: []want{
			{"s0", "", 0, "```\n``` info\n## inside\n```\n"},
			{"s1", "E", 2, "## E\n"},
		}},
		"backtick in info string is not a fence": {content: "``` a`b\n## F\n", want: []want{
			{"s0", "", 0, "``` a`b\n"},
			{"s1", "F", 2, "## F\n"},
		}},
		"unclosed fence": {content: "## A\n```\n## inside\nrest", want: []want{
			{"s0", "A", 2, "## A\n```\n## inside\nrest"},
		}},
		"crlf fence": {content: "```\r\n## inside\r\n```\r\n## G\r\n", want: []want{
			{"s0", "", 0, "```\r\n## inside\r\n```\r\n"},
			{"s1", "G", 2, "## G\r\n"},
		}},
	} {
		t.Run(name, func(t *testing.T) {
			sections := parseDocumentSections(tc.content)
			got := []want{}
			joined := ""
			for _, section := range sections {
				text := tc.content[section.Start:section.End]
				got = append(got, want{section.ID, section.Heading, section.Level, text})
				joined += text
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("sections = %+v, want %+v", got, tc.want)
			}
			if joined != tc.content {
				t.Fatalf("sections do not reproduce the content: %q", joined)
			}
		})
	}
}

// documentReadFixture serves the governance fixture over a real HTTP server.
type documentReadFixture struct {
	*governanceAuthFixture
	url string
}

func newDocumentReadFixture(t *testing.T) *documentReadFixture {
	t.Helper()
	f := newGovernanceAuthFixture(t)
	server := httptest.NewServer(f.handler)
	t.Cleanup(server.Close)
	return &documentReadFixture{governanceAuthFixture: f, url: server.URL + "/mcp"}
}

// call posts one tools/call over HTTP and fails on any response above the read
// output budget, so every test also checks the bound.
func (f *documentReadFixture) call(token, tool string, args map[string]any) (string, bool) {
	f.t.Helper()
	payload, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": tool, "arguments": args}})
	if err != nil {
		f.t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, f.url, bytes.NewReader(payload))
	if err != nil {
		f.t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		f.t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		f.t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		f.t.Fatalf("%s status=%d body=%s", tool, response.StatusCode, body)
	}
	var envelope struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
		Error *rpcError `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		f.t.Fatal(err)
	}
	if envelope.Error != nil {
		return envelope.Error.Message, true
	}
	if len(envelope.Result.Content) != 1 {
		f.t.Fatalf("%s content=%s", tool, body)
	}
	text := envelope.Result.Content[0].Text
	if !envelope.Result.IsError && len(text) > mcpReadMaxBytes {
		f.t.Fatalf("%s response of %d bytes exceeds the budget", tool, len(text))
	}
	return text, envelope.Result.IsError
}

// documentCaller is one credential mode; claim carries the arguments it needs.
type documentCaller struct {
	name, token string
	claim       map[string]any
}

func (c documentCaller) args(args map[string]any) map[string]any {
	merged := map[string]any{}
	for k, v := range c.claim {
		merged[k] = v
	}
	for k, v := range args {
		merged[k] = v
	}
	return merged
}

// callers returns a person's credential without a claim and with a live claim,
// each of which every acceptance criterion runs under.
func (f *documentReadFixture) callers() []documentCaller {
	f.t.Helper()
	userToken := f.user("usr_docs", core.WorkspaceRoleContributor)
	orderID := f.order("docs", core.StageImplement)
	f.claimRun(orderID, "usr_docs", "docs-session")
	return []documentCaller{
		{name: "user credential", token: userToken},
		{name: "user credential with live claim", token: userToken, claim: map[string]any{"work_order_id": orderID, "session_id": "docs-session"}},
	}
}

func (f *documentReadFixture) readItem(c documentCaller, tool string, args map[string]any) map[string]any {
	f.t.Helper()
	text, isErr := f.call(c.token, tool, c.args(args))
	if isErr {
		f.t.Fatalf("%s %s %v: %s", c.name, tool, args, text)
	}
	var page mcpReadPage
	if err := json.Unmarshal([]byte(text), &page); err != nil {
		f.t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Snapshot != "" || page.NextOffset != nil {
		f.t.Fatalf("partial read page=%s", text)
	}
	var item map[string]any
	if err := json.Unmarshal(page.Items[0], &item); err != nil {
		f.t.Fatal(err)
	}
	return item
}

const documentTestGoverns = "\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/httpapi/**\n```"

// designBody adds the governs fence every stored design requires; it lands in
// the document's last section.
func designBody(body string) string { return body + documentTestGoverns }

// design creates a design and confirms one version per body, oldest first.
func (f *documentReadFixture) design(id string, bodies ...string) {
	f.t.Helper()
	_, first, err := f.store.CreateSystemDesign(f.ctx, core.SystemDesign{ID: id, Title: id, Category: "Component"}, core.SystemDesignVersion{Content: bodies[0], Origin: core.SystemDesignOriginOperator})
	if err != nil {
		f.t.Fatal(err)
	}
	if _, _, err = f.store.ConfirmSystemDesignVersion(f.ctx, id, first.Version); err != nil {
		f.t.Fatal(err)
	}
	for _, body := range bodies[1:] {
		f.confirmDesignVersion(id, body)
	}
}

func (f *documentReadFixture) proposeDesignVersion(id, body string) int {
	f.t.Helper()
	version, err := f.store.ProposeSystemDesignVersion(f.ctx, core.SystemDesignVersion{DocumentID: id, Content: body, Origin: core.SystemDesignOriginOperator})
	if err != nil {
		f.t.Fatal(err)
	}
	return version.Version
}

func (f *documentReadFixture) confirmDesignVersion(id, body string) int {
	f.t.Helper()
	version := f.proposeDesignVersion(id, body)
	if _, _, err := f.store.ConfirmSystemDesignVersion(f.ctx, id, version); err != nil {
		f.t.Fatal(err)
	}
	return version
}

func documentReadArgs(id string, extra ...any) map[string]any {
	args := map[string]any{"workspace_id": governanceWorkspace, "kind": "system_design", "document_id": id}
	for i := 0; i+1 < len(extra); i += 2 {
		args[extra[i].(string)] = extra[i+1]
	}
	return args
}

func outlineEntries(t *testing.T, item map[string]any) []map[string]any {
	t.Helper()
	raw, ok := item["outline"].([]any)
	if !ok {
		t.Fatalf("outline missing: %v", item)
	}
	entries := make([]map[string]any, len(raw))
	for i, value := range raw {
		entries[i] = value.(map[string]any)
	}
	return entries
}

func TestMCPDocumentOutlineReturnsHeadingsWithoutBody(t *testing.T) {
	f := newDocumentReadFixture(t)
	const secretBody = "UNIQUE-BODY-MARKER"
	body := "# Title\n" + secretBody + " intro\n## Alpha\n" + secretBody + " one\n### Beta\n" + secretBody + " two\n## Alpha\n" + secretBody + " three" + documentTestGoverns
	f.design("design-outline", body)
	for _, caller := range f.callers() {
		t.Run(caller.name, func(t *testing.T) {
			text, isErr := f.call(caller.token, "get_document", caller.args(documentReadArgs("design-outline", "outline", true)))
			if isErr {
				t.Fatal(text)
			}
			// AC-3.1: no body text anywhere in the response.
			if strings.Contains(text, secretBody) || strings.Contains(text, "conveyor:governs") {
				t.Fatalf("outline leaked body text: %s", text)
			}
			item := f.readItem(caller, "get_document", documentReadArgs("design-outline", "outline", true))
			for _, field := range []string{"content", "statements", "governs", "text"} {
				if _, present := item[field]; present {
					t.Fatalf("outline carries %s: %v", field, item)
				}
			}
			if item["version"] != float64(1) || item["selection"] != "current" || item["version_status"] != "confirmed" || item["active_authority"] != true || item["complete"] != true || item["truncated"] != false || item["total_sections"] != float64(4) {
				t.Fatalf("outline metadata: %v", item)
			}
			entries := outlineEntries(t, item)
			want := []struct {
				id, heading string
				level       float64
			}{{"s0", "", 0}, {"s1", "Alpha", 2}, {"s2", "Beta", 3}, {"s3", "Alpha", 2}}
			if len(entries) != len(want) {
				t.Fatalf("entries=%v", entries)
			}
			total := 0.0
			for i, entry := range entries {
				if entry["section_id"] != want[i].id || entry["heading"] != want[i].heading || entry["level"] != want[i].level {
					t.Fatalf("entry %d=%v", i, entry)
				}
				size := entry["size_bytes"].(float64)
				section := f.readItem(caller, "get_document", documentReadArgs("design-outline", "section_id", want[i].id, "version", 1))
				if float64(len(section["text"].(string))) != size {
					t.Fatalf("size %v does not match section %v", size, section)
				}
				total += size
			}
			if total != float64(len(body)) {
				t.Fatalf("sizes sum to %v, want %d", total, len(body))
			}
		})
	}
}

func TestMCPDocumentOutlinePagesEveryHeading(t *testing.T) {
	f := newDocumentReadFixture(t)
	var body strings.Builder
	// Quotes and angle brackets expand under JSON escaping.
	const headings = 6000
	for i := range headings {
		fmt.Fprintf(&body, "## H%05d <\"&\">\nb\n", i)
	}
	f.design("design-many", designBody(body.String()))
	// One heading too long for any bound stays labeled instead of dropped.
	f.design("design-long-heading", designBody("## "+strings.Repeat("x", mcpDocumentHeadingMaxBytes+10)+"\nbody\n"))
	for _, caller := range f.callers() {
		t.Run(caller.name, func(t *testing.T) {
			seen := 0
			args := documentReadArgs("design-many", "outline", true)
			pages := 0
			for {
				item := f.readItem(caller, "get_document", args)
				pages++
				for _, entry := range outlineEntries(t, item) {
					if entry["section_id"] != "s"+strconv.Itoa(seen) || entry["heading"] != fmt.Sprintf("H%05d <\"&\">", seen) {
						t.Fatalf("entry %d=%v", seen, entry)
					}
					seen++
				}
				if item["complete"] == true {
					if _, more := item["next_section_offset"]; more || item["truncated"] != false {
						t.Fatalf("final outline page=%v", item)
					}
					break
				}
				if item["truncated"] != true {
					t.Fatalf("page without marker: %v", item)
				}
				args = documentReadArgs("design-many", "outline", true, "version", item["version"], "section_offset", item["next_section_offset"])
			}
			if seen != headings || pages < 2 {
				t.Fatalf("seen=%d pages=%d", seen, pages)
			}
			if text, isErr := f.call(caller.token, "get_document", caller.args(documentReadArgs("design-many", "outline", true, "section_offset", 5))); !isErr || !strings.Contains(text, "section_offset above zero requires version") {
				t.Fatalf("unversioned continuation: %s", text)
			}
			if text, isErr := f.call(caller.token, "get_document", caller.args(documentReadArgs("design-many", "outline", true, "version", 1, "section_offset", headings+1))); !isErr || !strings.Contains(text, "exceeds") {
				t.Fatalf("offset past the end: %s", text)
			}
			long := outlineEntries(t, f.readItem(caller, "get_document", documentReadArgs("design-long-heading", "outline", true)))
			if len(long) != 1 || long[0]["heading_truncated"] != true || len(long[0]["heading"].(string)) != mcpDocumentHeadingMaxBytes {
				t.Fatalf("long heading=%v", long)
			}
			section := f.readItem(caller, "get_document", documentReadArgs("design-long-heading", "section_id", "s0"))
			if !strings.Contains(section["text"].(string), strings.Repeat("x", mcpDocumentHeadingMaxBytes+10)) {
				t.Fatal("section lost its long heading")
			}
		})
	}
}

func TestMCPDocumentSectionReadServesNamedVersion(t *testing.T) {
	f := newDocumentReadFixture(t)
	first := designBody("## One\nversion one text\n## One\nduplicate heading text\n")
	second := designBody("intro\n## Two\nversion two text\n")
	f.design("design-versions", first, second)
	pending := f.proposeDesignVersion("design-versions", designBody("## Pending\nproposed text\n"))
	for _, caller := range f.callers() {
		t.Run(caller.name, func(t *testing.T) {
			// AC-4.1: the section's text and the version it came from.
			current := f.readItem(caller, "get_document", documentReadArgs("design-versions", "section_id", "s1"))
			if current["text"] != designBody("## Two\nversion two text\n") || current["version"] != float64(2) || current["heading"] != "Two" || current["section_id"] != "s1" || current["selection"] != "current" || current["active_authority"] != true || current["complete"] != true || current["truncated"] != false {
				t.Fatalf("current=%v", current)
			}
			// AC-4.2: a named version is served for outline and section reads.
			historical := f.readItem(caller, "get_document", documentReadArgs("design-versions", "section_id", "s1", "version", 1))
			if historical["text"] != designBody("## One\nduplicate heading text\n") || historical["version"] != float64(1) || historical["historical"] != true || historical["active_authority"] != false || historical["version_status"] != "confirmed" || historical["selection"] != "explicit_version" {
				t.Fatalf("historical=%v", historical)
			}
			duplicate := f.readItem(caller, "get_document", documentReadArgs("design-versions", "section_id", "s0", "version", 1))
			if duplicate["text"] != "## One\nversion one text\n" {
				t.Fatalf("duplicate heading=%v", duplicate)
			}
			outline := outlineEntries(t, f.readItem(caller, "get_document", documentReadArgs("design-versions", "outline", true, "version", 1)))
			if len(outline) != 2 || outline[0]["heading"] != "One" || outline[1]["heading"] != "One" {
				t.Fatalf("historical outline=%v", outline)
			}
			// A proposed version keeps its proposed label and confers no authority.
			proposed := f.readItem(caller, "get_document", documentReadArgs("design-versions", "section_id", "s0", "version", pending))
			if proposed["text"] != designBody("## Pending\nproposed text\n") || proposed["version_status"] != "proposed" || proposed["active_authority"] != false {
				t.Fatalf("proposed=%v", proposed)
			}
			// Refusals are actionable and do not fall through to a whole read.
			for name, tc := range map[string]struct {
				args map[string]any
				want string
			}{
				"unknown section":     {documentReadArgs("design-versions", "section_id", "s9"), "outline=true"},
				"both modes":          {documentReadArgs("design-versions", "outline", true, "section_id", "s0"), "mutually exclusive"},
				"offset alone":        {documentReadArgs("design-versions", "byte_offset", 0), "byte_offset requires section_id"},
				"section offset":      {documentReadArgs("design-versions", "section_offset", 0), "section_offset requires outline=true"},
				"unversioned page":    {documentReadArgs("design-versions", "section_id", "s0", "byte_offset", 1), "byte_offset above zero requires version"},
				"past the end":        {documentReadArgs("design-versions", "section_id", "s0", "version", 2, "byte_offset", 1000), "exceeds"},
				"unknown version":     {documentReadArgs("design-versions", "section_id", "s0", "version", 99), "not found"},
				"snapshot protocol":   {documentReadArgs("design-versions", "section_id", "s0", "snapshot", strings.Repeat("a", 32)), "snapshot and offset do not apply"},
				"unknown document":    {documentReadArgs("design-absent", "outline", true), "not found"},
				"negative byte":       {documentReadArgs("design-versions", "section_id", "s0", "byte_offset", -1), "invalid byte_offset"},
				"non-string section":  {documentReadArgs("design-versions", "section_id", 7), "invalid section_id"},
				"non-boolean outline": {documentReadArgs("design-versions", "outline", "true"), "invalid outline"},
			} {
				if text, isErr := f.call(caller.token, "get_document", caller.args(tc.args)); !isErr || !strings.Contains(text, tc.want) {
					t.Fatalf("%s: isError=%v text=%q, want %q", name, isErr, text, tc.want)
				}
			}
		})
	}
}

// largeDocumentBody builds a document over 64 KiB holding one section over the
// response limit. Its text mixes every expansion case: JSON escapes, multi-byte
// runes, HTML-escaped characters, and one very long line.
func largeDocumentBody() string {
	var body strings.Builder
	body.WriteString("preamble with \"quotes\"\n")
	for i := range 40 {
		fmt.Fprintf(&body, "## Small %d\n%s\n", i, strings.Repeat("small section text ", 40))
	}
	body.WriteString("## Oversized\n")
	for i := range 3000 {
		fmt.Fprintf(&body, "line %d \"quoted\" <tag> & back\\slash\ttab é 日本語 🙂 \u2028 %c\n", i, rune(1))
	}
	body.WriteString(strings.Repeat("🙂a\"", 30000) + "\n")
	body.WriteString("### After\nthe end\n")
	return designBody(body.String())
}

// TestMCPDocumentSectionPagesReconstructLargeDocument is the exit check: every
// section of a document larger than 64 KiB reads through section reads, and the
// pages of an oversized section reproduce it (AC-4.3).
func TestMCPDocumentSectionPagesReconstructLargeDocument(t *testing.T) {
	f := newDocumentReadFixture(t)
	body := largeDocumentBody()
	if len(body) <= mcpReadMaxBytes*3 {
		t.Fatalf("fixture of %d bytes is too small", len(body))
	}
	f.design("design-large", body)
	// Reconstruct against the stored text: the store normalizes line endings.
	stored, err := f.store.GetSystemDesignVersion(f.ctx, "design-large", 1)
	if err != nil {
		t.Fatal(err)
	}
	body = stored.Content
	sections := parseDocumentSections(body)
	for _, caller := range f.callers() {
		t.Run(caller.name, func(t *testing.T) {
			outline := outlineEntries(t, f.readItem(caller, "get_document", documentReadArgs("design-large", "outline", true)))
			if len(outline) != len(sections) {
				t.Fatalf("outline holds %d of %d sections", len(outline), len(sections))
			}
			var rebuilt strings.Builder
			multiPage := 0
			for i, entry := range outline {
				id := entry["section_id"].(string)
				args := documentReadArgs("design-large", "section_id", id)
				var text strings.Builder
				pages := 0
				for {
					item := f.readItem(caller, "get_document", args)
					page := item["text"].(string)
					if !utf8.ValidString(page) || item["version"] != float64(1) || item["byte_offset"] != float64(text.Len()) {
						t.Fatalf("section %s page %d metadata: version=%v offset=%v", id, pages, item["version"], item["byte_offset"])
					}
					text.WriteString(page)
					pages++
					if item["complete"] == true {
						if _, more := item["next_byte_offset"]; more || item["truncated"] != false {
							t.Fatalf("final page=%v", item["next_byte_offset"])
						}
						break
					}
					next := item["next_byte_offset"].(float64)
					if item["truncated"] != true || page == "" || int(next) != text.Len() {
						t.Fatalf("section %s page %d did not advance: next=%v have=%d", id, pages, next, text.Len())
					}
					args = documentReadArgs("design-large", "section_id", id, "version", item["version"], "byte_offset", next)
				}
				if float64(text.Len()) != entry["size_bytes"] {
					t.Fatalf("section %s holds %d bytes, outline said %v", id, text.Len(), entry["size_bytes"])
				}
				if text.String() != body[sections[i].Start:sections[i].End] {
					t.Fatalf("section %s pages do not reproduce its text", id)
				}
				if pages > 1 {
					multiPage++
				}
				rebuilt.WriteString(text.String())
			}
			if rebuilt.String() != body {
				t.Fatal("section reads do not reproduce the document")
			}
			if multiPage == 0 {
				t.Fatal("no section needed pages")
			}
			// A byte offset that would split a rune is refused, never corrupted.
			oversized := sections[len(sections)-2]
			text := body[oversized.Start:oversized.End]
			splitAt := strings.Index(text, "é") + 1
			if message, isErr := f.call(caller.token, "get_document", caller.args(documentReadArgs("design-large", "section_id", oversized.ID, "version", 1, "byte_offset", splitAt))); !isErr || !strings.Contains(message, "inside a UTF-8 character") {
				t.Fatalf("split offset: %s", message)
			}
		})
	}
}

// TestMCPDocumentPagesFitWorstCaseEscaping drives the page cutter with text that
// expands six times under JSON escaping and with a single line far over the
// limit, directly against the budget after JSON escaping.
func TestMCPDocumentPagesFitWorstCaseEscaping(t *testing.T) {
	f := newDocumentReadFixture(t)
	caller := f.callers()[1]
	for name, text := range map[string]string{
		"control characters": strings.Repeat("\x01", 70000),
		"html escapes":       strings.Repeat("<&>", 40000),
		"line separators":    strings.Repeat("\u2028", 40000),
		"four-byte runes":    strings.Repeat("🙂", 40000),
		"plain single line":  strings.Repeat("a", 200000),
	} {
		t.Run(name, func(t *testing.T) {
			id := "design-" + strings.ReplaceAll(name, " ", "-")
			body := designBody("## H\n" + text)
			f.design(id, body)
			args := documentReadArgs(id, "section_id", "s0")
			var rebuilt strings.Builder
			for range 1000 {
				// call enforces the serialized budget on every page.
				item := f.readItem(caller, "get_document", args)
				rebuilt.WriteString(item["text"].(string))
				if item["complete"] == true {
					break
				}
				args = documentReadArgs(id, "section_id", "s0", "version", 1, "byte_offset", item["next_byte_offset"])
			}
			if rebuilt.String() != body {
				t.Fatalf("rebuilt %d of %d bytes", rebuilt.Len(), len(body))
			}
		})
	}
}

func TestMCPDocumentPageEndIsExactAtTheBudget(t *testing.T) {
	for _, text := range []string{"abcdef", "éééé", "\"\"\"\"", "a🙂b🙂", "\x01\x01"} {
		for budget := 6; budget < 40; budget++ {
			end, err := documentPageEnd(text, 0, budget)
			if err != nil {
				t.Fatalf("%q budget %d: %v", text, budget, err)
			}
			encoded, _ := json.Marshal(text[:end])
			if len(encoded)-2 > budget || !utf8.ValidString(text[:end]) {
				t.Fatalf("%q budget %d: end=%d encodes to %d", text, budget, end, len(encoded)-2)
			}
			if end < len(text) {
				_, width := utf8.DecodeRuneInString(text[end:])
				next, _ := json.Marshal(text[:end+width])
				if len(next)-2 <= budget {
					t.Fatalf("%q budget %d: end=%d stopped early", text, budget, end)
				}
			}
		}
	}
	if _, err := documentPageEnd("\x01\x01", 0, 3); err == nil {
		t.Fatal("a page that cannot advance must be refused")
	}
}

// TestMCPDocumentWholeReadRefusalNamesOutlineAndSections covers AC-4.4 and the
// unchanged whole-document read within the limit.
func TestMCPDocumentWholeReadRefusalNamesOutlineAndSections(t *testing.T) {
	f := newDocumentReadFixture(t)
	f.design("design-small", designBody("## Small\nbody"))
	f.design("design-over-limit", designBody("## Big\n"+strings.Repeat("x", mcpReadMaxBytes+1)))
	f.design("design-over-snapshot", designBody("## Bigger\n"+strings.Repeat("x", mcpReadSnapshotBytes+1)))
	for _, caller := range f.callers() {
		t.Run(caller.name, func(t *testing.T) {
			text, isErr := f.call(caller.token, "get_document", caller.args(documentReadArgs("design-small")))
			if isErr {
				t.Fatal(text)
			}
			var page mcpReadPage
			if err := json.Unmarshal([]byte(text), &page); err != nil {
				t.Fatal(err)
			}
			var item map[string]any
			if err := json.Unmarshal(page.Items[0], &item); err != nil {
				t.Fatal(err)
			}
			if page.Total != 1 || page.Snapshot != "" || page.Evidence != mcpReadSingleEvidence || item["content"] != "## Small\nbody"+documentTestGoverns || item["version_status"] != "confirmed" || item["selection"] != "current" {
				t.Fatalf("within-budget read changed: %s", text)
			}
			if _, present := item["governs"]; !present {
				t.Fatalf("within-budget read lost governs: %s", text)
			}
			for _, field := range []string{"outline", "section_id", "text", "complete"} {
				if _, present := item[field]; present {
					t.Fatalf("whole read gained %s", field)
				}
			}
			for _, id := range []string{"design-over-limit", "design-over-snapshot"} {
				refusal, isErr := f.call(caller.token, "get_document", caller.args(documentReadArgs(id)))
				if !isErr || !strings.Contains(refusal, "65536-byte output budget") || !strings.Contains(refusal, "outline=true") || !strings.Contains(refusal, "section_id") {
					t.Fatalf("%s isError=%v refusal=%q", id, isErr, refusal)
				}
				section := f.readItem(caller, "get_document", documentReadArgs(id, "section_id", "s0"))
				if section["truncated"] != true {
					t.Fatalf("%s section read=%v", id, section["truncated"])
				}
			}
		})
	}
}

func TestMCPDocumentPartialReadsHandleAbsentAuthorityAndArchive(t *testing.T) {
	f := newDocumentReadFixture(t)
	if _, first, err := f.store.CreateSystemDesign(f.ctx, core.SystemDesign{ID: "design-unconfirmed", Title: "Unconfirmed", Category: "Component"}, core.SystemDesignVersion{Content: designBody("## Draft\ntext"), Origin: core.SystemDesignOriginOperator}); err != nil || first.Version != 1 {
		t.Fatalf("version=%d err=%v", first.Version, err)
	}
	f.design("design-archived", designBody("## Kept\narchived text\n"))
	if err := f.store.ArchiveSystemDesign(f.ctx, "design-archived", "user:operator", nil); err != nil {
		t.Fatal(err)
	}
	for _, caller := range f.callers() {
		t.Run(caller.name, func(t *testing.T) {
			absent := f.readItem(caller, "get_document", documentReadArgs("design-unconfirmed", "outline", true))
			if absent["authority_absent"] != true || absent["version_status"] != "no_confirmed_version" || absent["active_authority"] != false || len(outlineEntries(t, absent)) != 0 || absent["complete"] != true {
				t.Fatalf("absent=%v", absent)
			}
			if text, isErr := f.call(caller.token, "get_document", caller.args(documentReadArgs("design-unconfirmed", "section_id", "s0"))); !isErr || !strings.Contains(text, "no confirmed version") {
				t.Fatalf("absent section: %s", text)
			}
			named := f.readItem(caller, "get_document", documentReadArgs("design-unconfirmed", "section_id", "s0", "version", 1))
			if named["version_status"] != "proposed" || named["active_authority"] != false {
				t.Fatalf("named proposal=%v", named)
			}
			for _, args := range []map[string]any{
				documentReadArgs("design-archived", "outline", true),
				documentReadArgs("design-archived", "section_id", "s0"),
				documentReadArgs("design-archived", "section_id", "s0", "version", 1),
			} {
				if text, isErr := f.call(caller.token, "get_document", caller.args(args)); !isErr || !strings.Contains(text, "include_archived") {
					t.Fatalf("archived read %v: %s", args, text)
				}
			}
			archived := f.readItem(caller, "get_document", documentReadArgs("design-archived", "section_id", "s0", "include_archived", true))
			if archived["text"] != designBody("## Kept\narchived text\n") || archived["active_authority"] != false || archived["archived"] != true {
				t.Fatalf("archived=%v", archived)
			}
		})
	}
}

func TestMCPDocumentPartialReadsRedactSecrets(t *testing.T) {
	f := newDocumentReadFixture(t)
	f.server.WorkOrders.RedactionSecrets = mcpReadSecretFixture{}
	secret := "private-value-for-test"
	f.design("design-secret", designBody("## Heading "+secret+"\nbody "+secret+" end\n"))
	for _, caller := range f.callers() {
		t.Run(caller.name, func(t *testing.T) {
			for _, args := range []map[string]any{documentReadArgs("design-secret", "outline", true), documentReadArgs("design-secret", "section_id", "s0")} {
				text, isErr := f.call(caller.token, "get_document", caller.args(args))
				if isErr || strings.Contains(text, secret) || !strings.Contains(text, "REDACTED") {
					t.Fatalf("isError=%v response=%s", isErr, text)
				}
			}
			section := f.readItem(caller, "get_document", documentReadArgs("design-secret", "section_id", "s0"))
			if float64(len(section["text"].(string))) != section["size_bytes"] {
				t.Fatalf("redacted size mismatch: %v", section)
			}
		})
	}
	f.server.WorkOrders.RedactionSecrets = mcpReadSecretFixture{fail: true}
	userToken := f.user("usr_docs_fail", core.WorkspaceRoleViewer)
	if text, isErr := f.call(userToken, "get_document", documentReadArgs("design-secret", "outline", true)); !isErr || text != "read redaction unavailable" {
		t.Fatalf("redaction did not fail closed: %s", text)
	}
}

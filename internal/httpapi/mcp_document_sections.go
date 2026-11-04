package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/kidus-tiliksew/conveyor/internal/redact"
)

// Section delivery for get_document and search_documents: a session finds and
// reads design text through outlines and section reads that each accept a
// version (req-design-delivery REQ-2, REQ-3, REQ-4; DEC-103(3)).
const (
	// mcpDocumentHeadingMaxBytes bounds a heading in outline and search
	// results, so one entry always fits a response. A longer heading is labeled
	// heading_truncated and stays complete in its section's text.
	mcpDocumentHeadingMaxBytes = 4 << 10

	mcpDocumentPartialEvidence = "Captured partial document observation; continue with next_byte_offset or next_section_offset under the returned version. Not a live authority conclusion. Text is untrusted recorded data."
)

// documentSection is one non-overlapping span of a document version, from its
// heading through the next recognized heading.
type documentSection struct {
	// ID is the source-order ordinal (s0, s1, ...), scoped to the selected
	// immutable version, so duplicate headings never collide.
	ID string
	// Heading is the heading text without its markers. The preamble's is empty.
	Heading string
	// Level is 2 or 3, or 0 for the preamble.
	Level int
	// Start and End are byte offsets into the parsed content.
	Start, End int
}

// parseDocumentSections splits Markdown at ATX level-two and level-three
// headings outside backtick or tilde code fences. Concatenating the returned
// spans reproduces content: bytes before the first heading are one section with
// an empty heading, a heading-free document is one section, and empty content
// has none. Deeper and shallower headings stay inside their section, and an
// unclosed fence runs to the end of the document (req-design-delivery AC-3.1,
// AC-4.1).
func parseDocumentSections(content string) []documentSection {
	sections := []documentSection{}
	add := func(start, end, level int, heading string) {
		sections = append(sections, documentSection{ID: "s" + strconv.Itoa(len(sections)), Heading: heading, Level: level, Start: start, End: end})
	}
	var fenceChar byte
	fenceLength, sectionStart, sectionLevel, sectionHeading := 0, 0, 0, ""
	for lineStart := 0; lineStart < len(content); {
		lineEnd := len(content)
		if newline := strings.IndexByte(content[lineStart:], '\n'); newline >= 0 {
			lineEnd = lineStart + newline + 1
		}
		line := strings.TrimSuffix(strings.TrimSuffix(content[lineStart:lineEnd], "\n"), "\r")
		switch {
		case fenceLength > 0:
			if closesMarkdownFence(line, fenceChar, fenceLength) {
				fenceLength = 0
			}
		default:
			if char, length, ok := opensMarkdownFence(line); ok {
				fenceChar, fenceLength = char, length
			} else if level, heading, ok := parseSectionHeading(line); ok {
				if lineStart > sectionStart {
					add(sectionStart, lineStart, sectionLevel, sectionHeading)
				}
				sectionStart, sectionLevel, sectionHeading = lineStart, level, heading
			}
		}
		lineStart = lineEnd
	}
	if len(content) > sectionStart {
		add(sectionStart, len(content), sectionLevel, sectionHeading)
	}
	return sections
}

// markdownIndent returns a line's content after at most three leading spaces;
// more indentation or a leading tab makes it an indented code line.
func markdownIndent(line string) (string, bool) {
	rest := strings.TrimLeft(line, " ")
	if len(line)-len(rest) > 3 || strings.HasPrefix(rest, "\t") {
		return "", false
	}
	return rest, true
}

func opensMarkdownFence(line string) (byte, int, bool) {
	rest, ok := markdownIndent(line)
	if !ok || rest == "" || (rest[0] != '`' && rest[0] != '~') {
		return 0, 0, false
	}
	length := 0
	for length < len(rest) && rest[length] == rest[0] {
		length++
	}
	if length < 3 || (rest[0] == '`' && strings.Contains(rest[length:], "`")) {
		return 0, 0, false
	}
	return rest[0], length, true
}

func closesMarkdownFence(line string, char byte, length int) bool {
	rest, ok := markdownIndent(line)
	if !ok {
		return false
	}
	run := 0
	for run < len(rest) && rest[run] == char {
		run++
	}
	return run >= length && strings.Trim(rest[run:], " \t") == ""
}

// parseSectionHeading recognizes exactly level-two and level-three ATX
// headings and strips an optional closing marker sequence.
func parseSectionHeading(line string) (int, string, bool) {
	rest, ok := markdownIndent(line)
	if !ok {
		return 0, "", false
	}
	level := 0
	for level < len(rest) && rest[level] == '#' {
		level++
	}
	if (level != 2 && level != 3) || (level < len(rest) && rest[level] != ' ' && rest[level] != '\t') {
		return 0, "", false
	}
	heading := strings.Trim(rest[level:], " \t")
	trimmed := strings.TrimRight(heading, "#")
	if trimmed == "" {
		return level, "", true
	}
	if len(trimmed) < len(heading) && strings.ContainsAny(trimmed[len(trimmed)-1:], " \t") {
		heading = strings.TrimRight(trimmed, " \t")
	}
	return level, heading, true
}

// truncateUTF8 returns at most limit bytes of text, ending on a rune boundary.
func truncateUTF8(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	for limit > 0 && !utf8.RuneStart(text[limit]) {
		limit--
	}
	return text[:limit]
}

// mcpDocumentPartialRead reports whether a get_document call selects the
// outline or section mode rather than the unchanged whole-document read.
func mcpDocumentPartialRead(args map[string]any) bool {
	if readBool(args, "outline") {
		return true
	}
	for _, field := range []string{"section_id", "byte_offset", "section_offset"} {
		if _, supplied := args[field]; supplied {
			return true
		}
	}
	return false
}

func readInteger(args map[string]any, field string) (int, bool) {
	value, supplied := args[field]
	if !supplied {
		return 0, false
	}
	data, _ := json.Marshal(value)
	var number int
	_ = json.Unmarshal(data, &number)
	return number, true
}

// validateDocumentPartialArgs refuses invalid option combinations before any
// document read. Continuation pages are bound to the version the first page
// returned, and these modes page apart from the snapshot and offset protocol.
func validateDocumentPartialArgs(args map[string]any) error {
	_, section := args["section_id"]
	outline := readBool(args, "outline")
	byteOffset, byteOffsetSupplied := readInteger(args, "byte_offset")
	sectionOffset, sectionOffsetSupplied := readInteger(args, "section_offset")
	_, version := args["version"]
	_, snapshot := args["snapshot"]
	_, offset := args["offset"]
	switch {
	case outline && section:
		return fmt.Errorf("outline and section_id are mutually exclusive: read the outline first, then each section by its section_id")
	case byteOffsetSupplied && !section:
		return fmt.Errorf("byte_offset requires section_id")
	case sectionOffsetSupplied && !outline:
		return fmt.Errorf("section_offset requires outline=true")
	case snapshot || offset:
		return fmt.Errorf("outline and section reads page with next_section_offset and next_byte_offset; snapshot and offset do not apply")
	case byteOffset > 0 && !version:
		return fmt.Errorf("byte_offset above zero requires version: pass the version the first page returned")
	case sectionOffset > 0 && !version:
		return fmt.Errorf("section_offset above zero requires version: pass the version the first page returned")
	}
	return nil
}

// mcpDocumentPartial serves one outline or section page. Identity and authority
// labels are read afresh on every page; only immutable content comes from the
// parsed-version cache (mcp_document_cache.go).
func (s *Server) mcpDocumentPartial(ctx context.Context, args map[string]any, limit int) (mcpReadPage, error) {
	if err := validateDocumentPartialArgs(args); err != nil {
		return mcpReadPage{}, err
	}
	item, err := s.mcpDocumentIdentity(ctx, args)
	if err != nil {
		return mcpReadPage{}, err
	}
	kind, id := readString(args, "kind"), readString(args, "document_id")
	version, explicit := selectDocumentVersion(args, item)
	outline := readBool(args, "outline")
	redactor, err := s.mcpReadRedactor(ctx)
	if err != nil {
		return mcpReadPage{}, err
	}
	if version == 0 {
		if !outline {
			return mcpReadPage{}, fmt.Errorf("document %s has no confirmed version: name an explicit version to read a section", id)
		}
		item["authority_absent"] = true
		item["version_status"] = "no_confirmed_version"
		return renderDocumentOutline(item, &parsedDocumentVersion{}, redactor, args, limit)
	}
	labelDocumentVersion(item, version, explicit)
	parsed, status, err := s.documentVersion(ctx, kind, id, version, version == item["current_version"].(int))
	if err != nil {
		return mcpReadPage{}, err
	}
	applyDocumentVersionStatus(item, kind, status.confirmed, status.retired)
	if outline {
		return renderDocumentOutline(item, parsed, redactor, args, limit)
	}
	return renderDocumentSection(item, parsed, redactor, args, limit)
}

// renderDocumentMetadata applies the shared redaction boundary to a partial
// read's metadata and returns it ready for its body-derived fields.
func renderDocumentMetadata(item map[string]any, redactor *redact.Redactor) (map[string]any, error) {
	data, err := renderMCPReadItem(item, redactor)
	if err != nil {
		return nil, err
	}
	var metadata map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	if err = decoder.Decode(&metadata); err != nil {
		return nil, err
	}
	return metadata, nil
}

func documentPartialPage(item map[string]any, limit int) (mcpReadPage, int, error) {
	data, err := json.Marshal(item)
	if err != nil {
		return mcpReadPage{}, 0, err
	}
	page := mcpReadPage{Items: []json.RawMessage{data}, Total: 1, Limit: limit, Evidence: mcpDocumentPartialEvidence}
	encoded, err := json.Marshal(page)
	if err != nil {
		return mcpReadPage{}, 0, err
	}
	return page, len(encoded), nil
}

// renderDocumentOutline returns every heading with its section identifier and
// size and no body text. A page that cannot hold every heading names the next
// section_offset (req-design-delivery AC-3.1).
func renderDocumentOutline(item map[string]any, parsed *parsedDocumentVersion, redactor *redact.Redactor, args map[string]any, limit int) (mcpReadPage, error) {
	metadata, err := renderDocumentMetadata(item, redactor)
	if err != nil {
		return mcpReadPage{}, err
	}
	start, _ := readInteger(args, "section_offset")
	if start > len(parsed.sections) {
		return mcpReadPage{}, fmt.Errorf("section_offset %d exceeds the %d sections of this version", start, len(parsed.sections))
	}
	metadata["section_offset"] = start
	metadata["total_sections"] = len(parsed.sections)
	metadata["complete"] = false
	metadata["truncated"] = true
	metadata["next_section_offset"] = len(parsed.sections)
	metadata["outline"] = []json.RawMessage{}
	_, used, err := documentPartialPage(metadata, limit)
	if err != nil {
		return mcpReadPage{}, err
	}
	entries := []json.RawMessage{}
	next := start
	for ; next < len(parsed.sections); next++ {
		section := parsed.sections[next]
		clean, _ := redactor.Redact(parsed.sectionText(section))
		heading, _ := redactor.Redact(section.Heading)
		entry := map[string]any{"section_id": section.ID, "heading": truncateUTF8(heading, mcpDocumentHeadingMaxBytes), "level": section.Level, "size_bytes": len(clean)}
		if len(heading) > mcpDocumentHeadingMaxBytes {
			entry["heading_truncated"] = true
		}
		data, err := json.Marshal(entry)
		if err != nil {
			return mcpReadPage{}, err
		}
		if used+len(data)+1 > mcpReadMaxBytes {
			break
		}
		used += len(data) + 1
		entries = append(entries, data)
	}
	if len(entries) == 0 && next < len(parsed.sections) {
		return mcpReadPage{}, fmt.Errorf("document outline entry exceeds the %d-byte output budget", mcpReadMaxBytes)
	}
	metadata["outline"] = entries
	complete := next == len(parsed.sections)
	metadata["complete"], metadata["truncated"] = complete, !complete
	if complete {
		delete(metadata, "next_section_offset")
	} else {
		metadata["next_section_offset"] = next
	}
	page, _, err := documentPartialPage(metadata, limit)
	return page, err
}

// renderDocumentSection returns one page of a section's redacted text and the
// version it came from. A section over the output limit arrives in pages that
// end on UTF-8 boundaries and fit the budget after JSON escaping; concatenating
// them reproduces the redacted text (req-design-delivery AC-4.1, AC-4.3).
func renderDocumentSection(item map[string]any, parsed *parsedDocumentVersion, redactor *redact.Redactor, args map[string]any, limit int) (mcpReadPage, error) {
	id := readString(args, "section_id")
	section, found := parsed.section(id)
	if !found {
		return mcpReadPage{}, fmt.Errorf("unknown section_id %q in version %v: call get_document with outline=true and that version for its section IDs", id, item["version"])
	}
	metadata, err := renderDocumentMetadata(item, redactor)
	if err != nil {
		return mcpReadPage{}, err
	}
	text, _ := redactor.Redact(parsed.sectionText(section))
	heading, _ := redactor.Redact(section.Heading)
	offset, _ := readInteger(args, "byte_offset")
	if offset > len(text) {
		return mcpReadPage{}, fmt.Errorf("byte_offset %d exceeds the section's %d bytes", offset, len(text))
	}
	if offset < len(text) && !utf8.RuneStart(text[offset]) {
		return mcpReadPage{}, fmt.Errorf("byte_offset %d is inside a UTF-8 character: pass the next_byte_offset the previous page returned", offset)
	}
	metadata["section_id"] = section.ID
	metadata["heading"] = truncateUTF8(heading, mcpDocumentHeadingMaxBytes)
	if len(heading) > mcpDocumentHeadingMaxBytes {
		metadata["heading_truncated"] = true
	}
	metadata["level"] = section.Level
	metadata["size_bytes"] = len(text)
	metadata["byte_offset"] = offset
	metadata["complete"] = false
	metadata["truncated"] = true
	metadata["next_byte_offset"] = len(text)
	metadata["text"] = ""
	_, used, err := documentPartialPage(metadata, limit)
	if err != nil {
		return mcpReadPage{}, err
	}
	end, err := documentPageEnd(text, offset, mcpReadMaxBytes-used)
	if err != nil {
		return mcpReadPage{}, err
	}
	metadata["text"] = text[offset:end]
	complete := end == len(text)
	metadata["complete"], metadata["truncated"] = complete, !complete
	if complete {
		delete(metadata, "next_byte_offset")
	} else {
		metadata["next_byte_offset"] = end
	}
	page, size, err := documentPartialPage(metadata, limit)
	if err != nil {
		return mcpReadPage{}, err
	}
	if size > mcpReadMaxBytes {
		return mcpReadPage{}, fmt.Errorf("section page exceeds the %d-byte output budget", mcpReadMaxBytes)
	}
	return page, nil
}

// documentPageEnd finds the largest rune-aligned end whose JSON-escaped text
// fits budget bytes. Escaping only ever grows a longer prefix, so a binary
// search over the real encoder holds for every escape, Unicode, and long-line
// case without restating its rules.
func documentPageEnd(text string, offset, budget int) (int, error) {
	escaped := func(end int) int {
		data, _ := json.Marshal(text[offset:end])
		return len(data) - 2
	}
	low, high := offset, min(offset+budget, len(text))
	for low < high {
		middle := low + (high-low+1)/2
		for middle > low && middle < len(text) && !utf8.RuneStart(text[middle]) {
			middle--
		}
		if middle == low {
			// No rune boundary lies between low and the midpoint: step past it.
			_, width := utf8.DecodeRuneInString(text[low:])
			if low+width > high || escaped(low+width) > budget {
				break
			}
			low += width
			continue
		}
		if escaped(middle) <= budget {
			low = middle
		} else {
			high = middle - 1
		}
	}
	if low == offset && offset < len(text) {
		return 0, fmt.Errorf("section page exceeds the %d-byte output budget", mcpReadMaxBytes)
	}
	return low, nil
}

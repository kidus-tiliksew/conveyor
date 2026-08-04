package core

import "testing"

func TestNormalizeReferenceMarkdown(t *testing.T) {
	if got, err := NormalizeReferenceMarkdown("Overview.md", "text/markdown; charset=utf-8", []byte("# Product\n\nFacts.")); err != nil || got != "# Product\n\nFacts." {
		t.Fatalf("markdown normalization = %q, %v", got, err)
	}
	for _, media := range []string{"application/octet-stream", "text/plain; charset=utf-8", "text/x-markdown"} {
		if got, err := NormalizeReferenceMarkdown("overview.markdown", media, []byte("# Product")); err != nil || got != "# Product" {
			t.Fatalf("generic markdown media %q = %q, %v", media, got, err)
		}
	}
	for _, content := range []string{"<!-- centered heading -->\n# Product", `<div align="center">Product</div>`} {
		if got, err := NormalizeReferenceMarkdown("overview.md", "text/markdown", []byte(content)); err != nil || got != content {
			t.Fatalf("HTML-leading markdown normalization = %q, %v", got, err)
		}
	}
	for _, input := range []struct {
		name, media string
		content     []byte
	}{
		{"overview.pdf", "application/pdf", []byte("content")},
		{"overview.md", "application/pdf", []byte("content")},
		{"overview.docx", "text/markdown", []byte("content")},
		{"overview.md", "application/octet-stream", []byte("%PDF-1.7\n")},
		{"overview.md", "application/octet-stream", []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}},
		{"overview.md", "text/plain", []byte{'t', 'e', 'x', 't', 0}},
		{"overview.md", "not a media type", []byte("content")},
	} {
		if _, err := NormalizeReferenceMarkdown(input.name, input.media, input.content); err == nil {
			t.Fatalf("accepted %s as %s with %q", input.name, input.media, input.content)
		}
	}
}

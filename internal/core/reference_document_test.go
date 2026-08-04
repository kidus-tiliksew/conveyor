package core

import "testing"

func TestNormalizeReferenceMarkdown(t *testing.T) {
	if got, err := NormalizeReferenceMarkdown("Overview.md", "text/markdown; charset=utf-8", []byte("# Product\n\nFacts.")); err != nil || got != "# Product\n\nFacts." {
		t.Fatalf("markdown normalization = %q, %v", got, err)
	}
	for _, input := range []struct{ name, media string }{{"overview.pdf", "application/pdf"}, {"overview.md", "application/pdf"}, {"overview.docx", "text/markdown"}} {
		if _, err := NormalizeReferenceMarkdown(input.name, input.media, []byte("content")); err == nil {
			t.Fatalf("accepted %s as %s", input.name, input.media)
		}
	}
}

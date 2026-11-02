package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestStyledConfigRowsAlignAndAttributeSources(t *testing.T) {
	var output bytes.Buffer
	if err := renderCLIConfigRow(&output, true, "workspace", "demo", "stored file"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "workspace") || !strings.Contains(output.String(), "demo") || !strings.Contains(output.String(), "source: stored file") {
		t.Fatalf("styled config row = %q", output.String())
	}
}

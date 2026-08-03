package main

import (
	"strings"
	"testing"
)

func TestLineageRebuildLongHelpDocumentsOperationalBound(t *testing.T) {
	command := lineageCmd()
	rebuild, _, err := command.Find([]string{"rebuild"})
	if err != nil {
		t.Fatal(err)
	}
	for _, phrase := range []string{
		"every workspace event in memory within one transaction",
		"never deletes rows it cannot regenerate",
		"Stale identity cleanup requires a migration",
	} {
		if !strings.Contains(rebuild.Long, phrase) {
			t.Fatalf("lineage rebuild help omitted %q: %s", phrase, rebuild.Long)
		}
	}
}

package singlestore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPackageBindsLogThroughNoticeBatch: a store transaction binds the log
// only through logTx, which also binds the transaction's notice batch, so a
// job appended inside it raises its notice only after commit
// (component-persistence, Change notices).
func TestPackageBindsLogThroughNoticeBatch(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") || file == "change_notices.go" {
			continue
		}
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for number, line := range strings.Split(string(body), "\n") {
			if strings.Contains(line, "s2log.WithTx(") {
				t.Errorf("%s:%d binds the log without its notice batch; use logTx: %s", file, number+1, strings.TrimSpace(line))
			}
		}
	}
}

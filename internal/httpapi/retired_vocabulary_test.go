package httpapi

import (
	"bufio"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRetiredExecutionModeVocabularyStaysOutOfPayloadsAndWebCopy(t *testing.T) {
	t.Parallel()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test filename")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
	forbidden := []string{
		"auto task", "auto-task", "manual task", "manual-task",
		"auto mode", "auto-mode", "manual mode", "manual-mode",
		"execution mode", "execution-mode",
	}
	// These are the only nearby uses of "auto" vocabulary that remain part of
	// the product contract: gate labels and the local run-mode implementation
	// constant. They are intentionally explicit so additions require review.
	allowlist := map[string][]string{
		"web/src/components/task/task-create-sheet.tsx": {"Auto-approve", "Auto-merge"},
		"cmd/conveyor/run_cmd.go":                       {"runModeAuto"},
	}

	paths := []string{filepath.Join(root, "internal", "httpapi"), filepath.Join(root, "web", "src")}
	for relative := range allowlist {
		if strings.HasPrefix(relative, "cmd/") {
			paths = append(paths, filepath.Join(root, filepath.FromSlash(relative)))
		}
	}
	seenAllowlist := make(map[string]bool)
	for _, path := range paths {
		err := filepath.WalkDir(path, func(current string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				if entry.Name() == "dashboard" {
					return filepath.SkipDir
				}
				return nil
			}
			extension := filepath.Ext(current)
			if extension != ".go" && extension != ".ts" && extension != ".tsx" || strings.HasSuffix(current, "_test.go") {
				return nil
			}
			relative, err := filepath.Rel(root, current)
			if err != nil {
				return err
			}
			relative = filepath.ToSlash(relative)
			file, err := os.Open(current)
			if err != nil {
				return err
			}
			defer file.Close()
			scanner := bufio.NewScanner(file)
			lineNumber := 0
			for scanner.Scan() {
				lineNumber++
				line := scanner.Text()
				for _, allowed := range allowlist[relative] {
					if strings.Contains(line, allowed) {
						seenAllowlist[relative+"\x00"+allowed] = true
					}
				}
				lower := strings.ToLower(line)
				for _, retired := range forbidden {
					if strings.Contains(lower, retired) {
						t.Errorf("retired vocabulary %q in %s:%d", retired, relative, lineNumber)
					}
				}
			}
			return scanner.Err()
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for relative, values := range allowlist {
		for _, value := range values {
			if !seenAllowlist[relative+"\x00"+value] {
				t.Errorf("documented allowlist entry %q is absent from %s", value, relative)
			}
		}
	}
}

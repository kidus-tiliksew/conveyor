package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/docsconfig"
	"github.com/spf13/cobra"
)

func docsCmd() *cobra.Command {
	c := &cobra.Command{Use: "docs", Short: "Inspect repository documentation declarations"}
	c.AddCommand(docsValidateCmd())
	return c
}

type docsValidateDiagnostic struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}
type docsValidateReceipt struct {
	Path        string                   `json:"path"`
	Status      string                   `json:"status,omitempty"`
	Diagnostics []docsValidateDiagnostic `json:"diagnostics"`
}

func docsValidateCmd() *cobra.Command {
	return &cobra.Command{Use: "validate [path]", Short: "Validate .conveyor/docs.yaml against tracked files", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		input := "."
		if len(args) > 0 {
			input = args[0]
		}
		receipt, err := validateLocalDocs(cmd.Context(), input)
		if outErr := json.NewEncoder(cmd.OutOrStdout()).Encode(receipt); outErr != nil {
			return outErr
		}
		return err
	}}
}
func validateLocalDocs(ctx context.Context, input string) (docsValidateReceipt, error) {
	receipt := docsValidateReceipt{Path: input, Diagnostics: []docsValidateDiagnostic{}}
	fail := func(err error) (docsValidateReceipt, error) {
		receipt.Diagnostics = append(receipt.Diagnostics, docsValidateDiagnostic{Path: receipt.Path, Message: err.Error()})
		return receipt, err
	}
	abs, err := filepath.Abs(input)
	if err != nil {
		return fail(err)
	}
	declaration := abs
	info, err := os.Stat(abs)
	if err != nil {
		return fail(err)
	}
	if info.IsDir() {
		declaration = filepath.Join(abs, filepath.FromSlash(docsconfig.FilePath))
	}
	receipt.Path = declaration
	info, err = os.Lstat(declaration)
	if err != nil {
		if os.IsNotExist(err) {
			receipt.Status = "gate off: declaration absent"
			return receipt, nil
		}
		return fail(err)
	}
	if !info.Mode().IsRegular() {
		return fail(fmt.Errorf("documentation declaration must be a regular file, not a symlink or directory"))
	}
	f, err := os.Open(declaration)
	if err != nil {
		return fail(err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, docsconfig.MaxFileBytes+1))
	if err != nil {
		return fail(err)
	}
	cfg, err := docsconfig.Parse(data)
	if err != nil {
		return fail(err)
	}
	rootBytes, err := docsGit(ctx, filepath.Dir(declaration), "rev-parse", "--show-toplevel")
	if err != nil {
		return fail(err)
	}
	tracked, err := docsGit(ctx, strings.TrimSpace(string(rootBytes)), "ls-files", "-z")
	if err != nil {
		return fail(err)
	}
	files := strings.Split(string(tracked), "\x00")
	for entry, doc := range cfg.Docs {
		for index := 0; index <= len(doc.Governs); index++ {
			glob := doc.Path
			if index > 0 {
				glob = doc.Governs[index-1]
			}
			if err := ctx.Err(); err != nil {
				return fail(err)
			}
			pattern, err := core.CompileGovernedPath(glob)
			if err != nil {
				return fail(err)
			}
			matched := false
			for _, file := range files {
				if err := ctx.Err(); err != nil {
					return fail(err)
				}
				if pattern.Match(file) {
					matched = true
					break
				}
			}
			if !matched {
				key := fmt.Sprintf("docs[%d].path", entry)
				if index > 0 {
					key = fmt.Sprintf("docs[%d].governs[%d]", entry, index-1)
				}
				receipt.Diagnostics = append(receipt.Diagnostics, docsValidateDiagnostic{Path: key, Message: fmt.Sprintf("matches no tracked file: %s", glob)})
			}
		}
	}
	if len(receipt.Diagnostics) > 0 {
		return receipt, fmt.Errorf("documentation declaration has %d invalid entries", len(receipt.Diagnostics))
	}
	return receipt, nil
}
func docsGit(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

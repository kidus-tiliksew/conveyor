package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/governedpath"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
	"github.com/spf13/cobra"
)

// riskBuiltinOnlyStatus is the AC-3.6 report for a worktree with no
// declaration.
const riskBuiltinOnlyStatus = "no declaration: only built-in minimums apply"

func riskCmd() *cobra.Command {
	c := &cobra.Command{Use: "risk", Short: "Inspect repository risk declarations"}
	c.AddCommand(riskValidateCmd())
	return c
}

type riskValidateDiagnostic struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

type riskValidateReceipt struct {
	Path        string                   `json:"path"`
	Status      string                   `json:"status,omitempty"`
	Diagnostics []riskValidateDiagnostic `json:"diagnostics"`
}

func riskValidateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "validate [path]",
		Short: "Validate .conveyor/risk.yaml against tracked files",
		Long: "Validate the worktree's .conveyor/risk.yaml: its schema, and that each declared glob matches at least one tracked file. " +
			"With no declaration, it reports that only the built-in minimums apply and succeeds.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			input := "."
			if len(args) > 0 {
				input = args[0]
			}
			receipt, err := validateLocalRisk(cmd.Context(), input)
			if outErr := json.NewEncoder(cmd.OutOrStdout()).Encode(receipt); outErr != nil {
				return outErr
			}
			return err
		},
	}
}

// validateLocalRisk implements req-risk-based-review AC-3.6. The command owns
// the file and Git reads; internal/riskpolicy parses the bytes and
// internal/governedpath matches the globs (component-risk-review, Boundary).
// It reports every declared glob that matches no tracked file, in
// declaration order. The built-in globs need not match a tracked file.
func validateLocalRisk(ctx context.Context, input string) (riskValidateReceipt, error) {
	receipt := riskValidateReceipt{Path: input, Diagnostics: []riskValidateDiagnostic{}}
	fail := func(err error) (riskValidateReceipt, error) {
		receipt.Diagnostics = append(receipt.Diagnostics, riskValidateDiagnostic{Path: receipt.Path, Message: err.Error()})
		return receipt, err
	}
	abs, err := filepath.Abs(input)
	if err != nil {
		return fail(err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return fail(err)
	}
	declaration := abs
	if info.IsDir() {
		declaration = filepath.Join(abs, filepath.FromSlash(riskpolicy.DeclarationPath))
	}
	receipt.Path = declaration
	info, err = os.Lstat(declaration)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			receipt.Status = riskBuiltinOnlyStatus
			return receipt, nil
		}
		return fail(err)
	}
	if !info.Mode().IsRegular() {
		return fail(fmt.Errorf("risk declaration must be a regular file, not a symlink or directory"))
	}
	f, err := os.Open(declaration)
	if err != nil {
		return fail(err)
	}
	defer f.Close()
	// Read one byte past the limit so the parser sees an oversize file.
	data, err := io.ReadAll(io.LimitReader(f, riskpolicy.MaxDeclarationBytes+1))
	if err != nil {
		return fail(err)
	}
	parsed, err := riskpolicy.ParseDeclaration(data)
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
	for ruleIndex, rule := range parsed.Rules {
		for globIndex, glob := range rule.Paths {
			if err := ctx.Err(); err != nil {
				return fail(err)
			}
			matcher, err := governedpath.Compile(glob)
			if err != nil {
				return fail(err)
			}
			matched := false
			for _, file := range files {
				if matcher.Match(file) {
					matched = true
					break
				}
			}
			if !matched {
				receipt.Diagnostics = append(receipt.Diagnostics, riskValidateDiagnostic{
					Path:    fmt.Sprintf("rules[%d].paths[%d]", ruleIndex, globIndex),
					Message: fmt.Sprintf("matches no tracked file: %s", glob),
				})
			}
		}
	}
	if len(receipt.Diagnostics) > 0 {
		return receipt, fmt.Errorf("risk declaration has %d globs that match no tracked file", len(receipt.Diagnostics))
	}
	return receipt, nil
}

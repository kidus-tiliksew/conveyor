package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/docsconfig"
	"github.com/spf13/cobra"
)

func docsCmd() *cobra.Command {
	command := &cobra.Command{Use: "docs", Short: "Inspect the repository documentation-closure gate"}
	command.AddCommand(docsValidateCmd())
	return command
}

type docsValidateDiagnostic struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

type docsValidateGlob struct {
	Path    string   `json:"path"`
	Matches []string `json:"matches"`
}

type docsValidateReceipt struct {
	SchemaVersion  int                      `json:"schema_version"`
	Path           string                   `json:"path"`
	Revision       string                   `json:"revision,omitempty"`
	Globs          []docsValidateGlob       `json:"globs"`
	NoneStatement  string                   `json:"none_statement,omitempty"`
	ReasonRequired bool                     `json:"reason_required"`
	Diagnostics    []docsValidateDiagnostic `json:"diagnostics"`
}

// docsValidateCmd validates .conveyor/docs.yaml against tracked files with no
// network access. A declared glob that matches no tracked file is an error, so
// a repository seeds at least one declared doc before adopting the gate.
func docsValidateCmd() *cobra.Command {
	command := &cobra.Command{
		Use: "validate [path]", Short: "Validate the documentation-closure declaration offline", Args: cobra.MaximumNArgs(1),
		Long: "Validate a .conveyor/docs.yaml file (or a repository directory) against tracked files and print a JSON receipt. The declaration must name at least one repository-relative glob that matches a tracked file. No server is contacted.",
		RunE: func(cmd *cobra.Command, args []string) error {
			input := "."
			if len(args) == 1 {
				input = args[0]
			}
			receipt, validationErr := validateLocalDocs(cmd.Context(), input)
			encoder := json.NewEncoder(cmd.OutOrStdout())
			encoder.SetIndent("", "  ")
			if err := encoder.Encode(receipt); err != nil {
				return err
			}
			return validationErr
		},
	}
	return command
}

func validateLocalDocs(ctx context.Context, input string) (docsValidateReceipt, error) {
	receipt := docsValidateReceipt{SchemaVersion: 1, Path: input, Globs: []docsValidateGlob{}, Diagnostics: []docsValidateDiagnostic{}}
	failed := func(path string, err error) (docsValidateReceipt, error) {
		receipt.Diagnostics = append(receipt.Diagnostics, docsValidateDiagnostic{Path: path, Message: err.Error()})
		return receipt, err
	}
	abs, err := filepath.Abs(input)
	if err != nil {
		return failed(input, err)
	}
	declaration := abs
	if st, statErr := os.Stat(abs); statErr == nil && st.IsDir() {
		declaration = filepath.Join(abs, filepath.FromSlash(docsconfig.FilePath))
	}
	rootBytes, err := docsGit(ctx, filepath.Dir(declaration), "rev-parse", "--show-toplevel")
	if err != nil {
		return failed(input, err)
	}
	root := strings.TrimSpace(string(rootBytes))
	receipt.Path = declaration
	data, err := os.ReadFile(declaration)
	if err != nil {
		return failed(declaration, err)
	}
	cfg, err := docsconfig.Parse(data)
	if err != nil {
		return failed(declaration, err)
	}
	receipt.NoneStatement = cfg.Rule.NoneStatement
	receipt.ReasonRequired = cfg.Rule.ReasonRequired
	if revision, revErr := docsGit(ctx, root, "rev-parse", "HEAD"); revErr == nil {
		receipt.Revision = strings.TrimSpace(string(revision))
	}
	tracked, err := docsGit(ctx, root, "ls-files", "-z")
	if err != nil {
		return failed(root, err)
	}
	files := []string{}
	for _, record := range bytes.Split(tracked, []byte{0}) {
		if len(record) > 0 {
			files = append(files, string(record))
		}
	}
	var failedGlobs []string
	for _, glob := range cfg.Paths() {
		entry := docsValidateGlob{Path: glob, Matches: []string{}}
		for _, file := range files {
			if docsconfig.MatchGlob(glob, file) {
				entry.Matches = append(entry.Matches, file)
			}
		}
		if len(entry.Matches) == 0 {
			failedGlobs = append(failedGlobs, glob)
		}
		receipt.Globs = append(receipt.Globs, entry)
	}
	if len(failedGlobs) > 0 {
		for _, glob := range failedGlobs {
			receipt.Diagnostics = append(receipt.Diagnostics, docsValidateDiagnostic{Path: glob, Message: "declared glob matches no tracked file"})
		}
		return receipt, fmt.Errorf("documentation closure declaration %s has globs matching no tracked file", declaration)
	}
	return receipt, nil
}

func docsGit(ctx context.Context, root string, args ...string) ([]byte, error) {
	argv := append([]string{"--literal-pathspecs", "-c", "core.fsmonitor=false", "-C", root}, args...)
	command := exec.CommandContext(ctx, "git", argv...)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	data, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("local git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return data, nil
}

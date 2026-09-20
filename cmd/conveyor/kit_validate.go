package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/verification"
	"github.com/spf13/cobra"
)

func kitCmd() *cobra.Command {
	command := &cobra.Command{Use: "kit", Short: "Inspect repository verification kits"}
	command.AddCommand(kitValidateCmd())
	return command
}

// kitValidateCmd uses only local files and Git objects. No client, credentials,
// network discovery or exercise process participates (VK-3, AC-2.4).
func kitValidateCmd() *cobra.Command {
	var stage string
	var rawPins []string
	command := &cobra.Command{
		Use: "validate <path>", Short: "Validate a kit manifest and print its selection receipt offline", Args: cobra.ExactArgs(1),
		Long: "Validate a manifest file (or a repository directory's .conveyor/kits/manifest.yaml) against explicit authoritative pins. Digests use committed HEAD kit trees; changed or untracked kit files are invalid until committed. Pin authority is supplied by the caller and is not verified with a server.",
		RunE: func(cmd *cobra.Command, args []string) error {
			pins, err := parseKitPins(rawPins)
			if err != nil {
				return err
			}
			receipt, validationErr := validateLocalKits(cmd.Context(), args[0], stage, pins)
			encoder := json.NewEncoder(cmd.OutOrStdout())
			encoder.SetIndent("", "  ")
			if err := encoder.Encode(receipt); err != nil {
				return err
			}
			return validationErr
		},
	}
	command.Flags().StringVar(&stage, "stage", "verify", "stage to evaluate")
	command.Flags().StringArrayVar(&rawPins, "pin", nil, "authoritative kind:document_id:version (repeat; kind is requirement or system_design)")
	return command
}
func parseKitPins(raw []string) ([]verification.Pin, error) {
	pins := []verification.Pin{}
	seen := map[string]bool{}
	for _, s := range raw {
		parts := strings.Split(s, ":")
		if len(parts) != 3 {
			return nil, fmt.Errorf("--pin %q: expected kind:document_id:version", s)
		}
		version, err := strconv.Atoi(parts[2])
		if err != nil || version <= 0 || strings.TrimSpace(parts[1]) == "" || (parts[0] != verification.PinRequirement && parts[0] != verification.PinSystemDesign) {
			return nil, fmt.Errorf("--pin %q: require requirement/system_design, document ID and positive version", s)
		}
		key := parts[0] + ":" + parts[1]
		if seen[key] {
			return nil, fmt.Errorf("--pin %q: duplicate document identity", s)
		}
		seen[key] = true
		pins = append(pins, verification.Pin{Kind: parts[0], DocumentID: parts[1], Version: version})
	}
	return pins, nil
}
func localKitGit(ctx context.Context, root string, args ...string) ([]byte, error) {
	argv := append([]string{"--literal-pathspecs", "-c", "core.fsmonitor=false", "-C", root}, args...)
	cmd := exec.CommandContext(ctx, "git", argv...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	data, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("local git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return data, nil
}
func validateLocalKits(ctx context.Context, input, stage string, pins []verification.Pin) (verification.SelectionReceipt, error) {
	failed := func(err error) (verification.SelectionReceipt, error) {
		return verification.SelectionReceipt{SchemaVersion: 1, ContextPins: pins, Stage: stage, Kits: []verification.KitReceipt{}, Diagnostics: []verification.Diagnostic{{Path: input, Message: err.Error()}}}, err
	}
	manifestPath, err := filepath.Abs(input)
	if err != nil {
		return failed(err)
	}
	if st, err := os.Stat(manifestPath); err == nil && st.IsDir() {
		manifestPath = filepath.Join(manifestPath, ".conveyor/kits/manifest.yaml")
	}
	rootBytes, err := localKitGit(ctx, filepath.Dir(manifestPath), "rev-parse", "--show-toplevel")
	if err != nil {
		return failed(err)
	}
	root := strings.TrimSpace(string(rootBytes))
	checker, err := verification.FilesystemPathCheck(root)
	if err != nil {
		return failed(err)
	}
	relative, err := filepath.Rel(root, manifestPath)
	if err != nil {
		return failed(err)
	}
	if err = checker(".", filepath.ToSlash(relative)); err != nil {
		return failed(fmt.Errorf("manifest path: %w", err))
	}
	file, err := os.Open(manifestPath)
	if err != nil {
		return failed(err)
	}
	defer file.Close()
	manifestHash := sha256.New()
	m, parseErr := verification.Parse(io.TeeReader(file, manifestHash), checker)
	if m == nil {
		return failed(parseErr)
	}
	head, err := localKitGit(ctx, root, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return failed(err)
	}
	sha := strings.TrimSpace(string(head))
	trees := map[string][]verification.TreeEntry{}
	for i := range m.Kits {
		k := &m.Kits[i]
		if len(k.Diagnostics) > 0 {
			continue
		}
		entries, err := localKitTree(ctx, root, sha, k.Path)
		if err != nil {
			k.Diagnostics = append(k.Diagnostics, verification.Diagnostic{Path: fmt.Sprintf("manifest.kits[%d].path", i), Message: err.Error()})
			continue
		}
		trees[k.ID] = entries
	}
	receipt := verification.Evaluate(m, verification.SelectionContext{Pins: pins, Stage: stage, ManifestRevision: "working-tree:sha256:" + hex.EncodeToString(manifestHash.Sum(nil)), SourceRevision: sha}, trees)
	if receipt.Invalid() {
		return receipt, fmt.Errorf("kit manifest contains invalid entries")
	}
	return receipt, nil
}
func localKitTree(ctx context.Context, root, sha, kitPath string) ([]verification.TreeEntry, error) {
	// A current manifest may be a draft, but its kit digest must not mislabel
	// modified checkout bytes as the committed source tree's content.
	status, err := localKitGit(ctx, root, "status", "--porcelain=v1", "--untracked-files=all", "--ignored=matching", "--", kitPath)
	if err != nil {
		return nil, err
	}
	if len(status) != 0 {
		return nil, fmt.Errorf("kit %s has uncommitted files; commit before computing its source-tree digest", kitPath)
	}
	data, err := localKitGit(ctx, root, "ls-tree", "-r", "-z", "--full-tree", sha, "--", kitPath)
	if err != nil {
		return nil, err
	}
	entries := []verification.TreeEntry{}
	prefix := strings.TrimSuffix(filepath.ToSlash(filepath.Clean(kitPath)), "/") + "/"
	if kitPath == "." {
		prefix = ""
	}
	for _, record := range bytes.Split(data, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		parts := bytes.SplitN(record, []byte{'\t'}, 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid git tree record")
		}
		fields := strings.Fields(string(parts[0]))
		if len(fields) != 3 || fields[1] != "blob" {
			return nil, fmt.Errorf("kit %s contains a non-blob tree entry (submodules are unsupported)", kitPath)
		}
		name := string(parts[1])
		if !strings.HasPrefix(name, prefix) {
			return nil, fmt.Errorf("git tree path outside kit %s", kitPath)
		}
		entries = append(entries, verification.TreeEntry{Mode: fields[0], Path: strings.TrimPrefix(name, prefix), BlobOID: fields[2]})
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("kit %s has no committed tree entries", kitPath)
	}
	return entries, nil
}

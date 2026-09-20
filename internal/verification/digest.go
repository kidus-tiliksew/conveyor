package verification

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// TreeEntry uses a kit-relative Git path and the blob OID from the exact source
// tree, not a filesystem content hash. Gitlinks never expand scope (VK-3).
type TreeEntry struct {
	Mode    string `json:"mode"`
	Path    string `json:"path"`
	BlobOID string `json:"blob_oid"`
}

// NormalizeKit is the schema-1 digest encoding: compact UTF-8 JSON of the typed
// entry in declared field order, no trailing newline, preserving array order.
// YAML comments, key ordering and scalar spelling do not affect this encoding.
// Optional omitted fields are omitted; nil contract lists normalize to [].
func NormalizeKit(kit Kit) ([]byte, error) {
	// Round trip to own all slices before normalizing: callers retain their data.
	data, err := json.Marshal(kit)
	if err != nil {
		return nil, err
	}
	var k Kit
	if err = json.Unmarshal(data, &k); err != nil {
		return nil, err
	}
	if k.GoverningPins.Requirements == nil {
		k.GoverningPins.Requirements = []DocumentPin{}
	}
	if k.GoverningPins.SystemDesigns == nil {
		k.GoverningPins.SystemDesigns = []DocumentPin{}
	}
	if k.Exercises == nil {
		k.Exercises = []Exercise{}
	}
	for i := range k.Exercises {
		e := &k.Exercises[i]
		if e.Prerequisites == nil {
			e.Prerequisites = []Prerequisite{}
		}
		if e.Permissions == nil {
			e.Permissions = []Permission{}
		}
		if e.Inputs == nil {
			e.Inputs = []Input{}
		}
		if e.RequiredAssertions == nil {
			e.RequiredAssertions = []string{}
		}
		if e.Operations == nil {
			e.Operations = []Operation{}
		}
		if e.EvidenceOutputs == nil {
			e.EvidenceOutputs = []EvidenceOutput{}
		}
		if e.Supports == nil {
			e.Supports = []Support{}
		}
	}
	if k.UI != nil && k.UI.Assets == nil {
		k.UI.Assets = []string{}
	}
	return json.Marshal(k)
}

// ContentDigest is shared by forge-tree and checkout callers (VK-3). Hash input
// is NormalizeKit(k), LF, then each sorted '<mode> <path> <blob OID>\n' record.
// Record delimiters and validation make the byte stream unambiguous.
func ContentDigest(k Kit, entries []TreeEntry) (string, error) {
	if ds := validateKit(k, "kit", nil); len(ds) > 0 {
		return "", ds[0]
	}
	normalized, err := NormalizeKit(k)
	if err != nil {
		return "", err
	}
	sorted := append([]TreeEntry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	h := sha256.New()
	h.Write(normalized)
	h.Write([]byte{'\n'})
	for i, e := range sorted {
		if err := safePath(e.Path); err != nil {
			return "", fmt.Errorf("tree.%s: %w", e.Path, err)
		}
		if e.Path == "." || strings.HasPrefix(e.Path, "./") || strings.Contains(e.Path, "//") || strings.HasSuffix(e.Path, "/") {
			return "", fmt.Errorf("tree.%s: noncanonical path", e.Path)
		}
		if i > 0 && sorted[i-1].Path == e.Path {
			return "", fmt.Errorf("tree.%s: duplicate path", e.Path)
		}
		switch e.Mode {
		case "100644", "100755", "120000":
		default:
			return "", fmt.Errorf("tree.%s: unsupported mode %s (gitlinks are forbidden)", e.Path, e.Mode)
		}
		oid, err := hex.DecodeString(e.BlobOID)
		if err != nil || (len(oid) != 20 && len(oid) != 32) || strings.ToLower(e.BlobOID) != e.BlobOID {
			return "", fmt.Errorf("tree.%s: invalid blob OID", e.Path)
		}
		fmt.Fprintf(h, "%s %s %s\n", e.Mode, e.Path, e.BlobOID)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

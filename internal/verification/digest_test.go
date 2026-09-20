package verification

import (
	"strings"
	"testing"
)

func fixtureTree() []TreeEntry {
	return []TreeEntry{{"100755", "exercise.sh", strings.Repeat("a", 40)}, {"100644", "index.html", strings.Repeat("b", 40)}}
}
func TestDigestGolden(t *testing.T) {
	k := fixtureManifest(t).Kits[0]
	entries := fixtureTree()
	digest, err := ContentDigest(k, entries)
	if err != nil {
		t.Fatal(err)
	}
	const want = "c1dd0b42f99b43ff8fd144ae2c463f8c6ff050124750851d231563583a4b1243"
	if digest != want {
		t.Fatalf("digest = %s, want %s", digest, want)
	}
	entries[0], entries[1] = entries[1], entries[0]
	again, err := ContentDigest(k, entries)
	if err != nil || again != digest {
		t.Fatalf("ordering: %s %v", again, err)
	}
	for _, field := range []string{"mode", "path", "oid", "manifest"} {
		t.Run(field, func(t *testing.T) {
			tree := fixtureTree()
			kit := k
			switch field {
			case "mode":
				tree[0].Mode = "100644"
			case "path":
				tree[0].Path = "renamed.sh"
			case "oid":
				tree[0].BlobOID = strings.Repeat("c", 40)
			case "manifest":
				kit.Version = "2"
			}
			got, err := ContentDigest(kit, tree)
			if err != nil || got == digest {
				t.Fatalf("mutation not represented: %s %v", got, err)
			}
		})
	}
}
func TestDigestNormalizationAndRefusals(t *testing.T) {
	m := fixtureManifest(t)
	k := m.Kits[0]
	before, _ := NormalizeKit(k)
	input := "# comment\n" + strings.Replace(validManifest, "schema_version: 1", "schema_version: 0x1", 1)
	other, err := Parse(strings.NewReader(input), nil)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := NormalizeKit(other.Kits[0])
	if string(before) != string(after) {
		t.Fatal("YAML spelling changes digest")
	}
	for _, tc := range []struct {
		name string
		tree []TreeEntry
	}{
		{"gitlink", []TreeEntry{{"160000", "module", strings.Repeat("a", 40)}}},
		{"duplicate", []TreeEntry{fixtureTree()[0], fixtureTree()[0]}},
		{"traversal", []TreeEntry{{"100644", "../escape", strings.Repeat("a", 40)}}},
		{"record injection", []TreeEntry{{"100644", "a\nb", strings.Repeat("a", 40)}}},
		{"bad oid", []TreeEntry{{"100644", "a", "bad"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ContentDigest(k, tc.tree); err == nil {
				t.Fatal("accepted invalid tree")
			}
		})
	}
}

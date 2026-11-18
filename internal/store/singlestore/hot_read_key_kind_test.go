package singlestore

import "testing"

func TestHotReadIndexKeyKindMatchesEngine(t *testing.T) {
	hash := hotReadIndex{name: "h", hash: true}
	plain := hotReadIndex{name: "p"}
	cases := []struct {
		name        string
		index       hotReadIndex
		columnstore bool
		kind        string
		nonUnique   int
		want        bool
	}{
		{"columnstore hash target needs hash", hash, true, "COLUMNSTORE HASH", 1, true},
		{"columnstore hash target plain hash", hash, true, "HASH", 1, true},
		{"columnstore hash target rejects btree", hash, true, "BTREE", 1, false},
		{"rowstore hash target takes btree", hash, false, "BTREE", 1, true},
		{"rowstore hash target takes skiplist", hash, false, "SKIPLIST", 1, true},
		{"rowstore hash target rejects hash", hash, false, "HASH", 1, false},
		{"plain target on rowstore", plain, false, "BTREE", 1, true},
		{"unique key never matches", hash, true, "HASH", 0, false},
	}
	for _, tc := range cases {
		if got := hotReadIndexTypeMatches(tc.index, tc.columnstore, tc.kind, tc.nonUnique); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

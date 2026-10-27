package postgres

import (
	"regexp"
	"strconv"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// TestLineageRelationRankSQLMatchesCore holds the neighborhood CTE's relation
// CASE to core.LineageRelationPriority, the one Go ranking, for every
// projector-owned kind (component-lineage). It reads the SQL text and needs
// no database.
func TestLineageRelationRankSQLMatchesCore(t *testing.T) {
	sqlRanks := map[string]int{}
	for _, match := range regexp.MustCompile(`WHEN '([a-z_]+)' THEN (\d+)`).FindAllStringSubmatch(lineageRelationRankSQL, -1) {
		rank, _ := strconv.Atoi(match[2])
		if _, duplicate := sqlRanks[match[1]]; duplicate {
			t.Fatalf("SQL CASE ranks %q twice", match[1])
		}
		sqlRanks[match[1]] = rank
	}
	elseMatch := regexp.MustCompile(`ELSE (\d+) END`).FindStringSubmatch(lineageRelationRankSQL)
	if len(sqlRanks) == 0 || elseMatch == nil {
		t.Fatalf("relation CASE is not parseable: %s", lineageRelationRankSQL)
	}
	sqlDefault, _ := strconv.Atoi(elseMatch[1])
	if sqlDefault != core.LineageRelationDefaultPriority {
		t.Fatalf("SQL default rank=%d want %d", sqlDefault, core.LineageRelationDefaultPriority)
	}
	sqlRank := func(kind string) int {
		if rank, ok := sqlRanks[kind]; ok {
			return rank
		}
		return sqlDefault
	}
	owned := store.CanonicalLineageKinds()
	for kind := range owned {
		if got, want := sqlRank(kind), core.LineageRelationPriority(kind); got != want {
			t.Errorf("projector-owned kind %q: SQL rank %d, Go rank %d", kind, got, want)
		}
	}
	for kind, rank := range sqlRanks {
		if _, ok := owned[kind]; !ok {
			t.Errorf("SQL CASE ranks %q, which no projector case produces", kind)
		}
		if want := core.LineageRelationPriority(kind); rank != want {
			t.Errorf("SQL CASE ranks %q at %d, Go ranks it at %d", kind, rank, want)
		}
	}
	for _, kind := range core.LineageRankedRelationKinds() {
		if _, ok := owned[kind]; !ok {
			t.Errorf("Go ranking names %q, which no projector case produces", kind)
		}
		if _, ok := sqlRanks[kind]; !ok {
			t.Errorf("Go ranks %q but the SQL CASE falls through to its default", kind)
		}
	}
}

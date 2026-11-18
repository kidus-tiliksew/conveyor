package singlestore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"os"
	"regexp"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

var artifactLookupIndexes = []string{
	"artifact_links_artifact_lookup_idx", "artifact_links_task_lookup_idx",
	"artifact_links_requirement_lookup_idx", "artifact_links_session_lookup_idx",
}

// The predicates are pure text, so every layout is checked without a database.
func TestArtifactLinkPredicatesFollowEachLookupIndependently(t *testing.T) {
	layout := func(ready ...string) *Store {
		s := &Store{hotReadLayout: map[string]bool{}}
		for _, name := range ready {
			s.hotReadLayout[name] = true
		}
		return s
	}
	cases := []struct {
		name         string
		store        *Store
		artifactKey  bool
		ownerKeys    map[string]string
		ownerArgsFor map[string]int
	}{
		{"prepared", layout(artifactLookupIndexes...), true,
			map[string]string{"task_id": "task_lookup_key", "requirement_id": "requirement_lookup_key", "planning_session_id": "session_lookup_key"},
			map[string]int{"task_id": 2, "requirement_id": 2, "planning_session_id": 2}},
		{"legacy", layout(), false, map[string]string{}, map[string]int{"task_id": 1, "requirement_id": 1, "planning_session_id": 1}},
		{"partial", layout("artifact_links_task_lookup_idx", "artifact_links_artifact_lookup_idx"), true,
			map[string]string{"task_id": "task_lookup_key"},
			map[string]int{"task_id": 2, "requirement_id": 1, "planning_session_id": 1}},
		{"only session", layout("artifact_links_session_lookup_idx"), false,
			map[string]string{"planning_session_id": "session_lookup_key"},
			map[string]int{"task_id": 1, "requirement_id": 1, "planning_session_id": 2}},
	}
	for _, c := range cases {
		for _, exact := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s exact=%v", c.name, exact), func(t *testing.T) {
				original := func(left, right string) string {
					if exact {
						return "BINARY " + left + "=BINARY " + right
					}
					return left + "=" + right
				}
				id := c.store.artifactLinkIDMatch("l", "a.id", exact)
				if !strings.Contains(id, original("l.artifact_id", "a.id")) {
					t.Fatalf("artifact join lost its original equality: %s", id)
				}
				if got := strings.Contains(id, "l.artifact_lookup_key=SHA2(a.id,256)"); got != c.artifactKey {
					t.Fatalf("artifact digest use = %v, want %v: %s", got, c.artifactKey, id)
				}
				if strings.Contains(id, "SHA2(l.") {
					t.Fatalf("artifact join digests the stored column: %s", id)
				}
				for column, want := range c.ownerArgsFor {
					match := c.store.artifactLinkOwnerMatch("l", column, "?", exact)
					if got := strings.Count(match, "?"); got != want {
						t.Fatalf("%s placeholders = %d, want %d: %s", column, got, want, match)
					}
					if !strings.Contains(match, original("l."+column, "?")) {
						t.Fatalf("%s lost its original equality: %s", column, match)
					}
					key, prepared := c.ownerKeys[column]
					if got := strings.Contains(match, "l."+key+"=SHA2(?,256)"); prepared && !got {
						t.Fatalf("%s lacks digest equality: %s", column, match)
					}
					if !prepared && strings.Contains(match, "lookup_key") {
						t.Fatalf("%s references a missing generated column: %s", column, match)
					}
					if strings.Contains(match, "SHA2(COALESCE(") || strings.Contains(match, "SHA2(l.") {
						t.Fatalf("%s digests the stored column: %s", column, match)
					}
				}
			})
		}
	}

	// A chunk pairs its own digest and original value for every predicate, in
	// order, and an unprepared chunk carries no digest at all.
	prepared := layout(artifactLookupIndexes...)
	preds := []ownerPredicate{{"task_id", "t1"}, {"requirement_id", "r1"}, {"planning_session_id", "s1"}}
	query, args := prepared.artifactOwnerChunkQuery("ws", preds)
	if want := []any{"ws", "t1", "t1", "r1", "r1", "s1", "s1"}; !slices.Equal(args, want) || strings.Count(query, "?") != len(want) || strings.Count(query, " OR ") != 2 {
		t.Fatalf("prepared chunk args %v query %s", args, query)
	}
	query, args = layout().artifactOwnerChunkQuery("ws", preds)
	if want := []any{"ws", "t1", "r1", "s1"}; !slices.Equal(args, want) || strings.Contains(query, "SHA2") || strings.Contains(query, "lookup_key") {
		t.Fatalf("legacy chunk args %v query %s", args, query)
	}
}

func requireLegacyArtifactSQL(t *testing.T, name, query string) {
	t.Helper()
	for _, index := range hotReadIndexes {
		if index.column != "" && strings.Contains(query, index.column) {
			t.Errorf("%s legacy SQL references generated column %s: %s", name, index.column, query)
		}
	}
}

func TestArtifactLegacyBuildersAvoidGeneratedColumns(t *testing.T) {
	legacy := &Store{hotReadLayout: map[string]bool{}}
	query, _ := legacy.artifactHydrationQuery("workspace", []string{"artifact"})
	requireLegacyArtifactSQL(t, "hydration", query)
	owners := []ownerPredicate{{"task_id", "task"}, {"requirement_id", "requirement"}, {"planning_session_id", "session"}}
	for _, owner := range owners {
		query, _ := legacy.artifactOwnerChunkQuery("workspace", []ownerPredicate{owner})
		requireLegacyArtifactSQL(t, owner.column, query)
	}
	query, _ = legacy.artifactOwnerChunkQuery("workspace", owners)
	requireLegacyArtifactSQL(t, "mixed owners", query)
}

func TestArtifactProfileDecoderCountsActualTree(t *testing.T) {
	full := `Top
 Project [` + "`SUM(CRC32(artifact_id))`" + `] actual_rows: 1 exec_time: 0ms
 Aggregate actual_rows: 1 exec_time: 0ms
 ColumnStoreScan conveyor_test.artifact_links AS artifact_links, SORT KEY workspace_id (workspace_id) table_type:sharded_columnstore est_table_rows:51,072 est_filtered:51,072 actual_rows: 51,072 exec_time: 1ms`
	p := parseArtifactProfile(full)
	if p.baseInput != 51_072 || p.scanRows != 51_072 || p.filterInput != 0 {
		t.Fatalf("full scan parsed base=%d scans=%d filters_in=%d, want 51072/51072/0", p.baseInput, p.scanRows, p.filterInput)
	}

	hydration := `Top
 Gather partitions:single actual_rows: 101 exec_time: 0ms network_traffic: 1,024.5 KB
 Project [a.id] actual_rows: 101 exec_time: 0ms network_traffic: 18.231001 KB
 Sort [a.created_at, a.id, artifact_links.role] actual_rows: 101
 HashJoin type:left actual_rows: 101
 |---HashTableProbe [artifact_links.workspace_id = a.workspace_id AND artifact_links.expr = a.id]
 |   HashTableBuild alias:artifact_links actual_rows: 100
 |   Project [artifact_links_0.role] actual_rows: 100
 |   ColumnStoreFilter [artifact_links_0.artifact_lookup_key IN SHA2(?,?)] actual_rows: 100 total_rows_in: 20,936
 |   ColumnStoreScan conveyor_test.artifact_links AS artifact_links_0, SORT KEY workspace_id (workspace_id) table_type:sharded_columnstore actual_rows: 20,936
 ChoosePlan
 |   :estimate
 |---Filter [CAST(a.id AS BINARY) = CAST(? AS BINARY)] actual_rows: 100
 |   IndexRangeScan conveyor_test.artifacts AS a, PRIMARY KEY (workspace_id, id) actual_rows: 20,936`
	p = parseArtifactProfile(hydration)
	if p.baseInput != 20_936 || p.scanRows != 20_936 || p.filterInput != 20_936 {
		t.Fatalf("hydration parsed base=%d scans=%d filters_in=%d, want 20936 once, not 41872", p.baseInput, p.scanRows, p.filterInput)
	}
	if p.joinRows != 101 || p.filterOutput != 200 || p.networkKB != 1024.5 {
		t.Fatalf("hydration parsed joins=%d filter_output=%d network=%.3f, want 101/200/1024.5", p.joinRows, p.filterOutput, p.networkKB)
	}
	if p.baseInput == p.filterInput+p.scanRows {
		t.Fatalf("hydration double counted filter and scan: base=%d", p.baseInput)
	}
	for _, scan := range []string{"TableScan", "RowStoreScan", "IndexRangeScan"} {
		t.Run(scan, func(t *testing.T) {
			profile := parseArtifactProfile("Aggregate actual_rows: 1\n" +
				scan + " conveyor_test.artifact_links table_type:sharded_rowstore actual_rows: 51,072\n" +
				"IndexRangeScan conveyor_test.artifacts actual_rows: 20,936")
			if profile.baseInput != 51_072 || profile.scanRows != 51_072 {
				t.Fatalf("rowstore link input=%d scans=%d, want 51072 excluding artifact-table input", profile.baseInput, profile.scanRows)
			}
		})
	}
}

// artifactLinkRow is one physical artifact_links row as the seed created it.
// The owner fields mirror the persisted columns; an empty owner is NULL.
type artifactLinkRow struct {
	role        core.ArtifactRole
	task        string
	requirement string
	session     string
}

type artifactSeed struct {
	ctx                                  context.Context
	ws                                   string
	task, noise, caseUpper, caseLower    string
	requirement, session                 string
	orphan, shared, sessionArt, evidence string
	caseUpperArt, caseLowerArt           string
	equalAt                              time.Time
	// artifacts and links are the independent manifest for the selected
	// workspace. Expected association results are derived from them, not from
	// the new SQL helpers.
	artifacts map[string]core.Artifact
	links     map[string][]artifactLinkRow
}

// artifactOwnerIndex points at one physical link by owner value.
type artifactOwnerIndex struct {
	artifactID string
	link       artifactLinkRow
}

// artifactOracle answers what the selected workspace's artifacts and
// associations are without asking the database or the new builders.
type artifactOracle struct {
	artifacts map[string]core.Artifact
	links     map[string][]artifactLinkRow
	owners    map[string][]artifactOwnerIndex
}

func seedArtifactTasks(t *testing.T, st *Store, ctx context.Context, id string) string {
	t.Helper()
	ws, _ := store.WorkspaceFromContext(ctx)
	if err := st.CreateTask(ctx, core.Task{ID: id, Workspace: ws, Title: id, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/" + id, State: core.TaskQueued, NextStage: core.StageImplement}); err != nil {
		t.Fatal(err)
	}
	return id
}

func artifactPNG(t *testing.T, seed byte) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := png.Encode(&out, image.NewGray(image.Rect(0, 0, 1+int(seed)%7, 1))); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// seedArtifactAssociations builds every association shape the readers must
// keep: more owned rows than one page with equal created_at, noise, a
// duplicate content address under two owners, an orphan, case-twin owners,
// requirement and session owners, and verification evidence.
func seedArtifactAssociations(t *testing.T, st *Store) artifactSeed {
	t.Helper()
	ws := "artifact-hot-" + core.NewTaskID()
	ctx := documentPlanWorkspace(t, st, ws)
	suffix := core.NewTaskID()
	s := artifactSeed{ctx: ctx, ws: ws, equalAt: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC),
		artifacts: map[string]core.Artifact{}, links: map[string][]artifactLinkRow{}}
	s.task = seedArtifactTasks(t, st, ctx, "art-own-"+suffix)
	s.noise = seedArtifactTasks(t, st, ctx, "art-noise-"+suffix)
	s.caseUpper = seedArtifactTasks(t, st, ctx, "ArtCase-"+suffix)
	s.caseLower = seedArtifactTasks(t, st, ctx, "artcase-"+suffix)
	session, err := st.CreatePlanningSession(ctx, core.PlanningSession{ID: "art-session-" + suffix, Title: "Artifact session"})
	if err != nil {
		t.Fatal(err)
	}
	s.session = session.ID
	requirement, _, err := st.CreateRequirement(ctx, core.Requirement{ID: "art-req-" + suffix, Title: "Artifact requirement"}, core.RequirementVersion{
		Content: "# Artifact requirement", Origin: core.RequirementOriginChat, OriginSessionID: session.ID,
		Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Keep artifact reads exact."}},
	})
	if err != nil {
		t.Fatal(err)
	}
	s.requirement = requirement.ID
	create := func(a core.Artifact, body []byte) core.Artifact {
		t.Helper()
		if a.ContentType == "" {
			a.ContentType = "text/plain"
		}
		if a.CreatedAt.IsZero() {
			a.CreatedAt = s.equalAt
		}
		created, err := st.CreateArtifact(ctx, a, body)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := s.artifacts[created.ID]; !ok {
			s.artifacts[created.ID] = created
		}
		if created.TaskID != "" || created.RequirementID != "" || created.PlanningSessionID != "" {
			s.links[created.ID] = append(s.links[created.ID], artifactLinkRow{
				role: created.Role, task: created.TaskID,
				requirement: created.RequirementID, session: created.PlanningSessionID,
			})
		}
		return created
	}
	for i := range 110 {
		create(core.Artifact{Name: fmt.Sprintf("own-%03d.txt", i), TaskID: s.task}, []byte(fmt.Sprintf("own-%03d-%s", i, suffix)))
	}
	for i := range 20 {
		create(core.Artifact{Name: fmt.Sprintf("noise-%03d.txt", i), TaskID: s.noise}, []byte(fmt.Sprintf("noise-%03d-%s", i, suffix)))
	}
	s.caseUpperArt = create(core.Artifact{Name: "upper.txt", TaskID: s.caseUpper}, []byte("upper-"+suffix)).ID
	s.caseLowerArt = create(core.Artifact{Name: "lower.txt", TaskID: s.caseLower}, []byte("lower-"+suffix)).ID
	s.orphan = create(core.Artifact{Name: "orphan.txt"}, []byte("orphan-"+suffix)).ID
	shared := []byte("shared-" + suffix)
	s.shared = create(core.Artifact{Name: "shared.txt", TaskID: s.task}, shared).ID
	if again := create(core.Artifact{Name: "shared-req.txt", RequirementID: s.requirement}, shared); again.ID != s.shared {
		t.Fatalf("duplicate content address %s, want %s", again.ID, s.shared)
	}
	s.sessionArt = create(core.Artifact{Name: "session.txt", PlanningSessionID: s.session}, []byte("session-"+suffix)).ID
	s.evidence = create(core.Artifact{Name: "proof.png", ContentType: "image/png", TaskID: s.task, Role: core.ArtifactRoleVerificationEvidence}, artifactPNG(t, 3)).ID
	for name, id := range map[string]string{"orphan": s.orphan, "shared": s.shared, "session": s.sessionArt, "upper": s.caseUpperArt, "lower": s.caseLowerArt} {
		if name == "orphan" {
			if len(s.links[id]) != 0 {
				t.Fatalf("seed orphan %s has %d links, want 0", id, len(s.links[id]))
			}
			continue
		}
		want := 1
		if name == "shared" {
			want = 2
		}
		if got := len(s.links[id]); got != want {
			t.Fatalf("seed %s %s has %d links, want %d", name, id, got, want)
		}
	}
	return s
}

// artifactObservation is every reader's answer, as ordered association keys,
// so layouts are compared on rows and on public order.
func artifactObservation(t *testing.T, st *Store, seed artifactSeed) map[string][]string {
	t.Helper()
	ctx := seed.ctx
	keys := func(artifacts []core.Artifact, err error) []string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		out := make([]string, len(artifacts))
		for i, a := range artifacts {
			if a.DownloadURL != "" {
				t.Fatalf("metadata read set a download URL: %+v", a)
			}
			out[i] = fmt.Sprintf("%s|%s|%s|%s|%s|%s|%s|%d", a.ID, a.Name, a.Role, a.TaskID, a.RequirementID, a.PlanningSessionID, a.CreatedAt.UTC().Format(time.RFC3339Nano), a.SizeBytes)
		}
		return out
	}
	many := []string{seed.task, seed.task, "", seed.caseUpper}
	for i := range 130 {
		many = append(many, fmt.Sprintf("missing-%03d-%s", i, seed.ws))
	}
	many = append(many, seed.noise)
	obs := map[string][]string{
		"list":          keys(st.ListArtifacts(ctx)),
		"owners task":   keys(st.ListArtifactsForOwners(ctx, store.ArtifactOwners{TaskIDs: []string{seed.task, seed.task, ""}})),
		"owners upper":  keys(st.ListArtifactsForOwners(ctx, store.ArtifactOwners{TaskIDs: []string{seed.caseUpper}})),
		"owners lower":  keys(st.ListArtifactsForOwners(ctx, store.ArtifactOwners{TaskIDs: []string{seed.caseLower}})),
		"owners req":    keys(st.ListArtifactsForOwners(ctx, store.ArtifactOwners{RequirementIDs: []string{seed.requirement}})),
		"owners wrong":  keys(st.ListArtifactsForOwners(ctx, store.ArtifactOwners{TaskIDs: []string{strings.ToUpper(seed.task)}})),
		"owners mixed":  keys(st.ListArtifactsForOwners(ctx, store.ArtifactOwners{TaskIDs: many, RequirementIDs: []string{seed.requirement}, SessionIDs: []string{seed.session}})),
		"owners empty":  keys(st.ListArtifactsForOwners(ctx, store.ArtifactOwners{})),
		"owners none":   keys(st.ListArtifactsForOwners(ctx, store.ArtifactOwners{TaskIDs: []string{"missing-" + seed.ws}})),
		"lineage task":  keys(st.ListArtifactsForLineage(ctx, []core.LineageNode{{Type: core.LineageTask, ID: seed.task}})),
		"lineage req":   keys(st.ListArtifactsForLineage(ctx, []core.LineageNode{{Type: core.LineageRequirement, ID: seed.requirement}})),
		"lineage mixed": keys(st.ListArtifactsForLineage(ctx, []core.LineageNode{{Type: core.LineagePlanningSession, ID: seed.session}, {Type: core.LineageEvidence, ID: seed.evidence}, {Type: core.LineageTask, ID: seed.caseLower}, {Type: core.LineageTask, ID: seed.task}})),
		"lineage none":  keys(st.ListArtifactsForLineage(ctx, []core.LineageNode{{Type: core.LineageTask, ID: "missing-" + seed.ws}})),
	}
	got, _, err := st.GetArtifact(ctx, seed.shared)
	if err != nil {
		t.Fatal(err)
	}
	obs["get shared"] = keys([]core.Artifact{got}, nil)
	got, _, err = st.GetArtifact(ctx, seed.orphan)
	if err != nil {
		t.Fatal(err)
	}
	obs["get orphan"] = keys([]core.Artifact{got}, nil)
	got, _, err = st.GetArtifactForPlanningSession(ctx, seed.sessionArt, seed.session)
	if err != nil {
		t.Fatal(err)
	}
	obs["get session"] = keys([]core.Artifact{got}, nil)
	return obs
}

func requireArtifactContract(t *testing.T, obs map[string][]string, seed artifactSeed) {
	t.Helper()
	want := map[string]int{
		// 110 equal-created_at rows, the shared row and the evidence row.
		"owners task": 112, "owners upper": 1, "owners lower": 1, "owners req": 1, "owners wrong": 0,
		"owners empty": 0, "owners none": 0,
		// The task's 112 rows, one case-twin row, the 20 noise rows, the
		// requirement row, and the session row.
		"owners mixed": 112 + 1 + 20 + 1 + 1,
		"lineage task": 112, "lineage req": 1, "lineage none": 0,
		"get shared": 1, "get orphan": 1, "get session": 1,
	}
	for name, count := range want {
		if got := len(obs[name]); got != count {
			t.Errorf("%s returned %d rows, want %d", name, got, count)
		}
	}
	for _, name := range []string{"owners task", "owners mixed", "lineage task", "lineage mixed"} {
		seen := map[string]bool{}
		for _, key := range obs[name] {
			if seen[key] {
				t.Errorf("%s repeated %s", name, key)
			}
			seen[key] = true
		}
	}
	// The listing keeps one metadata row per association and one bare row per orphan.
	var orphans, shared int
	for _, key := range obs["list"] {
		switch {
		case strings.HasPrefix(key, seed.orphan+"|"):
			orphans++
			if !strings.Contains(key, "|task_context|||") {
				t.Errorf("orphan row %s", key)
			}
		case strings.HasPrefix(key, seed.shared+"|"):
			shared++
		}
	}
	if orphans != 1 || shared != 2 {
		t.Errorf("listing orphan rows %d shared rows %d, want 1 and 2", orphans, shared)
	}
	if len(obs["list"]) <= store.ArtifactMetadataPageSize {
		t.Errorf("listing returned %d rows, want more than one page", len(obs["list"]))
	}
	// Every seeded row shares one created_at, so the public order is by ID,
	// and the page seam never reorders.
	ids := make([]string, len(obs["list"]))
	for i, key := range obs["list"] {
		ids[i] = strings.SplitN(key, "|", 2)[0]
	}
	if !slices.IsSorted(ids) {
		t.Error("listing is not in created_at, id order")
	}
}

func setArtifactLayout(t *testing.T, st *Store, ready ...string) {
	t.Helper()
	previous := st.hotReadLayout
	layout := map[string]bool{}
	for _, name := range ready {
		layout[name] = true
	}
	st.hotReadLayout = layout
	t.Cleanup(func() { st.hotReadLayout = previous })
}

func diagnoseListArtifactsSQL(t *testing.T, st *Store, seed artifactSeed) {
	t.Helper()
	ctx := seed.ctx
	ws := documentWorkspace(ctx)
	run := func(name, query string, args ...any) {
		t.Helper()
		rows, err := st.db.QueryContext(ctx, query, args...)
		if err != nil {
			var me *mysql.MySQLError
			if errors.As(err, &me) {
				t.Fatalf("fixture %s: mysql %d sqlstate=%s message=%q", name, me.Number, string(me.SQLState[:]), me.Message)
			}
			t.Fatalf("fixture %s: %v", name, err)
		}
		defer rows.Close()
		for rows.Next() {
		}
		if err := rows.Err(); err != nil {
			var me *mysql.MySQLError
			if errors.As(err, &me) {
				t.Fatalf("fixture %s iterate: mysql %d sqlstate=%s message=%q", name, me.Number, string(me.SQLState[:]), me.Message)
			}
			t.Fatalf("fixture %s iterate: %v", name, err)
		}
	}
	fence := `SELECT created_at,id FROM artifacts WHERE workspace_id=? ORDER BY created_at DESC,id DESC LIMIT 1`
	run("list fence", fence, ws)
	var snapAt time.Time
	var snapID string
	if err := st.db.QueryRowContext(ctx, fence, ws).Scan(&snapAt, &snapID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return
		}
		var me *mysql.MySQLError
		if errors.As(err, &me) {
			t.Fatalf("fixture list fence scan: mysql %d sqlstate=%s message=%q", me.Number, string(me.SQLState[:]), me.Message)
		}
		t.Fatalf("fixture list fence scan: %v", err)
	}
	run("list first page", `SELECT id,workspace_id,name,content_type,size_bytes,created_at FROM artifacts WHERE workspace_id=? AND (created_at<? OR (created_at=? AND id<=?)) ORDER BY created_at,id LIMIT ?`, ws, snapAt, snapAt, snapID, store.ArtifactMetadataPageSize)
	run("list hydrate", `SELECT a.id,a.workspace_id,a.name,a.content_type,a.size_bytes,a.created_at,COALESCE(l.role,'task_context'),COALESCE(l.task_id,''),COALESCE(l.requirement_id,''),COALESCE(l.planning_session_id,'') FROM artifacts a LEFT JOIN artifact_links l ON l.workspace_id=a.workspace_id AND `+st.artifactLinkIDMatch("l", "a.id", true)+` WHERE a.workspace_id=? AND (BINARY a.id=BINARY ?) ORDER BY a.created_at,a.id,l.role`, ws, snapID)
}

const artifactPlanQueriedMin = 10000

// artifactPlanFixture is the plan workspace's independent seed manifest plus
// the owner lists the production chunk and diagnostic shapes need.
type artifactPlanFixture struct {
	seed                                         artifactSeed
	taskOwners, requirementOwners, sessionOwners []string
	heavyOwners                                  []string
}

func seedArtifactPlanLinks(t *testing.T, st *Store, seed artifactSeed) artifactPlanFixture {
	t.Helper()
	ctx := seed.ctx
	queried := seed.ws
	prefix := "plan-out-" + core.NewTaskID()
	otherWS := []string{prefix + "-a", prefix + "-b", prefix + "-c"}
	insertLinks := func(column string, rows [][3]string) {
		t.Helper()
		const batch = 200
		for start := 0; start < len(rows); start += batch {
			end := min(start+batch, len(rows))
			var b strings.Builder
			b.WriteString("INSERT INTO artifact_links(workspace_id,artifact_id,")
			b.WriteString(column)
			b.WriteString(",role) VALUES")
			args := make([]any, 0, (end-start)*4)
			for i, row := range rows[start:end] {
				if i > 0 {
					b.WriteByte(',')
				}
				b.WriteString("(?,?,?,?)")
				args = append(args, row[0], row[1], row[2], core.ArtifactRoleTaskContext)
			}
			if _, err := st.db.ExecContext(ctx, b.String(), args...); err != nil {
				t.Fatalf("plan link insert: %v", err)
			}
		}
	}
	insertArtifacts := func(rows [][3]string) {
		t.Helper()
		const batch = 200
		seen := map[string]bool{}
		var ids []string
		for _, row := range rows {
			if row[0] != queried || seen[row[1]] {
				continue
			}
			seen[row[1]] = true
			ids = append(ids, row[1])
		}
		for start := 0; start < len(ids); start += batch {
			end := min(start+batch, len(ids))
			var b strings.Builder
			b.WriteString("INSERT INTO artifacts(id,workspace_id,name,content_type,size_bytes,content,created_at) VALUES")
			args := make([]any, 0, (end-start)*7)
			for i, id := range ids[start:end] {
				if i > 0 {
					b.WriteByte(',')
				}
				b.WriteString("(?,?,?,?,0,?,?)")
				args = append(args, id, queried, "plan-noise", "text/plain", []byte{}, seed.equalAt)
			}
			if _, err := st.db.ExecContext(ctx, b.String(), args...); err != nil {
				t.Fatalf("plan artifact insert: %v", err)
			}
		}
	}
	fill := func(ws string, heavyN, heavyEach, lightN, lightEach, reqN, sessN, sessEach int) (tasks, reqs, sessions [][3]string) {
		for h := range heavyN {
			owner := fmt.Sprintf("%s-heavy-%d", ws, h)
			for i := range heavyEach {
				tasks = append(tasks, [3]string{ws, fmt.Sprintf("%s-h%d-%06d", ws, h, i), owner})
			}
		}
		for l := range lightN {
			owner := fmt.Sprintf("%s-light-%d", ws, l)
			for i := range lightEach {
				tasks = append(tasks, [3]string{ws, fmt.Sprintf("%s-l%d-%05d", ws, l, i), owner})
			}
		}
		reqOwner := ws + "-heavy-req"
		for i := range reqN {
			reqs = append(reqs, [3]string{ws, fmt.Sprintf("%s-r-%06d", ws, i), reqOwner})
		}
		for s := range sessN {
			owner := fmt.Sprintf("%s-sess-%d", ws, s)
			for i := range sessEach {
				sessions = append(sessions, [3]string{ws, fmt.Sprintf("%s-s%d-%04d", ws, s, i), owner})
			}
		}
		return tasks, reqs, sessions
	}
	qTasks, qReqs, qSessions := fill(queried, 2, 3000, 1200, 10, 2000, 80, 10)
	insertArtifacts(qTasks)
	insertArtifacts(qReqs)
	insertArtifacts(qSessions)
	insertLinks("task_id", qTasks)
	insertLinks("requirement_id", qReqs)
	insertLinks("planning_session_id", qSessions)
	plan := artifactPlanFixture{seed: seed}
	ownerSeen := map[string]bool{}
	addOwner := func(list *[]string, column, id string) {
		key := column + "\x00" + id
		if ownerSeen[key] {
			return
		}
		ownerSeen[key] = true
		*list = append(*list, id)
	}
	record := func(column string, rows [][3]string) {
		for _, row := range rows {
			if _, ok := seed.artifacts[row[1]]; !ok {
				seed.artifacts[row[1]] = core.Artifact{
					ID: row[1], Workspace: row[0], Name: "plan-noise", ContentType: "text/plain",
					SizeBytes: 0, CreatedAt: seed.equalAt, Role: core.ArtifactRoleTaskContext,
				}
			}
			link := artifactLinkRow{role: core.ArtifactRoleTaskContext}
			switch column {
			case "task_id":
				link.task = row[2]
				addOwner(&plan.taskOwners, column, row[2])
			case "requirement_id":
				link.requirement = row[2]
				addOwner(&plan.requirementOwners, column, row[2])
			case "planning_session_id":
				link.session = row[2]
				addOwner(&plan.sessionOwners, column, row[2])
			}
			seed.links[row[1]] = append(seed.links[row[1]], link)
		}
	}
	record("task_id", qTasks)
	record("requirement_id", qReqs)
	record("planning_session_id", qSessions)
	for _, owner := range plan.taskOwners {
		if strings.HasPrefix(owner, queried+"-heavy-") {
			plan.heavyOwners = append(plan.heavyOwners, owner)
		}
	}
	for _, ws := range otherWS {
		tasks, reqs, sessions := fill(ws, 2, 3000, 200, 20, 0, 0, 0)
		insertLinks("task_id", tasks)
		insertLinks("requirement_id", reqs)
		insertLinks("planning_session_id", sessions)
	}
	var queriedN, sharedN, distinctTasks, heavy, otherN int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM artifact_links WHERE workspace_id=?`, queried).Scan(&queriedN); err != nil {
		t.Fatal(err)
	}
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM artifact_links WHERE workspace_id=? AND artifact_id=?`, queried, seed.shared).Scan(&sharedN); err != nil {
		t.Fatal(err)
	}
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT task_id),COALESCE(MAX(cnt),0) FROM (SELECT task_id,COUNT(*) cnt FROM artifact_links WHERE workspace_id=? AND task_id IS NOT NULL GROUP BY task_id) owners`, queried).Scan(&distinctTasks, &heavy); err != nil {
		t.Fatal(err)
	}
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM artifact_links WHERE workspace_id LIKE ?`, prefix+"%").Scan(&otherN); err != nil {
		t.Fatal(err)
	}
	t.Logf("plan fixture queried workspace links=%d distinct_task_owners=%d heavy=%d shared_artifact_links=%d other_workspace_links=%d", queriedN, distinctTasks, heavy, sharedN, otherN)
	if queriedN < artifactPlanQueriedMin {
		t.Fatalf("queried workspace noise links=%d, want at least %d", queriedN, artifactPlanQueriedMin)
	}
	if distinctTasks < 1000 || heavy < 2000 {
		t.Fatalf("queried workspace skew task_owners=%d heavy=%d", distinctTasks, heavy)
	}
	if sharedN != 2 {
		t.Fatalf("hydration artifact %s has %d links, want 2", seed.shared, sharedN)
	}
	if otherN < 30000 {
		t.Fatalf("other workspace links=%d, want at least 30000", otherN)
	}
	if queriedN+otherN < 50000 {
		t.Fatalf("total fixture links=%d, want at least 50000", queriedN+otherN)
	}
	return plan
}

// newArtifactOracle indexes the seed manifest by physical owner. A link has
// exactly one owner, so a predicate finds every link with that owner without
// consulting artifact_links.
func newArtifactOracle(seed artifactSeed) artifactOracle {
	o := artifactOracle{
		artifacts: seed.artifacts, links: seed.links,
		owners: map[string][]artifactOwnerIndex{},
	}
	add := func(column, id, artifactID string, link artifactLinkRow) {
		if id == "" {
			return
		}
		key := column + "\x00" + id
		o.owners[key] = append(o.owners[key], artifactOwnerIndex{artifactID: artifactID, link: link})
	}
	for artifactID, links := range seed.links {
		for _, link := range links {
			add("task_id", link.task, artifactID, link)
			add("requirement_id", link.requirement, artifactID, link)
			add("planning_session_id", link.session, artifactID, link)
		}
	}
	return o
}

func (o artifactOracle) artifactRow(artifactID string, link artifactLinkRow) core.Artifact {
	row := o.artifacts[artifactID]
	row.Role = link.role
	row.TaskID = link.task
	row.RequirementID = link.requirement
	row.PlanningSessionID = link.session
	return row
}

// hydrationRows is the exact expected answer for one page: every physical
// link of every selected artifact plus the default row for an artifact with
// no link. Duplicate requested IDs do not duplicate rows.
func (o artifactOracle) hydrationRows(ids []string) []core.Artifact {
	seen := map[string]bool{}
	var rows []core.Artifact
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		links := o.links[id]
		if len(links) == 0 {
			rows = append(rows, o.artifactRow(id, artifactLinkRow{role: core.ArtifactRoleTaskContext}))
			continue
		}
		for _, link := range links {
			rows = append(rows, o.artifactRow(id, link))
		}
	}
	sortArtifactRows(rows)
	return rows
}

// ownerRows is the exact expected answer for an owner chunk: every physical
// link matching a predicate, once. Multiplicity across different links and
// repeated requested owner IDs is preserved as the database preserves it.
func (o artifactOracle) ownerRows(preds []ownerPredicate) []core.Artifact {
	seen := map[string]bool{}
	var rows []core.Artifact
	for _, pred := range preds {
		for _, ref := range o.owners[pred.column+"\x00"+pred.id] {
			key := artifactFullRowKey(o.artifactRow(ref.artifactID, ref.link))
			if seen[key] {
				continue
			}
			seen[key] = true
			rows = append(rows, o.artifactRow(ref.artifactID, ref.link))
		}
	}
	sortArtifactRows(rows)
	return rows
}

func sortArtifactRows(rows []core.Artifact) {
	slices.SortFunc(rows, func(a, b core.Artifact) int {
		if !a.CreatedAt.Equal(b.CreatedAt) {
			if a.CreatedAt.Before(b.CreatedAt) {
				return -1
			}
			return 1
		}
		if c := strings.Compare(a.ID, b.ID); c != 0 {
			return c
		}
		if c := strings.Compare(string(a.Role), string(b.Role)); c != 0 {
			return c
		}
		// The public order ties on role; sort the full physical row so the
		// two-link representative case is comparable without depending on
		// an unspecified join tie-break.
		return strings.Compare(artifactPhysicalKey(a), artifactPhysicalKey(b))
	})
}

func artifactPhysicalKey(a core.Artifact) string {
	return a.TaskID + "\x00" + a.RequirementID + "\x00" + a.PlanningSessionID
}

func artifactFullRowKey(a core.Artifact) string {
	return strings.Join([]string{
		a.ID, a.Workspace, a.Name, a.ContentType, fmt.Sprint(a.SizeBytes),
		a.CreatedAt.UTC().Format(time.RFC3339Nano), string(a.Role),
		a.TaskID, a.RequirementID, a.PlanningSessionID,
	}, "\x00")
}

// artifactLogicalCellBytes counts the ten projected columns under the same
// canonical driver representation used by the raw-cell seam: text as UTF-8,
// BIGINT size_bytes as its decimal string, DATETIME created_at as RFC3339Nano,
// and a NULL text column as zero bytes. workspace_id is part of the returned
// projection and must not be dropped from the oracle.
func artifactLogicalCellBytes(a core.Artifact) uint64 {
	return uint64(len(a.ID)+len(a.Workspace)+len(a.Name)+len(a.ContentType)) +
		uint64(len(strconv.FormatInt(a.SizeBytes, 10))) +
		uint64(len(a.CreatedAt.UTC().Format(time.RFC3339Nano))) +
		uint64(len(a.Role)+len(a.TaskID)+len(a.RequirementID)+len(a.PlanningSessionID))
}

func artifactCanonicalCells(a core.Artifact) [][]byte {
	return [][]byte{
		[]byte(a.ID), []byte(a.Workspace), []byte(a.Name), []byte(a.ContentType),
		[]byte(strconv.FormatInt(a.SizeBytes, 10)),
		[]byte(a.CreatedAt.UTC().Format(time.RFC3339Nano)),
		[]byte(a.Role), []byte(a.TaskID), []byte(a.RequirementID), []byte(a.PlanningSessionID),
	}
}

func artifactRowDigest(cells [][]byte) [sha256.Size]byte {
	sum := sha256.New()
	for _, cell := range cells {
		sum.Write([]byte(strconv.Itoa(len(cell))))
		sum.Write([]byte{':'})
		sum.Write(cell)
		sum.Write([]byte{0})
	}
	var out [sha256.Size]byte
	copy(out[:], sum.Sum(nil))
	return out
}

type artifactCellOracle struct {
	Rows       int
	CellBytes  uint64
	Statements uint64
	IDs        []string
	RowDigests [][sha256.Size]byte
	RowData    []artifactRawRow
}

func newArtifactCellOracle(rows []core.Artifact, statements uint64) artifactCellOracle {
	oracle := artifactCellOracle{Rows: len(rows), Statements: statements}
	oracle.IDs = make([]string, 0, len(rows))
	oracle.RowDigests = make([][sha256.Size]byte, 0, len(rows))
	for _, row := range rows {
		cells := artifactCanonicalCells(row)
		digest := artifactRowDigest(cells)
		oracle.CellBytes += artifactLogicalCellBytes(row)
		oracle.IDs = append(oracle.IDs, row.ID)
		oracle.RowDigests = append(oracle.RowDigests, digest)
		oracle.RowData = append(oracle.RowData, artifactRawRow{
			cells: cells, digest: digest, createdAt: row.CreatedAt,
			id: row.ID, role: string(row.Role),
			task: row.TaskID, requirement: row.RequirementID, session: row.PlanningSessionID,
		})
	}
	canonicalizeArtifactRawTies(oracle.RowData)
	oracle.IDs = make([]string, 0, len(oracle.RowData))
	oracle.RowDigests = make([][sha256.Size]byte, 0, len(oracle.RowData))
	oracle.CellBytes = 0
	for _, row := range oracle.RowData {
		oracle.IDs = append(oracle.IDs, row.id)
		oracle.RowDigests = append(oracle.RowDigests, row.digest)
		for _, cell := range row.cells {
			oracle.CellBytes += uint64(len(cell))
		}
	}
	return oracle
}

func artifactRowDigestsHash(digests [][sha256.Size]byte) string {
	sum := sha256.New()
	for _, digest := range digests {
		sum.Write(digest[:])
	}
	return fmt.Sprintf("%x", sum.Sum(nil))
}

func artifactLogicalBytes(rows []core.Artifact) uint64 {
	var total uint64
	for _, row := range rows {
		total += artifactLogicalCellBytes(row)
	}
	return total
}

type artifactRawMeasurement struct {
	Columns    int
	Rows       int
	CellBytes  uint64
	Statements uint64
	IDs        []string
	RowDigests [][sha256.Size]byte
	RowData    []artifactRawRow
}

type artifactRawRow struct {
	cells       [][]byte
	digest      [sha256.Size]byte
	createdAt   time.Time
	id          string
	role        string
	task        string
	requirement string
	session     string
}

func sortArtifactRawRows(rows []artifactRawRow) {
	slices.SortFunc(rows, func(a, b artifactRawRow) int {
		if !a.createdAt.Equal(b.createdAt) {
			if a.createdAt.Before(b.createdAt) {
				return -1
			}
			return 1
		}
		if c := strings.Compare(a.id, b.id); c != 0 {
			return c
		}
		if c := strings.Compare(a.role, b.role); c != 0 {
			return c
		}
		return strings.Compare(a.task+"\x00"+a.requirement+"\x00"+a.session, b.task+"\x00"+b.requirement+"\x00"+b.session)
	})
}

func canonicalizeArtifactRawTies(rows []artifactRawRow) {
	for start := 0; start < len(rows); {
		end := start + 1
		for end < len(rows) &&
			rows[end].createdAt.Equal(rows[start].createdAt) &&
			rows[end].id == rows[start].id &&
			rows[end].role == rows[start].role {
			end++
		}
		if end-start > 1 {
			slices.SortFunc(rows[start:end], func(a, b artifactRawRow) int {
				return strings.Compare(a.task+"\x00"+a.requirement+"\x00"+a.session, b.task+"\x00"+b.requirement+"\x00"+b.session)
			})
		}
		start = end
	}
}

func requireArtifactRawOuterOrder(t *testing.T, name string, rows []artifactRawRow) {
	t.Helper()
	for i := 1; i < len(rows); i++ {
		prev, cur := rows[i-1], rows[i]
		if cur.createdAt.Before(prev.createdAt) {
			t.Fatalf("%s raw outer order regressed at row %d: created_at %s before %s", name, i, cur.createdAt, prev.createdAt)
		}
		if cur.createdAt.Equal(prev.createdAt) && cur.id < prev.id {
			t.Fatalf("%s raw outer order regressed at row %d: id %s before %s", name, i, cur.id, prev.id)
		}
		if cur.createdAt.Equal(prev.createdAt) && cur.id == prev.id && cur.role < prev.role {
			t.Fatalf("%s raw outer order regressed at row %d: role %s before %s", name, i, cur.role, prev.role)
		}
	}
}

func rebuildArtifactRawMeasurement(measurement artifactRawMeasurement) artifactRawMeasurement {
	measurement.IDs = make([]string, 0, len(measurement.RowData))
	measurement.RowDigests = make([][sha256.Size]byte, 0, len(measurement.RowData))
	measurement.CellBytes = 0
	for _, row := range measurement.RowData {
		measurement.IDs = append(measurement.IDs, row.id)
		measurement.RowDigests = append(measurement.RowDigests, row.digest)
		for _, cell := range row.cells {
			measurement.CellBytes += uint64(len(cell))
		}
	}
	return measurement
}

func (m artifactRawMeasurement) Digest() string {
	return artifactRowDigestsHash(m.RowDigests)
}

func (o artifactCellOracle) Digest() string {
	return artifactRowDigestsHash(o.RowDigests)
}

// artifactObservedWork is the candidate's actual measured SQL execution:
// it drains the ten projected columns into sql.RawBytes and captures the raw
// measurement in the same call before typed reconstruction. The canonical
// accounting is explicit: text stays UTF-8, size_bytes is its decimal string,
// created_at is RFC3339Nano, and NULL is zero bytes.
func artifactObservedWork(t *testing.T, st *Store, ctx context.Context, build func(*Store) (string, []any), observed *[]artifactRawMeasurement) storetest.ReadEgressWork {
	return func(runCtx context.Context) (storetest.ReadEgressOutcome, error) {
		query, args := build(st)
		rows, err := st.db.QueryContext(runCtx, query, args...)
		if err != nil {
			return storetest.ReadEgressOutcome{}, err
		}
		artifacts, measurement := scanArtifactObservedRows(t, rows)
		*observed = append(*observed, measurement)
		return artifactReadOutcome(t, artifacts), nil
	}
}

func queryArtifactObserved(t *testing.T, st *Store, ctx context.Context, query string, args []any) ([]core.Artifact, artifactRawMeasurement) {
	t.Helper()
	rows, err := st.db.QueryContext(ctx, query, args...)
	if err != nil {
		t.Fatalf("observed artifact read: %v", err)
	}
	return scanArtifactObservedRows(t, rows)
}

func artifactObservedOwnerWork(t *testing.T, st *Store, ctx context.Context, ws string, preds []ownerPredicate, observed *[]artifactRawMeasurement) storetest.ReadEgressWork {
	return func(runCtx context.Context) (storetest.ReadEgressOutcome, error) {
		var artifacts []core.Artifact
		combined := artifactRawMeasurement{Columns: 10}
		for start := 0; start < len(preds); start += store.ArtifactMetadataPageSize {
			chunk := preds[start:min(start+store.ArtifactMetadataPageSize, len(preds))]
			query, args := st.artifactOwnerChunkQuery(ws, chunk)
			rows, err := st.db.QueryContext(runCtx, query, args...)
			if err != nil {
				return storetest.ReadEgressOutcome{}, err
			}
			part, measurement := scanArtifactObservedRows(t, rows)
			artifacts = append(artifacts, part...)
			combined.Rows += measurement.Rows
			combined.Statements += measurement.Statements
			combined.RowData = append(combined.RowData, measurement.RowData...)
		}
		sortArtifactRows(artifacts)
		sortArtifactRawRows(combined.RowData)
		combined = rebuildArtifactRawMeasurement(combined)
		*observed = append(*observed, combined)
		return artifactReadOutcome(t, artifacts), nil
	}
}

func scanArtifactObservedRows(t *testing.T, rows *sql.Rows) ([]core.Artifact, artifactRawMeasurement) {
	t.Helper()
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatalf("observed artifact columns: %v", err)
	}
	if len(columns) != 10 {
		t.Fatalf("observed artifact columns=%d, want exact 10: %v", len(columns), columns)
	}
	measurement := artifactRawMeasurement{Columns: len(columns), Statements: 1}
	var artifacts []core.Artifact
	for rows.Next() {
		raw := make([]sql.RawBytes, len(columns))
		dest := make([]any, len(columns))
		for i := range raw {
			dest[i] = &raw[i]
		}
		if err := rows.Scan(dest...); err != nil {
			t.Fatalf("observed artifact row %d scan: %v", measurement.Rows, err)
		}
		cells := artifactCanonicalRawCells(t, raw)
		row, createdAt, digest := artifactFromCanonicalCells(t, cells)
		measurement.Rows++
		for _, cell := range cells {
			measurement.CellBytes += uint64(len(cell))
		}
		measurement.IDs = append(measurement.IDs, row.ID)
		measurement.RowDigests = append(measurement.RowDigests, digest)
		measurement.RowData = append(measurement.RowData, artifactRawRow{
			cells: cells, digest: digest, createdAt: createdAt,
			id: row.ID, role: string(row.Role),
			task: row.TaskID, requirement: row.RequirementID, session: row.PlanningSessionID,
		})
		artifacts = append(artifacts, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("observed artifact rows: %v", err)
	}
	requireArtifactRawOuterOrder(t, "observed artifact", measurement.RowData)
	canonicalizeArtifactRawTies(measurement.RowData)
	measurement = rebuildArtifactRawMeasurement(measurement)
	return artifacts, measurement
}

func artifactFromCanonicalCells(t *testing.T, cells [][]byte) (core.Artifact, time.Time, [sha256.Size]byte) {
	t.Helper()
	if len(cells) != 10 {
		t.Fatalf("canonical artifact cells=%d, want 10", len(cells))
	}
	size, err := strconv.ParseInt(string(cells[4]), 10, 64)
	if err != nil {
		t.Fatalf("canonical size_bytes %q: %v", cells[4], err)
	}
	createdAt, err := time.Parse(time.RFC3339Nano, string(cells[5]))
	if err != nil {
		t.Fatalf("canonical created_at %q: %v", cells[5], err)
	}
	row := core.Artifact{
		ID: string(cells[0]), Workspace: string(cells[1]), Name: string(cells[2]),
		ContentType: string(cells[3]), SizeBytes: size, CreatedAt: createdAt,
		Role: core.ArtifactRole(cells[6]), TaskID: string(cells[7]),
		RequirementID: string(cells[8]), PlanningSessionID: string(cells[9]),
	}
	return row, createdAt, artifactRowDigest(cells)
}

func artifactBroadPageWork(t *testing.T, st *Store, ctx context.Context, ids []string) storetest.ReadEgressWork {
	wanted := make(map[string]bool, len(ids))
	for _, id := range ids {
		wanted[id] = true
	}
	return func(runCtx context.Context) (storetest.ReadEgressOutcome, error) {
		rows, err := st.db.QueryContext(runCtx, legacyArtifactListSQL, documentWorkspace(ctx))
		if err != nil {
			return storetest.ReadEgressOutcome{}, err
		}
		artifacts, err := scanArtifactSQLRows(rows)
		if err != nil {
			return storetest.ReadEgressOutcome{}, err
		}
		filtered := artifacts[:0]
		for _, row := range artifacts {
			if wanted[row.ID] {
				filtered = append(filtered, row)
			}
		}
		return artifactReadOutcome(t, filtered), nil
	}
}

func artifactCanonicalRawCells(t *testing.T, raw []sql.RawBytes) [][]byte {
	t.Helper()
	if len(raw) != 10 {
		t.Fatalf("raw artifact cells=%d, want 10", len(raw))
	}
	cells := make([][]byte, len(raw))
	for i, cell := range raw {
		if cell == nil {
			continue
		}
		switch i {
		case 4:
			size, err := strconv.ParseInt(string(cell), 10, 64)
			if err != nil {
				t.Fatalf("raw size_bytes %q: %v", cell, err)
			}
			cells[i] = []byte(strconv.FormatInt(size, 10))
		case 5:
			createdAt, err := parseArtifactRawTime(string(cell))
			if err != nil {
				t.Fatalf("raw created_at %q: %v", cell, err)
			}
			cells[i] = []byte(createdAt.UTC().Format(time.RFC3339Nano))
		default:
			cells[i] = []byte(string(cell))
		}
	}
	return cells
}

func parseArtifactRawTime(value string) (time.Time, error) {
	for _, layout := range []string{
		"2006-01-02 15:04:05.999999999 -0700 MST",
		"2006-01-02 15:04:05 -0700 MST",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
		time.RFC3339Nano,
	} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("unsupported time layout")
}

// artifactFirstPageIDs is the consumer's first metadata page: the same
// forward key fence and page order ListArtifacts uses, without hydration.
func artifactFirstPageIDs(t *testing.T, st *Store, ctx context.Context, ws string) []string {
	t.Helper()
	var snapAt time.Time
	var snapID string
	if err := st.db.QueryRowContext(ctx, `SELECT created_at,id FROM artifacts WHERE workspace_id=? ORDER BY created_at DESC,id DESC LIMIT 1`, ws).Scan(&snapAt, &snapID); err != nil {
		t.Fatal(err)
	}
	rows, err := st.db.QueryContext(ctx, `SELECT id,workspace_id,name,content_type,size_bytes,created_at FROM artifacts WHERE workspace_id=? AND (created_at<? OR (created_at=? AND id<=?)) ORDER BY created_at,id LIMIT ?`, ws, snapAt, snapAt, snapID, store.ArtifactMetadataPageSize)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id, workspace, name, contentType string
		var size int64
		var createdAt time.Time
		if err := rows.Scan(&id, &workspace, &name, &contentType, &size, &createdAt); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(ids) != store.ArtifactMetadataPageSize {
		t.Fatalf("first page has %d artifacts, want %d", len(ids), store.ArtifactMetadataPageSize)
	}
	return ids
}

func flushArtifactPlanLinks(t *testing.T, st *Store, ctx context.Context) {
	t.Helper()
	columnstore, err := st.hotReadTableColumnstore(ctx, "artifact_links")
	if err != nil {
		t.Fatal(err)
	}
	if columnstore {
		runArtifactPlanSQL(t, st, ctx, "OPTIMIZE TABLE artifact_links FLUSH")
	}
}

func runArtifactPlanSQL(t *testing.T, st *Store, ctx context.Context, statement string) {
	started := time.Now()
	rows, err := st.db.QueryContext(ctx, statement)
	if err != nil {
		t.Fatalf("%s: %v", statement, err)
	}
	for rows.Next() {
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		t.Fatalf("%s: %v", statement, err)
	}
	t.Logf("artifact plan preparation %q took %s", statement, time.Since(started).Round(time.Millisecond))
}

var (
	artifactPlanAnalyzeReco  = regexp.MustCompile(`(?i)ANALYZE TABLE\s+(?:[^\s.]+\.)?` + "`?" + `(artifact_links|artifacts)` + "`?" + `(?:\s+COLUMNS\s+(.+?))?(?:\s+ENABLE)?\s*;`)
	artifactPlanHistogramCol = regexp.MustCompile("(?i)(?:missing histogram|histogram on)\\s+`?(artifact_links|artifacts)`?\\.`?([A-Za-z0-9_]+)`?")
)

func artifactPlanStatsWarnings(text string) bool {
	u := strings.ToUpper(text)
	return strings.Contains(u, "AUTOSTATS_OUTDATED") || strings.Contains(u, "MISSING STATISTICS") || strings.Contains(u, "MISSING HISTOGRAM") || strings.Contains(u, "COLUMNS") && strings.Contains(u, "ENABLE") && strings.Contains(u, "WARNING")
}

func applyArtifactPlanAnalyzeRecos(t *testing.T, st *Store, ctx context.Context, text string) {
	t.Helper()
	seen := map[string]bool{}
	run := func(stmt string) {
		t.Helper()
		if seen[stmt] {
			return
		}
		seen[stmt] = true
		runArtifactPlanSQL(t, st, ctx, stmt)
	}
	for _, m := range artifactPlanAnalyzeReco.FindAllStringSubmatch(text, -1) {
		table := strings.ToLower(m[1])
		cols := strings.TrimSpace(m[2])
		if cols == "" {
			run("ANALYZE TABLE " + table)
			continue
		}
		var cleaned []string
		for _, col := range strings.Split(cols, ",") {
			col = strings.Trim(strings.TrimSpace(col), "`")
			if col == "" || col == "ENABLE" {
				continue
			}
			if regexp.MustCompile(`^[A-Za-z0-9_]+$`).MatchString(col) {
				cleaned = append(cleaned, col)
			}
		}
		if len(cleaned) == 0 {
			run("ANALYZE TABLE " + table)
			continue
		}
		run("ANALYZE TABLE " + table + " COLUMNS " + strings.Join(cleaned, ", ") + " ENABLE")
	}
	for _, m := range artifactPlanHistogramCol.FindAllStringSubmatch(text, -1) {
		run("ANALYZE TABLE " + strings.ToLower(m[1]) + " COLUMNS " + m[2] + " ENABLE")
	}
	if strings.Contains(strings.ToUpper(text), "AUTOSTATS_OUTDATED") {
		run("ANALYZE TABLE artifact_links")
		run("ANALYZE TABLE artifacts")
	}
}

func explainArtifactPlanWithWarnings(t *testing.T, st *Store, ctx context.Context, query string, args []any) string {
	t.Helper()
	conn, err := st.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	rows, err := conn.QueryContext(ctx, "EXPLAIN "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		lines = append(lines, strings.Split(line, "\n")...)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		t.Fatal(err)
	}
	warns, err := conn.QueryContext(ctx, "SHOW WARNINGS")
	if err == nil {
		defer warns.Close()
		for warns.Next() {
			var level, message string
			var code any
			if err := warns.Scan(&level, &code, &message); err != nil {
				t.Fatal(err)
			}
			lines = append(lines, level+": "+message)
		}
		if err := warns.Err(); err != nil {
			t.Fatal(err)
		}
	}
	return strings.Join(lines, "\n")
}

func prepareArtifactPlanMeasurement(t *testing.T, st *Store, ctx context.Context, statements map[string]func(*Store) (string, []any)) {
	t.Helper()
	flushArtifactPlanLinks(t, st, ctx)
	for _, statement := range []string{
		"OPTIMIZE TABLE artifact_links",
		"ANALYZE TABLE artifact_links",
		"ANALYZE TABLE artifacts",
	} {
		runArtifactPlanSQL(t, st, ctx, statement)
	}
	var segments, rows int64
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(rows_count),0) FROM information_schema.columnar_segments WHERE database_name=DATABASE() AND table_name='artifact_links' AND column_name='artifact_id'`).Scan(&segments, &rows); err != nil {
		t.Fatal(err)
	}
	t.Logf("artifact plan preparation artifact_links segments=%d rows=%d", segments, rows)
	deadline := time.Now().Add(45 * time.Second)
	var last string
	for {
		var b strings.Builder
		for name, build := range statements {
			query, args := build(st)
			plan := explainArtifactPlanWithWarnings(t, st, ctx, query, args)
			fmt.Fprintf(&b, "== %s ==\n%s\n", name, plan)
		}
		statsWarnings := artifactPlanStatsWarnings(b.String())
		var outdated int
		if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.OPTIMIZER_STATISTICS WHERE DATABASE_NAME=DATABASE() AND TABLE_NAME IN ('artifact_links','artifacts') AND AUTOSTATS_OUTDATED=1`).Scan(&outdated); err != nil {
			t.Logf("OPTIMIZER_STATISTICS: %v", err)
		} else {
			fmt.Fprintf(&b, "optimizer_statistics autostats_outdated_columns=%d\n", outdated)
		}
		last = b.String()
		if !statsWarnings && outdated == 0 {
			return
		}
		applyArtifactPlanAnalyzeRecos(t, st, ctx, last)
		if time.Now().After(deadline) {
			t.Fatalf("fixture stats blocker: AUTOSTATS_OUTDATED or missing histograms remain after ANALYZE COLUMNS ENABLE:\n%s", last)
		}
		time.Sleep(2 * time.Second)
	}
}

// artifactProfileOp is one executed profile operator block. The executed
// input rows of an artifact_links base access are either the scan's
// actual_rows or, when a pushed filter reports it, the filter's
// total_rows_in. Filter actual_rows is output, never substituted for input.
type artifactProfileOp struct {
	name           string
	block          string
	actualRows     uint64
	totalRowsIn    uint64
	hasTotalRowsIn bool
}

type artifactProfile struct {
	text         string
	baseInput    uint64
	scanRows     uint64
	filterInput  uint64
	filterOutput uint64
	joinRows     uint64
	networkKB    float64
	ops          []artifactProfileOp
}

var (
	artifactProfileOpStart     = regexp.MustCompile(`^\s*([A-Za-z][A-Za-z0-9_]*)\b`)
	artifactProfileActual      = regexp.MustCompile(`actual_rows:\s*([0-9][0-9,]*)`)
	artifactProfileTotalIn     = regexp.MustCompile(`total_rows_in:\s*([0-9][0-9,]*)`)
	artifactProfileNetwork     = regexp.MustCompile(`network_traffic:\s*([0-9][0-9.,]*)\s*KB`)
	artifactProfileFilterTable = regexp.MustCompile(`(?i)(artifact_links|l\.|artifact_lookup_key|task_lookup_key|requirement_lookup_key|session_lookup_key)`)
)

// artifactProfileOperators is the set that starts a new block in SHOW
// PROFILE text. Continuation lines keep their leading attribute names, so
// only these names begin an operator.
var artifactProfileOperators = map[string]bool{
	"Top": true, "Gather": true, "Project": true, "ColumnStoreFilter": true,
	"ColumnStoreScan": true, "HashJoin": true, "NestedLoopJoin": true,
	"Filter": true, "Join": true, "Aggregate": true, "Repartition": true,
	"Union": true, "Broadcast": true, "Distribute": true, "Limit": true,
	"Order": true, "Sort": true, "Insert": true, "Delete": true,
	"Update": true, "Result": true, "Remote": true, "Exchange": true,
	"Merge": true, "Materialize": true, "RowStoreScan": true, "TableScan": true,
	"HashTableProbe": true, "HashTableBuild": true, "ChoosePlan": true,
	"IndexRangeScan": true,
}

// parseArtifactProfile reads the executed text profile. It counts each
// artifact_links base access once: a filter's total_rows_in when present for
// that access, otherwise the scan's actual_rows. networkKB is the maximum
// per-operator network_traffic, not summed, so a movement reported at both a
// Project and its child is not double counted; it is never added to base
// input or client result bytes. Join movement is recorded separately.
func artifactProfileUint(raw string) uint64 {
	value, err := strconv.ParseUint(strings.ReplaceAll(raw, ",", ""), 10, 64)
	if err != nil {
		return 0
	}
	return value
}

func parseArtifactProfile(text string) artifactProfile {
	p := artifactProfile{text: text}
	var current *artifactProfileOp
	flush := func() {
		if current != nil {
			p.ops = append(p.ops, *current)
			current = nil
		}
	}
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		header := strings.TrimLeft(trimmed, "|+- \t")
		if m := artifactProfileOpStart.FindStringSubmatch(header); m != nil && artifactProfileOperators[m[1]] {
			flush()
			current = &artifactProfileOp{name: m[1], block: line}
		} else if current == nil {
			continue
		} else {
			current.block += "\n" + line
		}
		if m := artifactProfileActual.FindStringSubmatch(line); m != nil {
			current.actualRows = artifactProfileUint(m[1])
		}
		if m := artifactProfileTotalIn.FindStringSubmatch(line); m != nil {
			current.totalRowsIn = artifactProfileUint(m[1])
			current.hasTotalRowsIn = true
		}
	}
	flush()

	var pendingFilterInput uint64
	for _, op := range p.ops {
		switch op.name {
		case "ColumnStoreFilter", "Filter":
			if op.hasTotalRowsIn {
				p.filterInput += op.totalRowsIn
				if artifactProfileFilterTable.MatchString(op.block) && op.totalRowsIn > pendingFilterInput {
					pendingFilterInput = op.totalRowsIn
				}
			}
			p.filterOutput += op.actualRows
		case "ColumnStoreScan", "RowStoreScan", "TableScan", "IndexRangeScan":
			if !strings.Contains(op.block, "artifact_links") {
				continue
			}
			input := op.actualRows
			if pendingFilterInput > 0 {
				input = pendingFilterInput
				pendingFilterInput = 0
			}
			p.scanRows += op.actualRows
			p.baseInput += input
		case "HashJoin", "NestedLoopJoin", "Join":
			p.joinRows += op.actualRows
		}
		if m := artifactProfileNetwork.FindStringSubmatch(op.block); m != nil {
			if kb, err := strconv.ParseFloat(strings.ReplaceAll(m[1], ",", ""), 64); err == nil && kb > p.networkKB {
				p.networkKB = kb
			}
		}
	}
	return p
}

// profileArtifactStatement executes query under PROFILE on one connection
// and reads SHOW PROFILE from that connection, so the profile is the
// statement's own. PROFILE's ordinary result is drained but not recorded.
func profileArtifactStatement(t *testing.T, st *Store, ctx context.Context, query string, args []any) artifactProfile {
	t.Helper()
	conn, err := st.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	rows, err := conn.QueryContext(ctx, "PROFILE "+query, args...)
	if err != nil {
		t.Fatalf("PROFILE %s: %v", query, err)
	}
	for rows.Next() {
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		t.Fatalf("PROFILE %s iterate: %v", query, err)
	}
	profileRows, err := conn.QueryContext(ctx, "SHOW PROFILE")
	if err != nil {
		t.Fatalf("SHOW PROFILE after %s: %v", query, err)
	}
	var lines []string
	for profileRows.Next() {
		var line string
		if err := profileRows.Scan(&line); err != nil {
			profileRows.Close()
			t.Fatalf("SHOW PROFILE after %s scan: %v", query, err)
		}
		lines = append(lines, line)
	}
	err = profileRows.Err()
	profileRows.Close()
	if err != nil {
		t.Fatalf("SHOW PROFILE after %s iterate: %v", query, err)
	}
	if len(lines) == 0 {
		t.Fatalf("SHOW PROFILE after %s returned no tree rows", query)
	}
	text := strings.Join(lines, "\n")
	profile := parseArtifactProfile(text)
	t.Logf("artifact profile %q base_input=%d scans=%d filters_in=%d filters_out=%d joins=%d network_kb_max=%.6f",
		query, profile.baseInput, profile.scanRows, profile.filterInput, profile.filterOutput, profile.joinRows, profile.networkKB)
	t.Logf("artifact raw profile %q:\n%s", query, profile.text)
	return profile
}

// calibrateArtifactFullScan measures F, the executed artifact_links input
// ROWS for one full scan of the immutable representative fixture. The stored
// value checksum forces the engine to examine rows, not metadata. F is always
// the positive PROFILE row count, never a byte total or COUNT substitution;
// COUNT(*) below is only an independent fixture-cardinality sanity check.
func calibrateArtifactFullScan(t *testing.T, st *Store, ctx context.Context) uint64 {
	t.Helper()
	const query = `SELECT SUM(CRC32(artifact_id)) FROM artifact_links`
	profile := profileArtifactStatement(t, st, ctx, query, nil)
	F := profile.baseInput
	if F == 0 {
		F = profile.scanRows
	}
	if F == 0 {
		t.Fatalf("full-scan calibration found no executed artifact_links input rows:\n%s", profile.text)
	}
	var tableRows int64
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM artifact_links`).Scan(&tableRows); err != nil {
		t.Fatalf("full-scan calibration count: %v", err)
	}
	t.Logf("artifact full-scan calibration F=%d rows; actual table cardinality count(*)=%d", F, tableRows)
	if F != uint64(tableRows) {
		t.Fatalf("full-scan calibration F=%d does not match immutable fixture count(*)=%d", F, tableRows)
	}
	return F
}

// requireArtifactScanBudget applies the approved v3 fallback: for the two
// named artifact statements, the statement may scan only if its actual
// executed artifact_links input rows are at most 2F. EXPLAIN estimates and
// filter output rows are not used.
func requireArtifactScanBudget(t *testing.T, st *Store, ctx context.Context, name, query string, args []any, F uint64) artifactProfile {
	t.Helper()
	profile := profileArtifactStatement(t, st, ctx, query, args)
	if profile.baseInput == 0 {
		t.Fatalf("%s: no executed artifact_links input rows in profile:\n%s", name, profile.text)
	}
	if profile.baseInput > 2*F {
		t.Fatalf("%s: executed artifact_links input rows %d exceed 2F=%d (F=%d):\n%s", name, profile.baseInput, 2*F, F, profile.text)
	}
	t.Logf("%s: measured artifact_links input rows %d <= 2F=%d (F=%d), joins=%d, network_kb=%.3f",
		name, profile.baseInput, 2*F, F, profile.joinRows, profile.networkKB)
	return profile
}

func scanArtifactSQLRows(rows *sql.Rows) ([]core.Artifact, error) {
	defer rows.Close()
	var result []core.Artifact
	for rows.Next() {
		var artifact core.Artifact
		if err := rows.Scan(&artifact.ID, &artifact.Workspace, &artifact.Name, &artifact.ContentType, &artifact.SizeBytes, &artifact.CreatedAt, &artifact.Role, &artifact.TaskID, &artifact.RequirementID, &artifact.PlanningSessionID); err != nil {
			return nil, err
		}
		result = append(result, artifact)
	}
	return result, rows.Err()
}

func queryArtifactRows(t *testing.T, st *Store, ctx context.Context, query string, args []any) []core.Artifact {
	t.Helper()
	rows, err := st.db.QueryContext(ctx, query, args...)
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := scanArtifactSQLRows(rows)
	if err != nil {
		t.Fatal(err)
	}
	return artifacts
}

func queryArtifactBuild(t *testing.T, st *Store, ctx context.Context, build func(*Store) (string, []any)) []core.Artifact {
	t.Helper()
	query, args := build(st)
	return queryArtifactRows(t, st, ctx, query, args)
}

func requireArtifactBuildBudget(t *testing.T, st *Store, ctx context.Context, name string, build func(*Store) (string, []any), F uint64) artifactProfile {
	t.Helper()
	query, args := build(st)
	return requireArtifactScanBudget(t, st, ctx, name, query, args, F)
}

// artifactV2HydrationQuery is the bounded original hydration shape: the
// original raw page-ID and join equalities without a generated digest
// column. It is a reference for regressions, not the candidate.
func artifactV2HydrationQuery(workspace string, ids []string) (string, []any) {
	args := []any{workspace}
	clauses := make([]string, 0, len(ids))
	for _, id := range ids {
		clauses = append(clauses, "BINARY a.id=BINARY ?")
		args = append(args, id)
	}
	return `SELECT a.id,a.workspace_id,a.name,a.content_type,a.size_bytes,a.created_at,COALESCE(l.role,'task_context'),COALESCE(l.task_id,''),COALESCE(l.requirement_id,''),COALESCE(l.planning_session_id,'') FROM artifacts a LEFT JOIN artifact_links l ON l.workspace_id=a.workspace_id AND BINARY l.artifact_id=BINARY a.id WHERE a.workspace_id=? AND (` + strings.Join(clauses, " OR ") + `) ORDER BY a.created_at,a.id,l.role`, args
}

// artifactV2OwnerChunkQuery is the original owner-OR chunk shape: the
// original value equality only, no digest columns.
func artifactV2OwnerChunkQuery(workspace string, chunk []ownerPredicate) (string, []any) {
	args := []any{workspace}
	clauses := make([]string, 0, len(chunk))
	for _, pred := range chunk {
		clauses = append(clauses, "(BINARY l."+pred.column+"=BINARY ?)")
		args = append(args, pred.id)
	}
	return `SELECT a.id,a.workspace_id,a.name,a.content_type,a.size_bytes,a.created_at,COALESCE(l.role,'task_context'),COALESCE(l.task_id,''),COALESCE(l.requirement_id,''),COALESCE(l.planning_session_id,'') FROM artifact_links l JOIN artifacts a ON a.workspace_id=l.workspace_id AND a.id=l.artifact_id WHERE l.workspace_id=? AND (` + strings.Join(clauses, " OR ") + `) ORDER BY a.created_at,a.id,l.role`, args
}

const legacyArtifactListSQL = `SELECT a.id,a.workspace_id,a.name,a.content_type,a.size_bytes,a.created_at,COALESCE(l.role,'task_context'),COALESCE(l.task_id,''),COALESCE(l.requirement_id,''),COALESCE(l.planning_session_id,'') FROM artifacts a LEFT JOIN artifact_links l ON l.workspace_id=a.workspace_id AND l.artifact_id=a.id WHERE a.workspace_id=? ORDER BY a.created_at,a.id,l.role`

func artifactReadOutcome(t *testing.T, rows []core.Artifact) storetest.ReadEgressOutcome {
	t.Helper()
	keys := make([]string, len(rows))
	for i, row := range rows {
		keys[i] = artifactFullRowKey(row)
	}
	slices.Sort(keys)
	digest, err := storetest.ReadEgressDigest(keys)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(rows); i++ {
		if rows[i].CreatedAt.Before(rows[i-1].CreatedAt) {
			t.Fatalf("artifact public order regressed at row %d", i)
		}
	}
	return storetest.ReadEgressOutcome{Digest: digest, Successes: len(rows)}
}

func artifactSQLWork(t *testing.T, st *Store, ctx context.Context, build func(*Store) (string, []any)) storetest.ReadEgressWork {
	return func(ctx context.Context) (storetest.ReadEgressOutcome, error) {
		query, args := build(st)
		rows, err := st.db.QueryContext(ctx, query, args...)
		if err != nil {
			return storetest.ReadEgressOutcome{}, err
		}
		artifacts, err := scanArtifactSQLRows(rows)
		if err != nil {
			return storetest.ReadEgressOutcome{}, err
		}
		return artifactReadOutcome(t, artifacts), nil
	}
}

// artifactDurationSpread reports the warmup-excluded latency spread for one
// variant's measured trials.
func artifactDurationSpread(durations []time.Duration) (min, median, max time.Duration) {
	if len(durations) == 0 {
		return 0, 0, 0
	}
	sorted := append([]time.Duration(nil), durations...)
	slices.Sort(sorted)
	return sorted[0], sorted[len(sorted)/2], sorted[len(sorted)-1]
}

func timedArtifactWork(work storetest.ReadEgressWork, durations *[]time.Duration) storetest.ReadEgressWork {
	return func(ctx context.Context) (storetest.ReadEgressOutcome, error) {
		started := time.Now()
		outcome, err := work(ctx)
		*durations = append(*durations, time.Since(started))
		return outcome, err
	}
}

// measureArtifactRead runs one before/after comparison with one warmup and
// five interleaved trials, recording driver bytes, rows, statements, result
// digest, successful rows and latency spread for both variants. A nonempty
// result is required because the two artifact reads must serve work.
func measureArtifactRead(t *testing.T, st *Store, ctx context.Context, name string, baseline, candidate storetest.ReadEgressWork) storetest.ReadEgressReport {
	t.Helper()
	var baselineDurations, candidateDurations []time.Duration
	baseline = timedArtifactWork(baseline, &baselineDurations)
	candidate = timedArtifactWork(candidate, &candidateDurations)
	baselineRevision := artifactRevision("CONVEYOR_READ_EGRESS_BASELINE_REVISION", "bounded-v2-baseline")
	candidateRevision := artifactRevision("CONVEYOR_READ_EGRESS_CANDIDATE_REVISION", artifactRevision("CANDIDATE_REVISION", "ci-head-unavailable"))
	env := storetest.ReadEgressEnv{
		Backend:           "singlestore",
		CacheMode:         "none",
		BaselineRevision:  baselineRevision,
		CandidateRevision: candidateRevision,
		ReadStats:         st.ReadStats,
	}
	report, err := storetest.MeasureReadEgress(ctx, env, storetest.ReadEgressCase{
		Name: name, Baseline: baseline, Candidate: candidate,
	})
	if err != nil {
		t.Fatal(err)
	}
	b, c := report.Summary["baseline"], report.Summary["candidate"]
	t.Logf("read egress %s baseline bytes=%d rows=%d statements=%d; candidate bytes=%d rows=%d statements=%d; successes=%d",
		name, b.Bytes.Median, b.Rows.Median, b.Statements.Median,
		c.Bytes.Median, c.Rows.Median, c.Statements.Median, report.Successes)
	if raw, err := json.Marshal(report.Trials); err == nil {
		t.Logf("read egress trials %s json=%s", name, raw)
	}
	if raw, err := json.Marshal(report); err == nil {
		t.Logf("read egress report %s json=%s", name, raw)
	}
	if len(baselineDurations) > 1 && len(candidateDurations) > 1 {
		bmin, bmed, bmax := artifactDurationSpread(baselineDurations[1:])
		cmin, cmed, cmax := artifactDurationSpread(candidateDurations[1:])
		t.Logf("read latency %s baseline min/median/max=%s/%s/%s candidate=%s/%s/%s",
			name, bmin, bmed, bmax, cmin, cmed, cmax)
	}
	return report
}

func artifactBriefKeys(rows []core.Artifact) []string {
	out := make([]string, len(rows))
	for i, a := range rows {
		out[i] = fmt.Sprintf("%s|%s|%s|%s|%s|%s", a.ID, a.Role, a.TaskID, a.RequirementID, a.PlanningSessionID, a.CreatedAt.UTC().Format(time.RFC3339Nano))
	}
	return out
}

func requireArtifactPublicOrder(t *testing.T, name string, rows []core.Artifact) {
	t.Helper()
	for i := 1; i < len(rows); i++ {
		prev, row := rows[i-1], rows[i]
		if row.CreatedAt.Before(prev.CreatedAt) {
			t.Fatalf("%s row %d created_at %s is before row %d %s", name, i, row.CreatedAt, i-1, prev.CreatedAt)
		}
		if !row.CreatedAt.Equal(prev.CreatedAt) {
			continue
		}
		if row.ID < prev.ID {
			t.Fatalf("%s row %d id %s is before row %d %s", name, i, row.ID, i-1, prev.ID)
		}
		if row.ID == prev.ID && row.Role < prev.Role {
			t.Fatalf("%s row %d role %s is before row %d %s", name, i, row.Role, i-1, prev.Role)
		}
	}
}

func canonicalizeArtifactTies(rows []core.Artifact) {
	for start := 0; start < len(rows); {
		end := start + 1
		for end < len(rows) &&
			rows[end].CreatedAt.Equal(rows[start].CreatedAt) &&
			rows[end].ID == rows[start].ID &&
			rows[end].Role == rows[start].Role {
			end++
		}
		if end-start > 1 {
			slices.SortFunc(rows[start:end], func(a, b core.Artifact) int {
				return strings.Compare(artifactPhysicalKey(a), artifactPhysicalKey(b))
			})
		}
		start = end
	}
}

func artifactRevision(env, fallback string) string {
	if value := os.Getenv(env); value != "" {
		return value
	}
	if value := os.Getenv("GITHUB_SHA"); value != "" {
		return value
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" && setting.Value != "" {
				return setting.Value
			}
		}
	}
	return fallback
}

// measureArtifactReadChecked takes the independent seed-manifest cell-byte
// oracle B_expected before any baseline or candidate call. It measures the
// bounded equivalent baseline only for driver-accounting parity, never as
// B_expected. Every candidate trial must return exact R rows and its expected
// statement count; the actual driver bytes are retained separately from the
// typed seed cell-byte oracle.
type artifactParityMode uint8

const (
	artifactBoundedParity artifactParityMode = iota
	artifactBroadPageParity
)

func measureArtifactReadChecked(t *testing.T, st *Store, ctx context.Context, name string, baseline, candidate storetest.ReadEgressWork, mode artifactParityMode, expected []core.Artifact, expectedStatements uint64, observed *[]artifactRawMeasurement) storetest.ReadEgressReport {
	t.Helper()
	oracle := newArtifactCellOracle(expected, expectedStatements)
	report := measureArtifactRead(t, st, ctx, name, baseline, candidate)
	requireArtifactTrialParity(t, name, report, mode, oracle, *observed)
	return report
}

func requireArtifactTrialParity(t *testing.T, name string, report storetest.ReadEgressReport, mode artifactParityMode, oracle artifactCellOracle, observed []artifactRawMeasurement) {
	t.Helper()
	baseline := map[int]storetest.ReadEgressTrial{}
	for _, row := range report.Trials {
		if row.Variant == "baseline" {
			baseline[row.Trial] = row
		}
	}
	if len(observed) != 1+storetest.ReadEgressTrials {
		t.Fatalf("%s observed candidate calls=%d, want warmup plus %d trials", name, len(observed), storetest.ReadEgressTrials)
	}
	candidates := 0
	for _, row := range report.Trials {
		if row.Variant != "candidate" {
			continue
		}
		candidates++
		base, ok := baseline[row.Trial]
		if !ok {
			t.Fatalf("%s candidate trial %d has no baseline", name, row.Trial)
		}
		if row.Bytes == 0 {
			t.Fatalf("%s candidate trial %d reported no wire bytes", name, row.Trial)
		}
		if row.Rows != uint64(oracle.Rows) {
			t.Fatalf("%s candidate trial %d driver rows=%d, exact R=%d", name, row.Trial, row.Rows, oracle.Rows)
		}
		if row.Statements != oracle.Statements {
			t.Fatalf("%s candidate trial %d statements=%d, want %d", name, row.Trial, row.Statements, oracle.Statements)
		}
		switch mode {
		case artifactBoundedParity:
			if row.Bytes != base.Bytes {
				t.Fatalf("%s candidate trial %d wire bytes=%d, bounded baseline wire bytes=%d", name, row.Trial, row.Bytes, base.Bytes)
			}
			if row.Rows != base.Rows {
				t.Fatalf("%s candidate trial %d driver rows=%d, bounded baseline rows=%d", name, row.Trial, row.Rows, base.Rows)
			}
			if row.Statements != base.Statements {
				t.Fatalf("%s candidate trial %d statements=%d, bounded baseline=%d", name, row.Trial, row.Statements, base.Statements)
			}
		case artifactBroadPageParity:
			if row.Bytes > base.Bytes {
				t.Fatalf("%s candidate trial %d page wire bytes=%d, broad baseline wire bytes=%d", name, row.Trial, row.Bytes, base.Bytes)
			}
			if base.Rows <= row.Rows {
				t.Fatalf("%s candidate trial %d broad baseline rows=%d, want more than page rows=%d", name, row.Trial, base.Rows, row.Rows)
			}
			t.Logf("%s candidate trial %d: broad baseline rows=%d wire_bytes=%d; page rows=%d wire_bytes=%d",
				name, row.Trial, base.Rows, base.Bytes, row.Rows, row.Bytes)
		default:
			t.Fatalf("%s candidate trial %d has unknown parity mode %d", name, row.Trial, mode)
		}
		actual := observed[candidates]
		requireArtifactRawOracle(t, fmt.Sprintf("%s candidate trial %d", name, row.Trial), oracle, actual)
		t.Logf("%s candidate trial %d: B_expected_cell_bytes=%d exact_raw_cell_bytes=%d wire_bytes=%d rows=%d statements=%d raw_digest=%s",
			name, row.Trial, oracle.CellBytes, actual.CellBytes, row.Bytes, row.Rows, row.Statements, actual.Digest())
	}
	if candidates != storetest.ReadEgressTrials {
		t.Fatalf("%s candidate trials=%d, want %d", name, candidates, storetest.ReadEgressTrials)
	}
	t.Logf("%s: B_expected_cell_bytes=%d; exact raw cell bytes and wire bytes retained separately; candidate wire median=%d",
		name, oracle.CellBytes, report.Summary["candidate"].Bytes.Median)
}

func requireArtifactRawOracle(t *testing.T, name string, oracle artifactCellOracle, actual artifactRawMeasurement) {
	t.Helper()
	if actual.Columns != 10 {
		t.Fatalf("%s raw columns=%d, want 10", name, actual.Columns)
	}
	if actual.Rows != oracle.Rows {
		t.Fatalf("%s raw rows=%d, want R=%d", name, actual.Rows, oracle.Rows)
	}
	if actual.CellBytes != oracle.CellBytes {
		t.Fatalf("%s raw cell bytes=%d, want B_expected=%d", name, actual.CellBytes, oracle.CellBytes)
	}
	if actual.Statements != oracle.Statements {
		t.Fatalf("%s raw statements=%d, want %d", name, actual.Statements, oracle.Statements)
	}
	if !slices.Equal(actual.IDs, oracle.IDs) {
		t.Fatalf("%s raw ID order differs: got %v want %v", name, actual.IDs, oracle.IDs)
	}
	if actual.Digest() != oracle.Digest() {
		t.Fatalf("%s raw full-row digest=%s, want %s; %s", name, actual.Digest(), oracle.Digest(), firstArtifactRawDifference(actual.RowData, oracle.RowData))
	}
}

func firstArtifactRawDifference(actual, want []artifactRawRow) string {
	limit := min(len(actual), len(want))
	for row := range limit {
		columns := min(len(actual[row].cells), len(want[row].cells))
		for column := range columns {
			if !bytes.Equal(actual[row].cells[column], want[row].cells[column]) {
				return fmt.Sprintf("first difference row=%d id=%q role=%q column=%d got=%q want=%q",
					row, actual[row].id, actual[row].role, column,
					string(actual[row].cells[column]), string(want[row].cells[column]))
			}
		}
		if actual[row].id != want[row].id || actual[row].role != want[row].role ||
			!actual[row].createdAt.Equal(want[row].createdAt) {
			return fmt.Sprintf("first order difference row=%d got id=%q role=%q created=%s want id=%q role=%q created=%s",
				row, actual[row].id, actual[row].role, actual[row].createdAt,
				want[row].id, want[row].role, want[row].createdAt)
		}
	}
	if len(actual) != len(want) {
		return fmt.Sprintf("row count got=%d want=%d", len(actual), len(want))
	}
	return "rows and cells match but digest differs"
}

func requireArtifactRows(t *testing.T, name string, got, want []core.Artifact, expectedBytes uint64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s returned %d rows, want %d\n got=%v\nwant=%v", name, len(got), len(want), artifactBriefKeys(got), artifactBriefKeys(want))
	}
	requireArtifactPublicOrder(t, name, got)
	canonicalizeArtifactTies(got)
	for i := range want {
		if gotKey, wantKey := artifactFullRowKey(got[i]), artifactFullRowKey(want[i]); gotKey != wantKey {
			t.Fatalf("%s row %d differs:\n got %s\nwant %s", name, i, gotKey, wantKey)
		}
	}
	gotBytes := artifactLogicalBytes(got)
	if gotBytes > expectedBytes {
		t.Fatalf("%s logical cell bytes %d exceed B_expected=%d", name, gotBytes, expectedBytes)
	}
	if gotBytes != expectedBytes {
		t.Fatalf("%s logical cell bytes %d, want exactly B_expected=%d", name, gotBytes, expectedBytes)
	}
	for i := 1; i < len(got); i++ {
		if got[i].CreatedAt.Before(got[i-1].CreatedAt) {
			t.Fatalf("%s public order regressed at row %d", name, i)
		}
	}
}

func artifactOwnerPredicates(column string, ids []string) []ownerPredicate {
	preds := make([]ownerPredicate, 0, len(ids))
	for _, id := range ids {
		preds = append(preds, ownerPredicate{column, id})
	}
	return preds
}

func artifactOwnerSet(owners store.ArtifactOwners) map[string]bool {
	set := map[string]bool{}
	for _, id := range owners.TaskIDs {
		set["task_id\x00"+id] = true
	}
	for _, id := range owners.RequirementIDs {
		set["requirement_id\x00"+id] = true
	}
	for _, id := range owners.SessionIDs {
		set["planning_session_id\x00"+id] = true
	}
	return set
}

func artifactRowMatchesOwners(row core.Artifact, set map[string]bool) bool {
	return set["task_id\x00"+row.TaskID] ||
		set["requirement_id\x00"+row.RequirementID] ||
		set["planning_session_id\x00"+row.PlanningSessionID]
}

func addArtifactNoiseLinks(t *testing.T, st *Store, ctx context.Context, count int) string {
	t.Helper()
	ws := "artifact-v3-noise-" + core.NewTaskID()
	const batch = 500
	for start := 0; start < count; start += batch {
		end := min(start+batch, count)
		var b strings.Builder
		b.WriteString("INSERT INTO artifact_links(workspace_id,artifact_id,task_id,role) VALUES")
		args := make([]any, 0, (end-start)*4)
		for i := start; i < end; i++ {
			if i > start {
				b.WriteByte(',')
			}
			b.WriteString("(?,?,?,?)")
			args = append(args, ws, fmt.Sprintf("%s-art-%08d", ws, i), fmt.Sprintf("%s-owner-%04d", ws, i%97), core.ArtifactRoleTaskContext)
		}
		if _, err := st.db.ExecContext(ctx, b.String(), args...); err != nil {
			t.Fatal(err)
		}
	}
	return ws
}

// assertArtifactPageAndChunkStatements proves one hydration per nonempty
// metadata page and one owner statement per chunk, not one query per ID.
func assertArtifactPageAndChunkStatements(t *testing.T, st *Store, seed artifactSeed, taskOwners []string) {
	t.Helper()
	st.ReadStats(1000)
	calls := 0
	previous := store.ArtifactListPageHook
	store.ArtifactListPageHook = func() { calls++ }
	defer func() { store.ArtifactListPageHook = previous }()
	listed, err := st.ListArtifacts(seed.ctx)
	if err != nil {
		t.Fatal(err)
	}
	interval := st.ReadStats(1000)
	pages := calls - 1 // one call after the fence, one after each page's key read
	if pages <= 0 {
		t.Fatalf("listing observed %d page hook calls", calls)
	}
	if got := interval.Queries; got != uint64(1+2*pages) {
		t.Fatalf("listing issued %d statements, want 1 fence + %d pages + %d hydrations = %d (rows=%d bytes=%d)",
			got, pages, pages, 1+2*pages, interval.Rows, interval.Bytes)
	}
	if len(listed) == 0 || interval.Rows == 0 {
		t.Fatalf("listing served no work: rows=%d stats_rows=%d", len(listed), interval.Rows)
	}
	t.Logf("listing pages=%d hydration statements=%d total=%d", pages, pages, interval.Queries)

	owners := store.ArtifactOwners{TaskIDs: append([]string(nil), taskOwners[:250]...)}
	st.ReadStats(1000)
	if _, err := st.ListArtifactsForOwners(seed.ctx, owners); err != nil {
		t.Fatal(err)
	}
	interval = st.ReadStats(1000)
	chunks := (len(store.ArtifactOwnerIDs(owners.TaskIDs)) + store.ArtifactMetadataPageSize - 1) / store.ArtifactMetadataPageSize
	if got := interval.Queries; got != uint64(chunks) {
		t.Fatalf("owner read issued %d statements, want %d chunks", got, chunks)
	}
	t.Logf("owner chunks=%d statements=%d", chunks, interval.Queries)

	st.ReadStats(1000)
	if _, err := st.ListArtifactsForOwners(seed.ctx, store.ArtifactOwners{}); err != nil {
		t.Fatal(err)
	}
	interval = st.ReadStats(1000)
	if interval.Queries != 0 || interval.Rows != 0 || interval.Bytes != 0 {
		t.Fatalf("empty owner read issued work: statements=%d rows=%d bytes=%d", interval.Queries, interval.Rows, interval.Bytes)
	}

	st.ReadStats(1000)
	if _, err := st.hydrateArtifactAssociations(seed.ctx, nil); err != nil {
		t.Fatal(err)
	}
	interval = st.ReadStats(1000)
	if interval.Queries != 0 || interval.Rows != 0 || interval.Bytes != 0 {
		t.Fatalf("empty hydration issued work: statements=%d rows=%d bytes=%d", interval.Queries, interval.Rows, interval.Bytes)
	}
}

// TestArtifactHotReadLayoutsIntegration runs the real readers on SingleStore
// under each layout and requires the same rows in the same order, then
// checks the plans take the prepared lookup only when it is prepared.
func TestArtifactHotReadLayoutsIntegration(t *testing.T) {
	st := integrationStore(t)
	exerciseArtifactHotReadLayouts(t, st, sharedDatabase.cfg.FormatDSN())
}

func TestArtifactHotReadRowstoreLayoutsIntegration(t *testing.T) {
	db, dsn := hotReadIntegrationDatabase(t, false)
	ctx := t.Context()
	// An operator-owned rowstore is a supported legacy layout. Preparing it
	// adds the same lookups without changing its engine.
	if _, err := db.ExecContext(ctx, `CREATE ROWSTORE TABLE artifact_links (
		workspace_id VARCHAR(255) NOT NULL,
		artifact_id LONGTEXT NOT NULL,
		task_id LONGTEXT,
		role LONGTEXT NOT NULL DEFAULT 'task_context',
		requirement_id LONGTEXT,
		planning_session_id LONGTEXT,
		SHARD KEY(workspace_id)
	) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin`); err != nil {
		t.Fatal(err)
	}
	legacy, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	legacy.Close()
	applySSPrepareScript(t, ctx, db, loadHotReadPrepareScript(t, "prepare-singlestore.sql", "rowstore-proof", "rowstore-task"))
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if columnstore, err := st.hotReadTableColumnstore(ctx, "artifact_links"); err != nil || columnstore {
		t.Fatalf("artifact_links engine changed: columnstore=%v err=%v", columnstore, err)
	}
	exerciseArtifactHotReadLayouts(t, st, dsn)
}

func exerciseArtifactHotReadLayouts(t *testing.T, st *Store, dsn string) {
	t.Helper()
	for _, name := range artifactLookupIndexes {
		if !st.hotReadIndexReady(name) {
			t.Fatalf("%s is not prepared", name)
		}
	}
	seed := seedArtifactAssociations(t, st)
	diagnoseListArtifactsSQL(t, st, seed)
	prepared := artifactObservation(t, st, seed)
	requireArtifactContract(t, prepared, seed)

	layouts := []struct {
		name  string
		ready []string
	}{
		{"legacy", nil},
		{"artifact only", []string{"artifact_links_artifact_lookup_idx"}},
		{"task only", []string{"artifact_links_task_lookup_idx"}},
		{"requirement and session", []string{"artifact_links_requirement_lookup_idx", "artifact_links_session_lookup_idx"}},
	}
	for _, layout := range layouts {
		t.Run(layout.name, func(t *testing.T) {
			setArtifactLayout(t, st, layout.ready...)
			got := artifactObservation(t, st, seed)
			requireArtifactContract(t, got, seed)
			for name, want := range prepared {
				if !slices.Equal(got[name], want) {
					t.Errorf("%s differs from the prepared layout:\n got %v\nwant %v", name, got[name], want)
				}
			}
		})
	}

	t.Run("reopen", func(t *testing.T) {
		reopened, err := Open(t.Context(), dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(reopened.Close)
		for _, name := range artifactLookupIndexes {
			if !reopened.hotReadIndexReady(name) {
				t.Fatalf("%s not ready after reopen", name)
			}
		}
		got := artifactObservation(t, reopened, seed)
		for name, want := range prepared {
			if !slices.Equal(got[name], want) {
				t.Errorf("%s differs after reopen:\n got %v\nwant %v", name, got[name], want)
			}
		}
	})

	planSeed := seedArtifactAssociations(t, st)
	plan := seedArtifactPlanLinks(t, st, planSeed)

	t.Run("plans", func(t *testing.T) {
		pageIDs := artifactFirstPageIDs(t, st, planSeed.ctx, planSeed.ws)
		preds := make([]ownerPredicate, 0, store.ArtifactMetadataPageSize)
		for _, owner := range plan.taskOwners[:store.ArtifactMetadataPageSize] {
			preds = append(preds, ownerPredicate{"task_id", owner})
		}
		diagnostic := make([]ownerPredicate, 0, 128)
		for _, owner := range plan.taskOwners[:128] {
			diagnostic = append(diagnostic, ownerPredicate{"task_id", owner})
		}
		statements := map[string]func(*Store) (string, []any){
			"owner OR chunk": func(s *Store) (string, []any) { return s.artifactOwnerChunkQuery(planSeed.ws, preds) },
			"owner 128 diagnostic": func(s *Store) (string, []any) {
				return s.artifactOwnerChunkQuery(planSeed.ws, diagnostic)
			},
			"hydration": func(s *Store) (string, []any) { return s.artifactHydrationQuery(planSeed.ws, pageIDs) },
		}
		prepareArtifactPlanMeasurement(t, st, planSeed.ctx, statements)
		F := calibrateArtifactFullScan(t, st, planSeed.ctx)
		indexOf := map[string]string{"owner OR chunk": "artifact_links_task_lookup_idx", "owner 128 diagnostic": "artifact_links_task_lookup_idx", "hydration": "artifact_links_artifact_lookup_idx"}
		for name, build := range statements {
			query, args := build(st)
			evidenced := strings.Join(explainDocumentEventQuery(t, st, planSeed.ctx, query, args), "\n")
			t.Logf("%s prepared plan names %s=%v (plan text is evidence only, not the acceptance)",
				name, indexOf[name], strings.Contains(evidenced, indexOf[name]))
			requireArtifactScanBudget(t, st, planSeed.ctx, name, query, args, F)
			setArtifactLayout(t, st)
			query, args = build(st)
			requireLegacyArtifactSQL(t, name, query)
			setArtifactLayout(t, st, artifactLookupIndexes...)
		}
	})

	t.Run("artifact v3 measured access", func(t *testing.T) {
		if len(plan.taskOwners) < 128 || len(plan.heavyOwners) == 0 {
			t.Fatalf("plan fixture owners task=%d heavy=%d", len(plan.taskOwners), len(plan.heavyOwners))
		}
		for _, name := range artifactLookupIndexes {
			if !st.hotReadIndexReady(name) {
				t.Fatalf("%s is not prepared", name)
			}
		}
		var version string
		if err := st.db.QueryRowContext(planSeed.ctx, `SELECT @@memsql_version`).Scan(&version); err != nil {
			t.Fatal(err)
		}
		var totalLinks, workspaceLinks int
		if err := st.db.QueryRowContext(planSeed.ctx, `SELECT COUNT(*) FROM artifact_links`).Scan(&totalLinks); err != nil {
			t.Fatal(err)
		}
		if err := st.db.QueryRowContext(planSeed.ctx, `SELECT COUNT(*) FROM artifact_links WHERE workspace_id=?`, planSeed.ws).Scan(&workspaceLinks); err != nil {
			t.Fatal(err)
		}
		t.Logf("artifact v3 proof engine=%s fixture total_links=%d selected_workspace_links=%d artifacts=%d; transport metric: driver receive bytes (transport, incl. framing); canonical SQL cell bytes and driver rows/statements reported separately; billing interpretation unvetted",
			version, totalLinks, workspaceLinks, len(planSeed.artifacts))
		oracle := newArtifactOracle(planSeed)
		pageIDs := artifactFirstPageIDs(t, st, planSeed.ctx, planSeed.ws)
		pageExpected := oracle.hydrationRows(pageIDs)
		for _, row := range pageExpected {
			if row.Workspace != planSeed.ws {
				t.Fatalf("seed page oracle row %s workspace=%q, want %q", row.ID, row.Workspace, planSeed.ws)
			}
		}
		pageBytes := artifactLogicalBytes(pageExpected)
		if len(pageExpected) < len(pageIDs) {
			t.Fatalf("page oracle has %d rows for %d artifacts", len(pageExpected), len(pageIDs))
		}
		t.Logf("independent page oracle rows=%d logical_cell_bytes=%d artifacts=%d", len(pageExpected), pageBytes, len(pageIDs))

		v2Hydration := func(*Store) (string, []any) { return artifactV2HydrationQuery(planSeed.ws, pageIDs) }
		v3Hydration := func(s *Store) (string, []any) { return s.artifactHydrationQuery(planSeed.ws, pageIDs) }
		v2Rows := queryArtifactBuild(t, st, planSeed.ctx, v2Hydration)
		requireArtifactRows(t, "hydration v2", v2Rows, pageExpected, pageBytes)
		v3Rows := queryArtifactBuild(t, st, planSeed.ctx, v3Hydration)
		requireArtifactRows(t, "hydration v3 builder", v3Rows, pageExpected, pageBytes)

		st.ReadStats(1000)
		methodRows, err := st.hydrateArtifactAssociations(planSeed.ctx, pageIDs)
		if err != nil {
			t.Fatal(err)
		}
		interval := st.ReadStats(1000)
		requireArtifactRows(t, "hydration production method", methodRows, pageExpected, pageBytes)
		if interval.Queries != 1 {
			t.Fatalf("one page hydration issued %d statements, want 1", interval.Queries)
		}

		// A selected one-artifact page with two physical links returns
		// exactly two complete rows.
		sharedExpected := oracle.hydrationRows([]string{planSeed.shared})
		if len(sharedExpected) != 2 {
			t.Fatalf("shared artifact oracle rows=%d, want 2", len(sharedExpected))
		}
		sharedBytes := artifactLogicalBytes(sharedExpected)
		sharedQuery, sharedArgs := st.artifactHydrationQuery(planSeed.ws, []string{planSeed.shared})
		sharedRows := queryArtifactRows(t, st, planSeed.ctx, sharedQuery, sharedArgs)
		requireArtifactRows(t, "hydration shared two-link", sharedRows, sharedExpected, sharedBytes)

		// Duplicate requested page IDs do not duplicate rows or bytes.
		duplicateIDs := append(append([]string(nil), pageIDs...), pageIDs...)
		duplicateExpected := oracle.hydrationRows(duplicateIDs)
		duplicateQuery, duplicateArgs := st.artifactHydrationQuery(planSeed.ws, duplicateIDs)
		requireArtifactRows(t, "hydration duplicate page IDs", queryArtifactRows(t, st, planSeed.ctx, duplicateQuery, duplicateArgs), duplicateExpected, artifactLogicalBytes(duplicateExpected))

		// Orphan, session, case-twin and multi-link artifacts in one call.
		mixedIDs := []string{planSeed.orphan, planSeed.shared, planSeed.sessionArt, planSeed.caseUpperArt, planSeed.caseLowerArt}
		mixedExpected := oracle.hydrationRows(mixedIDs)
		mixedQuery, mixedArgs := st.artifactHydrationQuery(planSeed.ws, mixedIDs)
		mixedActual := queryArtifactRows(t, st, planSeed.ctx, mixedQuery, mixedArgs)
		t.Logf("mixed hydration ids=%v expected=%v actual=%v", mixedIDs, artifactBriefKeys(mixedExpected), artifactBriefKeys(mixedActual))
		for _, id := range mixedIDs {
			singleQuery, singleArgs := st.artifactHydrationQuery(planSeed.ws, []string{id})
			single := queryArtifactRows(t, st, planSeed.ctx, singleQuery, singleArgs)
			t.Logf("mixed hydration id=%s rows=%d keys=%v", id, len(single), artifactBriefKeys(single))
		}
		requireArtifactRows(t, "hydration orphan/session/case", mixedActual, mixedExpected, artifactLogicalBytes(mixedExpected))

		F := calibrateArtifactFullScan(t, st, planSeed.ctx)
		requireArtifactBuildBudget(t, st, planSeed.ctx, "hydration page", v3Hydration, F)
		requireArtifactScanBudget(t, st, planSeed.ctx, "hydration shared two-link", sharedQuery, sharedArgs, F)

		// Owner chunk shapes: production 100-owner chunk, 128-owner
		// diagnostic, heavy owner, mixed owners, no match and empty input.
		owners100 := store.ArtifactOwners{TaskIDs: append([]string(nil), plan.taskOwners[:100]...)}
		preds100 := artifactOwnerPredicates("task_id", owners100.TaskIDs)
		expected100 := oracle.ownerRows(preds100)
		ownerV2Query, ownerV2Args := artifactV2OwnerChunkQuery(planSeed.ws, preds100)
		requireArtifactRows(t, "owner v2 100", queryArtifactRows(t, st, planSeed.ctx, ownerV2Query, ownerV2Args), expected100, artifactLogicalBytes(expected100))
		ownerV3Query, ownerV3Args := st.artifactOwnerChunkQuery(planSeed.ws, preds100)
		requireArtifactRows(t, "owner v3 100", queryArtifactRows(t, st, planSeed.ctx, ownerV3Query, ownerV3Args), expected100, artifactLogicalBytes(expected100))
		st.ReadStats(1000)
		ownerMethodRows, err := st.ListArtifactsForOwners(planSeed.ctx, owners100)
		if err != nil {
			t.Fatal(err)
		}
		interval = st.ReadStats(1000)
		requireArtifactRows(t, "owner production 100 chunk", ownerMethodRows, expected100, artifactLogicalBytes(expected100))
		if interval.Queries != 1 {
			t.Fatalf("production 100-owner chunk issued %d statements, want 1", interval.Queries)
		}

		owners128 := store.ArtifactOwners{TaskIDs: append([]string(nil), plan.taskOwners[:128]...)}
		preds128 := artifactOwnerPredicates("task_id", owners128.TaskIDs)
		expected128 := oracle.ownerRows(preds128)
		diagV2Query, diagV2Args := artifactV2OwnerChunkQuery(planSeed.ws, preds128)
		requireArtifactRows(t, "owner v2 128 diagnostic", queryArtifactRows(t, st, planSeed.ctx, diagV2Query, diagV2Args), expected128, artifactLogicalBytes(expected128))
		diagV3Query, diagV3Args := st.artifactOwnerChunkQuery(planSeed.ws, preds128)
		requireArtifactRows(t, "owner v3 128 diagnostic", queryArtifactRows(t, st, planSeed.ctx, diagV3Query, diagV3Args), expected128, artifactLogicalBytes(expected128))
		st.ReadStats(1000)
		owner128Rows, err := st.ListArtifactsForOwners(planSeed.ctx, owners128)
		if err != nil {
			t.Fatal(err)
		}
		interval = st.ReadStats(1000)
		requireArtifactRows(t, "owner production 128 owners", owner128Rows, expected128, artifactLogicalBytes(expected128))
		if interval.Queries != 2 {
			t.Fatalf("128 owners issued %d statements, want 2 production chunks", interval.Queries)
		}

		heavy := plan.heavyOwners[0]
		heavyPreds := artifactOwnerPredicates("task_id", []string{heavy})
		heavyExpected := oracle.ownerRows(heavyPreds)
		if len(heavyExpected) != 3000 {
			t.Fatalf("heavy owner %s oracle rows=%d, want 3000", heavy, len(heavyExpected))
		}
		heavyV2Query, heavyV2Args := artifactV2OwnerChunkQuery(planSeed.ws, heavyPreds)
		heavyV3Query, heavyV3Args := st.artifactOwnerChunkQuery(planSeed.ws, heavyPreds)
		requireArtifactRows(t, "owner v2 heavy", queryArtifactRows(t, st, planSeed.ctx, heavyV2Query, heavyV2Args), heavyExpected, artifactLogicalBytes(heavyExpected))
		requireArtifactRows(t, "owner v3 heavy", queryArtifactRows(t, st, planSeed.ctx, heavyV3Query, heavyV3Args), heavyExpected, artifactLogicalBytes(heavyExpected))

		mixedOwners := store.ArtifactOwners{
			TaskIDs:        []string{planSeed.task, planSeed.task, planSeed.caseUpper, planSeed.caseLower, "missing-" + planSeed.ws},
			RequirementIDs: []string{planSeed.requirement},
			SessionIDs:     []string{planSeed.session},
		}
		var mixedPreds []ownerPredicate
		for _, id := range store.ArtifactOwnerIDs(mixedOwners.TaskIDs) {
			mixedPreds = append(mixedPreds, ownerPredicate{"task_id", id})
		}
		for _, id := range store.ArtifactOwnerIDs(mixedOwners.RequirementIDs) {
			mixedPreds = append(mixedPreds, ownerPredicate{"requirement_id", id})
		}
		for _, id := range store.ArtifactOwnerIDs(mixedOwners.SessionIDs) {
			mixedPreds = append(mixedPreds, ownerPredicate{"planning_session_id", id})
		}
		mixedOwnerExpected := oracle.ownerRows(mixedPreds)
		mixedOwnerV2Query, mixedOwnerV2Args := artifactV2OwnerChunkQuery(planSeed.ws, mixedPreds)
		mixedOwnerV3Query, mixedOwnerV3Args := st.artifactOwnerChunkQuery(planSeed.ws, mixedPreds)
		requireArtifactRows(t, "owner v2 mixed", queryArtifactRows(t, st, planSeed.ctx, mixedOwnerV2Query, mixedOwnerV2Args), mixedOwnerExpected, artifactLogicalBytes(mixedOwnerExpected))
		requireArtifactRows(t, "owner v3 mixed", queryArtifactRows(t, st, planSeed.ctx, mixedOwnerV3Query, mixedOwnerV3Args), mixedOwnerExpected, artifactLogicalBytes(mixedOwnerExpected))
		st.ReadStats(1000)
		mixedMethodRows, err := st.ListArtifactsForOwners(planSeed.ctx, mixedOwners)
		if err != nil {
			t.Fatal(err)
		}
		interval = st.ReadStats(1000)
		requireArtifactRows(t, "owner production mixed duplicate", mixedMethodRows, mixedOwnerExpected, artifactLogicalBytes(mixedOwnerExpected))
		if interval.Queries != 1 {
			t.Fatalf("mixed duplicate owners issued %d statements, want 1 chunk", interval.Queries)
		}

		requireArtifactScanBudget(t, st, planSeed.ctx, "owner 100 chunk", ownerV3Query, ownerV3Args, F)
		requireArtifactScanBudget(t, st, planSeed.ctx, "owner 128 diagnostic", diagV3Query, diagV3Args, F)
		requireArtifactScanBudget(t, st, planSeed.ctx, "owner heavy", heavyV3Query, heavyV3Args, F)
		requireArtifactScanBudget(t, st, planSeed.ctx, "owner mixed", mixedOwnerV3Query, mixedOwnerV3Args, F)

		st.ReadStats(1000)
		noMatchRows, err := st.ListArtifactsForOwners(planSeed.ctx, store.ArtifactOwners{TaskIDs: []string{"missing-" + planSeed.ws}})
		if err != nil {
			t.Fatal(err)
		}
		interval = st.ReadStats(1000)
		if len(noMatchRows) != 0 || interval.Queries != 1 {
			t.Fatalf("no-match owner read rows=%d statements=%d, want 0 rows and 1 statement", len(noMatchRows), interval.Queries)
		}

		assertArtifactPageAndChunkStatements(t, st, planSeed, plan.taskOwners)

		// Broad historical reference on the same fixture: the pre-change
		// single workspace fetch and Go owner filter, labelled as such.
		broadHydration := artifactSQLWork(t, st, planSeed.ctx, func(*Store) (string, []any) {
			return legacyArtifactListSQL, []any{planSeed.ws}
		})
		currentHydration := func(ctx context.Context) (storetest.ReadEgressOutcome, error) {
			rows, err := st.ListArtifacts(ctx)
			if err != nil {
				return storetest.ReadEgressOutcome{}, err
			}
			return artifactReadOutcome(t, rows), nil
		}
		measureArtifactRead(t, st, planSeed.ctx, "artifact-hydration-full-list-diagnostic", broadHydration, currentHydration)

		observedBroadPage := []artifactRawMeasurement{}
		observedBroadPageWork := artifactObservedWork(t, st, planSeed.ctx, func(*Store) (string, []any) {
			return st.artifactHydrationQuery(planSeed.ws, pageIDs)
		}, &observedBroadPage)
		broadPage := artifactBroadPageWork(t, st, planSeed.ctx, pageIDs)
		measureArtifactReadChecked(t, st, planSeed.ctx, "artifact-hydration-broad-page", broadPage, observedBroadPageWork, artifactBroadPageParity, pageExpected, 1, &observedBroadPage)

		ownerSet := artifactOwnerSet(owners100)
		broadOwner := func(ctx context.Context) (storetest.ReadEgressOutcome, error) {
			rows, err := st.db.QueryContext(ctx, legacyArtifactListSQL, planSeed.ws)
			if err != nil {
				return storetest.ReadEgressOutcome{}, err
			}
			artifacts, err := scanArtifactSQLRows(rows)
			if err != nil {
				return storetest.ReadEgressOutcome{}, err
			}
			filtered := artifacts[:0]
			for _, row := range artifacts {
				if artifactRowMatchesOwners(row, ownerSet) {
					filtered = append(filtered, row)
				}
			}
			return artifactReadOutcome(t, filtered), nil
		}
		currentOwner := func(ctx context.Context) (storetest.ReadEgressOutcome, error) {
			rows, err := st.ListArtifactsForOwners(ctx, owners100)
			if err != nil {
				return storetest.ReadEgressOutcome{}, err
			}
			return artifactReadOutcome(t, rows), nil
		}
		observedPageV2 := []artifactRawMeasurement{}
		observedPageV2Work := artifactObservedWork(t, st, planSeed.ctx, func(*Store) (string, []any) {
			return st.artifactHydrationQuery(planSeed.ws, pageIDs)
		}, &observedPageV2)
		observedOwner100 := []artifactRawMeasurement{}
		observedOwner100Work := artifactObservedOwnerWork(t, st, planSeed.ctx, planSeed.ws, preds100, &observedOwner100)
		observedOwner128 := []artifactRawMeasurement{}
		// Diagnostic 128-owner query; production owner chunks remain capped at 100.
		observedOwner128Work := artifactObservedWork(t, st, planSeed.ctx, func(s *Store) (string, []any) {
			return s.artifactOwnerChunkQuery(planSeed.ws, preds128)
		}, &observedOwner128)
		observedHeavy := []artifactRawMeasurement{}
		observedHeavyWork := artifactObservedOwnerWork(t, st, planSeed.ctx, planSeed.ws, heavyPreds, &observedHeavy)
		measureArtifactRead(t, st, planSeed.ctx, "artifact-owner-broad", broadOwner, currentOwner)

		measureArtifactReadChecked(t, st, planSeed.ctx, "artifact-hydration-v2-v3",
			artifactSQLWork(t, st, planSeed.ctx, v2Hydration),
			observedPageV2Work, artifactBoundedParity, pageExpected, 1, &observedPageV2)
		measureArtifactReadChecked(t, st, planSeed.ctx, "artifact-owner-v2-v3",
			artifactSQLWork(t, st, planSeed.ctx, func(*Store) (string, []any) { return artifactV2OwnerChunkQuery(planSeed.ws, preds100) }),
			observedOwner100Work, artifactBoundedParity, expected100, 1, &observedOwner100)
		measureArtifactReadChecked(t, st, planSeed.ctx, "artifact-owner-128-v2-v3",
			artifactSQLWork(t, st, planSeed.ctx, func(*Store) (string, []any) { return artifactV2OwnerChunkQuery(planSeed.ws, preds128) }),
			observedOwner128Work, artifactBoundedParity, expected128, 1, &observedOwner128)
		measureArtifactReadChecked(t, st, planSeed.ctx, "artifact-owner-heavy-v2-v3",
			artifactSQLWork(t, st, planSeed.ctx, func(*Store) (string, []any) { return artifactV2OwnerChunkQuery(planSeed.ws, heavyPreds) }),
			observedHeavyWork, artifactBoundedParity, heavyExpected, 1, &observedHeavy)

		// Enlarged unrelated noise: recalibrate F and prove the selected
		// sparse page's rows and logical bytes did not change.
		sparseBefore := queryArtifactRows(t, st, planSeed.ctx, sharedQuery, sharedArgs)
		requireArtifactRows(t, "sparse before noise", sparseBefore, sharedExpected, sharedBytes)
		addArtifactNoiseLinks(t, st, planSeed.ctx, 12000)
		addArtifactNoiseLinks(t, st, planSeed.ctx, 12000)
		flushArtifactPlanLinks(t, st, planSeed.ctx)
		runArtifactPlanSQL(t, st, planSeed.ctx, "ANALYZE TABLE artifact_links")
		runArtifactPlanSQL(t, st, planSeed.ctx, "ANALYZE TABLE artifacts")
		FNoise := calibrateArtifactFullScan(t, st, planSeed.ctx)
		if FNoise <= F {
			t.Fatalf("enlarged noise did not raise F: before=%d after=%d", F, FNoise)
		}
		sparseAfter := queryArtifactRows(t, st, planSeed.ctx, sharedQuery, sharedArgs)
		requireArtifactRows(t, "sparse after noise", sparseAfter, sharedExpected, sharedBytes)
		requireArtifactScanBudget(t, st, planSeed.ctx, "sparse after enlarged noise", sharedQuery, sharedArgs, FNoise)
		pageAfter := queryArtifactBuild(t, st, planSeed.ctx, v3Hydration)
		requireArtifactRows(t, "page after enlarged noise", pageAfter, pageExpected, pageBytes)
		pageNoiseQuery, pageNoiseArgs := v3Hydration(st)
		st.ReadStats(1000)
		pageNoiseRows, pageNoiseRaw := queryArtifactObserved(t, st, planSeed.ctx, pageNoiseQuery, pageNoiseArgs)
		pageNoiseInterval := st.ReadStats(1000)
		requireArtifactRows(t, "page after enlarged noise measured", pageNoiseRows, pageExpected, pageBytes)
		if pageNoiseInterval.Queries != 1 {
			t.Fatalf("page after enlarged noise issued %d statements, want 1 hydration", pageNoiseInterval.Queries)
		}
		requireArtifactRawOracle(t, "page after enlarged noise raw", newArtifactCellOracle(pageExpected, 1), pageNoiseRaw)
		requireArtifactScanBudget(t, st, planSeed.ctx, "hydration page after enlarged noise", pageNoiseQuery, pageNoiseArgs, FNoise)
		observedSparse := []artifactRawMeasurement{}
		observedSparseWork := artifactObservedWork(t, st, planSeed.ctx, func(*Store) (string, []any) {
			return st.artifactHydrationQuery(planSeed.ws, []string{planSeed.shared})
		}, &observedSparse)
		measureArtifactReadChecked(t, st, planSeed.ctx, "artifact-sparse-noisy-v2-v3",
			artifactSQLWork(t, st, planSeed.ctx, func(*Store) (string, []any) { return artifactV2HydrationQuery(planSeed.ws, []string{planSeed.shared}) }),
			observedSparseWork, artifactBoundedParity, sharedExpected, 1, &observedSparse)
	})

	t.Run("concurrent insert past the snapshot", func(t *testing.T) {
		for _, layout := range append([]struct {
			name  string
			ready []string
		}{{"prepared", artifactLookupIndexes}}, layouts...) {
			setArtifactLayout(t, st, layout.ready...)
			var snapAt time.Time
			var snapID string
			if err := st.db.QueryRowContext(seed.ctx, `SELECT created_at,id FROM artifacts WHERE workspace_id=? ORDER BY created_at DESC,id DESC LIMIT 1`, seed.ws).Scan(&snapAt, &snapID); err != nil {
				t.Fatal(err)
			}
			var inserted []string
			insert := func(at time.Time, above bool) {
				for i := 0; ; i++ {
					body := []byte(fmt.Sprintf("late-%s-%s-%d-%v", layout.name, seed.ws, i, at.UnixNano()))
					id := fmt.Sprintf("%x", sha256.Sum256(body))
					if above && id <= snapID {
						continue
					}
					created, err := st.CreateArtifact(seed.ctx, core.Artifact{Name: "late.txt", ContentType: "text/plain", TaskID: seed.noise, CreatedAt: at}, body)
					if err != nil {
						t.Error(err)
						return
					}
					inserted = append(inserted, created.ID)
					return
				}
			}
			fired := false
			store.ArtifactListPageHook = func() {
				if fired {
					return
				}
				fired = true
				insert(snapAt.Add(time.Hour), false) // later than the snapshot
				insert(snapAt, true)                 // equal created_at, ID above the snapshot ID
			}
			listed, err := st.ListArtifacts(seed.ctx)
			store.ArtifactListPageHook = nil
			if err != nil {
				t.Fatal(err)
			}
			for _, artifact := range listed {
				if slices.Contains(inserted, artifact.ID) {
					t.Errorf("%s listing observed insert %s past the snapshot", layout.name, artifact.ID)
				}
			}
		}
	})

	// Associations are read per page, so a link added or removed between
	// pages may change that artifact's association rows. It never repeats an
	// artifact or drops one that existed at the high-water read.
	t.Run("link changes between pages", func(t *testing.T) {
		for _, layout := range append([]struct {
			name  string
			ready []string
		}{{"prepared", artifactLookupIndexes}}, layouts...) {
			setArtifactLayout(t, st, layout.ready...)
			before, err := st.ListArtifacts(seed.ctx)
			if err != nil {
				t.Fatal(err)
			}
			existing := map[string]bool{}
			unchanged := map[string][]string{}
			for _, artifact := range before {
				existing[artifact.ID] = true
				if artifact.ID != seed.orphan && artifact.ID != seed.sessionArt {
					unchanged[artifact.ID] = append(unchanged[artifact.ID], artifactTestKey(artifact))
				}
			}
			calls := 0
			store.ArtifactListPageHook = func() {
				// Calls: the high-water read, then once after each page's key
				// read. The fixture lists more than one page, so the third call
				// lands after page 1 was hydrated and before page 2 is.
				calls++
				if calls != 3 {
					return
				}
				// Add an owner to the orphan and remove the session link.
				if _, execErr := st.db.ExecContext(seed.ctx, `INSERT INTO artifact_links(workspace_id,artifact_id,task_id,role) VALUES(?,?,?,?)`, seed.ws, seed.orphan, seed.noise, core.ArtifactRoleTaskContext); execErr != nil {
					t.Error(execErr)
				}
				if _, execErr := st.db.ExecContext(seed.ctx, `DELETE FROM artifact_links WHERE workspace_id=? AND artifact_id=?`, seed.ws, seed.sessionArt); execErr != nil {
					t.Error(execErr)
				}
			}
			after, err := st.ListArtifacts(seed.ctx)
			store.ArtifactListPageHook = nil
			// Put the links back for the next layout.
			if _, execErr := st.db.ExecContext(seed.ctx, `DELETE FROM artifact_links WHERE workspace_id=? AND artifact_id=?`, seed.ws, seed.orphan); execErr != nil {
				t.Fatal(execErr)
			}
			if _, execErr := st.db.ExecContext(seed.ctx, `INSERT INTO artifact_links(workspace_id,artifact_id,planning_session_id,role) VALUES(?,?,?,?)`, seed.ws, seed.sessionArt, seed.session, core.ArtifactRoleTaskContext); execErr != nil {
				t.Fatal(execErr)
			}
			if err != nil {
				t.Fatal(err)
			}
			got := map[string][]string{}
			present := map[string]bool{}
			for _, artifact := range after {
				present[artifact.ID] = true
				if artifact.ID != seed.orphan && artifact.ID != seed.sessionArt {
					got[artifact.ID] = append(got[artifact.ID], artifactTestKey(artifact))
				}
			}
			for id := range existing {
				if !present[id] {
					t.Errorf("%s: artifact %s existing at the high-water read was dropped", layout.name, id)
				}
			}
			for id := range present {
				if !existing[id] {
					t.Errorf("%s: artifact %s appeared that did not exist at the high-water read", layout.name, id)
				}
			}
			for id, want := range unchanged {
				if !slices.Equal(got[id], want) {
					t.Errorf("%s: unrelated artifact %s association rows changed: %v want %v", layout.name, id, got[id], want)
				}
			}
			// Each artifact's rows are contiguous: an artifact is never split
			// across pages into two groups.
			last, seenGroup := "", map[string]bool{}
			for _, artifact := range after {
				if artifact.ID != last {
					if seenGroup[artifact.ID] {
						t.Errorf("%s: artifact %s listed in two groups", layout.name, artifact.ID)
					}
					seenGroup[artifact.ID] = true
					last = artifact.ID
				}
			}
		}
	})
}

func artifactTestKey(a core.Artifact) string {
	return a.ID + "\x00" + string(a.Role) + "\x00" + a.TaskID + "\x00" + a.RequirementID + "\x00" + a.PlanningSessionID
}

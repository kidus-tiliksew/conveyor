package singlestore

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

type layoutFixture struct {
	columns        map[string]bool
	keys           map[string][]string
	kinds          map[string]string
	storage        map[string]string
	storageQueries map[string]int
	badColumn      string
	failure        string
}

type layoutConnector struct{ fixture *layoutFixture }
type layoutConn struct{ fixture *layoutFixture }
type layoutDriver struct{}
type layoutRows struct {
	columns []string
	values  [][]driver.Value
}

func (c layoutConnector) Connect(context.Context) (driver.Conn, error) {
	return layoutConn{c.fixture}, nil
}
func (c layoutConnector) Driver() driver.Driver        { return layoutDriver{} }
func (layoutDriver) Open(string) (driver.Conn, error)  { return nil, errors.New("unused") }
func (layoutConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unused") }
func (layoutConn) Close() error                        { return nil }
func (layoutConn) Begin() (driver.Tx, error)           { return nil, errors.New("unused") }
func (r *layoutRows) Columns() []string                { return r.columns }
func (r *layoutRows) Close() error                     { return nil }
func (r *layoutRows) Next(dest []driver.Value) error {
	if len(r.values) == 0 {
		return io.EOF
	}
	copy(dest, r.values[0])
	r.values = r.values[1:]
	return nil
}
func (c layoutConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	f := c.fixture
	if f.failure != "" && strings.Contains(query, f.failure) {
		return nil, errors.New("private metadata failure")
	}
	switch {
	case strings.Contains(query, "information_schema.columns"):
		n := int64(0)
		if f.columns[args[1].Value.(string)] {
			n = 1
		}
		return &layoutRows{columns: []string{"count"}, values: [][]driver.Value{{n}}}, nil
	case strings.HasPrefix(query, "SHOW CREATE TABLE "):
		table := strings.TrimPrefix(query, "SHOW CREATE TABLE ")
		var definition strings.Builder
		for _, index := range hotReadIndexes {
			if index.table != table || !f.columns[index.column] {
				continue
			}
			expression := index.expression
			if index.column == f.badColumn {
				expression = "SHA2('wrong',256)"
			}
			fmt.Fprintf(&definition, "\n`%s` AS (%s) PERSISTED VARCHAR(64),", index.column, expression)
		}
		return &layoutRows{columns: []string{"table", "create"}, values: [][]driver.Value{{table, definition.String()}}}, nil
	case strings.Contains(query, "information_schema.tables"):
		table := args[0].Value.(string)
		f.storageQueries[table]++
		return &layoutRows{columns: []string{"storage_type"}, values: [][]driver.Value{{f.storage[table]}}}, nil
	case strings.Contains(query, "information_schema.statistics"):
		name := args[1].Value.(string)
		r := &layoutRows{columns: []string{"column_name", "index_type", "non_unique"}}
		for _, column := range f.keys[name] {
			r.values = append(r.values, []driver.Value{column, f.kinds[name], int64(1)})
		}
		return r, nil
	default:
		return nil, fmt.Errorf("unexpected query")
	}
}

func preparedLayoutFixture() *layoutFixture {
	f := &layoutFixture{columns: map[string]bool{}, keys: map[string][]string{}, kinds: map[string]string{}, storage: map[string]string{}, storageQueries: map[string]int{}}
	for _, index := range hotReadIndexes {
		if index.column == "" {
			continue
		}
		f.columns[index.column] = true
		f.storage[index.table] = "COLUMNSTORE"
		f.keys[index.name] = append([]string(nil), index.columns...)
		if index.hash {
			f.kinds[index.name] = "COLUMNSTORE HASH"
		} else {
			f.kinds[index.name] = "BTREE"
		}
	}
	return f
}

func layoutStore(t *testing.T, f *layoutFixture) (*Store, error) {
	t.Helper()
	db := sql.OpenDB(layoutConnector{f})
	t.Cleanup(func() { db.Close() })
	s := &Store{db: db}
	return s, s.initHotReadLayout(context.Background())
}

func TestHotReadLayoutDiscovery(t *testing.T) {
	const document = "events_document_key_idx"
	const requirement = "events_requirement_key_idx"
	const task = "tasks_workspace_state_created_idx"
	t.Run("prepared and unknown", func(t *testing.T) {
		f := preparedLayoutFixture()
		f.kinds[document] = "HASH"
		s, err := layoutStore(t, f)
		if err != nil {
			t.Fatal(err)
		}
		for _, index := range hotReadIndexes {
			if index.column != "" && !s.hotReadIndexReady(index.name) {
				t.Errorf("%s not ready", index.name)
			}
		}
		for _, table := range []string{"events_columnstore", "tasks", "artifact_links"} {
			if got := f.storageQueries[table]; got != 1 {
				t.Errorf("%s storage queries=%d, want 1", table, got)
			}
		}
		if s.hotReadIndexReady("unknown") || s.hotReadIndexReady("tasks_workspace_created_idx") {
			t.Fatal("unknown lookup ready")
		}
	})
	t.Run("rowstore keys", func(t *testing.T) {
		f := preparedLayoutFixture()
		f.storage["artifact_links"] = "INMEMORY_ROWSTORE"
		for _, index := range hotReadIndexes {
			if index.table == "artifact_links" {
				f.kinds[index.name] = "BTREE"
			}
		}
		s, err := layoutStore(t, f)
		if err != nil {
			t.Fatal(err)
		}
		for _, index := range hotReadIndexes {
			if index.table == "artifact_links" && !s.hotReadIndexReady(index.name) {
				t.Errorf("%s rowstore key not ready", index.name)
			}
		}
		if got := f.storageQueries["artifact_links"]; got != 1 {
			t.Errorf("artifact_links storage queries=%d, want 1", got)
		}
	})
	t.Run("legacy and reopen", func(t *testing.T) {
		f := preparedLayoutFixture()
		f.columns = map[string]bool{}
		s, err := layoutStore(t, f)
		if err != nil {
			t.Fatal(err)
		}
		if s.hotReadIndexReady(document) {
			t.Fatal("legacy lookup ready")
		}
		f.columns = preparedLayoutFixture().columns
		if s.hotReadIndexReady(document) {
			t.Fatal("snapshot changed without reopen")
		}
		reopened, err := layoutStore(t, f)
		if err != nil {
			t.Fatal(err)
		}
		if !reopened.hotReadIndexReady(document) {
			t.Fatal("reopened lookup not ready")
		}
	})
	t.Run("partial and incompatible keys", func(t *testing.T) {
		f := preparedLayoutFixture()
		delete(f.columns, "event_document_key")
		delete(f.keys, requirement)
		f.keys[task] = []string{"task_state_key", "workspace_id", "created_at", "id"}
		f.kinds["tasks_workspace_intake_lookup_idx"] = "COLUMNSTORE HASH"
		s, err := layoutStore(t, f)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{document, requirement, task, "tasks_workspace_intake_lookup_idx"} {
			if s.hotReadIndexReady(name) {
				t.Errorf("%s incorrectly ready", name)
			}
		}
		if !s.hotReadIndexReady("artifact_links_task_lookup_idx") {
			t.Fatal("independent lookup not ready")
		}
	})
	t.Run("incompatible definition", func(t *testing.T) {
		f := preparedLayoutFixture()
		f.badColumn = "event_document_key"
		delete(f.keys, document)
		s, err := layoutStore(t, f)
		if err == nil || !strings.Contains(err.Error(), "incompatible hot-read computed column") || s.hotReadLayout != nil {
			t.Fatalf("layout=%v err=%v", s.hotReadLayout, err)
		}
	})
	for _, failure := range []string{"information_schema.columns", "SHOW CREATE TABLE", "information_schema.tables", "information_schema.statistics"} {
		t.Run("metadata failure "+failure, func(t *testing.T) {
			f := preparedLayoutFixture()
			f.failure = failure
			s, err := layoutStore(t, f)
			if err == nil || strings.Contains(err.Error(), "private metadata failure") || s.hotReadLayout != nil {
				t.Fatalf("layout=%v err=%v", s.hotReadLayout, err)
			}
		})
	}
}

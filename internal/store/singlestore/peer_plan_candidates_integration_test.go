package singlestore

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// TEMPORARY: compares statement shapes for the peer read's visibility branch.
func TestPeerVisibilityPlanCandidatesIntegration(t *testing.T) {
	writer := openOwnedStore(t)
	ctxA, wsA, taskA := seedSingleStoreNoticeTask(t, writer)
	_, wsB, _ := seedSingleStoreNoticeTask(t, writer)
	for i := 0; i < 5; i++ {
		if err := writer.AppendEvent(ctxA, core.Event{TaskID: taskA, Kind: "candidate"}); err != nil {
			t.Fatal(err)
		}
	}
	daemon := openSecondStore(t)
	if _, err := daemon.PeerMarks(context.Background()); err != nil {
		t.Fatal(err)
	}
	daemon.peerMu.Lock()
	idle := map[string]int64{}
	for k, v := range daemon.peerAccepted {
		idle[k] = v
	}
	daemon.peerMu.Unlock()
	floor := "CASE w.workspace_id WHEN '" + wsA + "' THEN " + itoa64(idle[wsA]) + " WHEN '" + wsB + "' THEN " + itoa64(idle[wsB]) + " ELSE 0 END"
	changed := "0"
	candidates := map[string]string{
		"A inner join":       `SELECT w.workspace_id, w.position FROM task_event_witnesses w JOIN events_columnstore e ON e.event_shard_key = w.event_shard_key AND e.workspace_id = w.workspace_id AND e.id = w.event_id WHERE w.position > %s`,
		"B exists in select": `SELECT w.workspace_id, w.position, EXISTS(SELECT 1 FROM events_columnstore e WHERE e.event_shard_key = w.event_shard_key AND e.workspace_id = w.workspace_id AND e.id = w.event_id) FROM task_event_witnesses w WHERE w.position > %s`,
		"C in subquery":      `SELECT w.workspace_id, w.position FROM task_event_witnesses w WHERE w.position > %s AND w.event_id IN (SELECT e.id FROM events_columnstore e WHERE e.event_shard_key = w.event_shard_key AND e.workspace_id = w.workspace_id)`,
		"D derived limit":    `SELECT w.workspace_id, w.position FROM (SELECT workspace_id, position, event_shard_key, event_id FROM task_event_witnesses WHERE position > %s LIMIT 100000) w JOIN events_columnstore e ON e.event_shard_key = w.event_shard_key AND e.workspace_id = w.workspace_id AND e.id = w.event_id`,
		"E force index":      `SELECT w.workspace_id, w.position FROM task_event_witnesses w JOIN events_columnstore e FORCE INDEX (events_task_event_idx) ON e.event_shard_key = w.event_shard_key AND e.workspace_id = w.workspace_id AND e.id = w.event_id WHERE w.position > %s`,
		"F straight join":    `SELECT STRAIGHT_JOIN w.workspace_id, w.position FROM task_event_witnesses w JOIN events_columnstore e ON e.event_shard_key = w.event_shard_key AND e.workspace_id = w.workspace_id AND e.id = w.event_id WHERE w.position > %s`,
		"G lateral style":    `SELECT w.workspace_id, w.position, (SELECT COUNT(*) FROM events_columnstore e WHERE e.event_shard_key = w.event_shard_key AND e.workspace_id = w.workspace_id AND e.id = w.event_id) FROM task_event_witnesses w WHERE w.position > %s`,
	}
	for _, flush := range []bool{false, true} {
		if flush {
			if _, err := writer.db.ExecContext(t.Context(), "OPTIMIZE TABLE events_columnstore FLUSH"); err != nil {
				t.Logf("flush: %v", err)
			}
		}
		for name, shape := range candidates {
			for _, bound := range []struct{ label, floor string }{{"idle", floor}, {"changed", changed}} {
				query := strings.Replace(shape, "%s", bound.floor, 1)
				text, rows, err := profileText(t, writer, query)
				if err != nil {
					t.Logf("CANDIDATE %s flush=%v %s: error %v", name, flush, bound.label, err)
					continue
				}
				t.Logf("CANDIDATE %s flush=%v %s rows=%d\n%s", name, flush, bound.label, rows, text)
			}
		}
	}
	t.Fatal("temporary candidate comparison")
}

func openSecondStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(t.Context(), sharedDatabase.cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	return st
}

func itoa64(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func profileText(t *testing.T, st *Store, query string) (string, int, error) {
	conn, err := st.db.Conn(t.Context())
	if err != nil {
		return "", 0, err
	}
	defer conn.Close()
	rows, err := conn.QueryContext(t.Context(), "PROFILE "+query)
	if err != nil {
		return "", 0, err
	}
	n := 0
	for rows.Next() {
		n++
	}
	rows.Close()
	var profile string
	if err = conn.QueryRowContext(t.Context(), "SHOW PROFILE JSON").Scan(&profile); err != nil {
		return "", n, err
	}
	var tree map[string]any
	if err = json.Unmarshal([]byte(profile), &tree); err != nil {
		return profile, n, nil
	}
	var text string
	var find func(any)
	find = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if s, ok := x["text_profile"].(string); ok {
				text = s
			}
			for _, c := range x {
				find(c)
			}
		case []any:
			for _, c := range x {
				find(c)
			}
		}
	}
	find(tree)
	return text, n, nil
}

package singlestore

import (
	"context"
	"fmt"
	"slices"
)

// initHotReadLayout publishes only a complete discovery; the map is never
// changed after Open, even if an operator prepares the schema later.
func (s *Store) initHotReadLayout(ctx context.Context) error {
	ready := make(map[string]bool)
	definitions := make(map[string]string)
	for _, index := range hotReadIndexes {
		if index.column == "" {
			continue
		}
		var exists int
		if err := backendRow(ctx, s.db, `SELECT COUNT(*) FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name=? AND column_name=?`, index.table, index.column).Scan(&exists); err != nil {
			return fmt.Errorf("discover hot-read columns: metadata query failed")
		}
		if exists == 0 {
			continue
		}
		definition, ok := definitions[index.table]
		if !ok {
			var table string
			if err := backendRow(ctx, s.db, "SHOW CREATE TABLE "+index.table).Scan(&table, &definition); err != nil {
				return fmt.Errorf("discover hot-read column definitions: metadata query failed")
			}
			definitions[index.table] = definition
		}
		if err := validateHotReadColumn(index, definition); err != nil {
			return err
		}
		rows, err := backendRows(ctx, s.db, `SELECT column_name,index_type,non_unique FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name=? AND index_name=? ORDER BY seq_in_index`, index.table, index.name)
		if err != nil {
			return fmt.Errorf("discover hot-read indexes: metadata query failed")
		}
		var columns []string
		validType := true
		for rows.Next() {
			var column, kind string
			var nonUnique int
			if err := rows.Scan(&column, &kind, &nonUnique); err != nil {
				rows.Close()
				return fmt.Errorf("discover hot-read indexes: invalid metadata")
			}
			validType = validType && hotReadIndexTypeMatches(index, kind, nonUnique)
			columns = append(columns, column)
		}
		err = rows.Err()
		closeErr := rows.Close()
		if err != nil || closeErr != nil {
			return fmt.Errorf("discover hot-read indexes: metadata query failed")
		}
		ready[index.name] = validType && slices.Equal(columns, index.columns)
	}
	s.hotReadLayout = ready
	return nil
}

func (s *Store) hotReadIndexReady(indexName string) bool {
	return s.hotReadLayout[indexName]
}

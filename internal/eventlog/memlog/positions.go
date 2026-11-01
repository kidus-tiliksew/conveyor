package memlog

import "github.com/kidus-tiliksew/conveyor/internal/eventlog"

// Positions returns each workspace's head position. The volatile backend's
// peer check read uses it (component-durable-queue, The log;
// component-persistence, Change notices).
func (s *Store) Positions() map[string]eventlog.Position {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]eventlog.Position, len(s.workspaces))
	for id, w := range s.workspaces {
		if w.position > 0 {
			out[id] = w.position
		}
	}
	return out
}

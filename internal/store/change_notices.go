package store

import (
	"context"

	"github.com/kidus-tiliksew/conveyor/internal/changes"
)

// The volatile backend publishes change notices after each mutation
// (component-persistence, Change notices; DEC-99(1)).

// Changes is the hub the volatile backend publishes notices on.
func (m *memory) Changes() *changes.Hub { return m.changes }

// EnablePeers reports false: the volatile backend notifies no other daemon,
// so a peered daemon runs the peer check on PeerMarks instead.
func (m *memory) EnablePeers(context.Context) bool { return false }

// raiseLocked publishes n, or holds it on a staged copy until the copy is
// installed. The caller holds the mutex.
func (m *memory) raiseLocked(n changes.Notice) {
	if m.staged != nil {
		*m.staged = append(*m.staged, n)
		return
	}
	m.changes.Publish(n)
}

// stageNotices makes staged hold its notices for installStaged.
func stageNotices(staged *memory) {
	staged.staged = &[]changes.Notice{}
}

// publishStaged publishes a staged copy's notices once m installed it.
func (m *memory) publishStaged(staged *memory) {
	if staged.staged == nil {
		return
	}
	for _, n := range *staged.staged {
		m.changes.Publish(n)
	}
	staged.staged = nil
}

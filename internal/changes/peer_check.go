package changes

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

// The daemon-wide peer check (feature-quiet-idle-runtime, Peers;
// component-runtime, Peers; DEC-99(3); req-quiet-idle-runtime AC-1.3,
// AC-2.5). On a backend without change notification, a daemon that runs
// beside other daemons learns of their commits from one statement every
// interval, for all its workspaces, open task streams, and pending
// await_review calls together. The check republishes what changed on the
// hub; nothing else in the daemon keeps a peer timer.

// PeerMark is what one peer check read for one workspace.
type PeerMark struct {
	// Log is the head position of the workspace's event log.
	Log int64
	// Events is the number of the workspace's task events.
	Events int64
	// LastEvent is the highest ID among the workspace's task events.
	LastEvent int64
}

// PeerReader is the optional capability of a backend without change
// notification: PeerMarks reads every workspace's marks in one statement
// (component-persistence, Change notices). A failed read returns no marks.
type PeerReader interface {
	PeerMarks(ctx context.Context) (map[string]PeerMark, error)
}

// PeerBackend is what peer mode selection asks of a store.
type PeerBackend interface {
	// EnablePeers turns on cross-daemon change notification and reports
	// whether the backend notifies other daemons itself.
	EnablePeers(ctx context.Context) bool
}

// PeerCheck reads every workspace's marks once per interval and publishes a
// job notice for each workspace whose log head moved and one task notice
// without a task ID for each workspace whose task events changed, compared
// with the previous successful read. A workspace that read did not return
// gets both. A failed read publishes nothing and keeps the previous marks,
// so the next successful read still finds every change since the last one.
type PeerCheck struct {
	Reader   PeerReader
	Hub      *Hub
	Interval time.Duration
	// Ticker replaces the interval's ticker in tests; nil selects
	// time.NewTicker.
	Ticker func(time.Duration) (<-chan time.Time, func())
	// Checked, when set, receives each check's published notice count and
	// error after its notices are published.
	Checked func(published int, err error)
	// Logf receives operational messages; nil discards them.
	Logf func(string, ...any)

	mu    sync.Mutex
	marks map[string]PeerMark
}

// Check runs one peer check: one read, then the notices it implies.
func (p *PeerCheck) Check(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	marks, err := p.Reader.PeerMarks(ctx)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			p.logf(`level=error msg="peer check failed" retry_in=%s err=%v`, p.Interval, err)
		}
		p.checked(0, err)
		return err
	}
	workspaces := make([]string, 0, len(marks))
	for workspace := range marks {
		workspaces = append(workspaces, workspace)
	}
	sort.Strings(workspaces)
	published := 0
	for _, workspace := range workspaces {
		if workspace == "" {
			continue
		}
		mark := marks[workspace]
		previous, seen := p.marks[workspace]
		if !seen || mark.Log != previous.Log {
			p.Hub.Publish(Notice{Workspace: workspace, Kind: Job})
			published++
		}
		if !seen || mark.Events != previous.Events || mark.LastEvent != previous.LastEvent {
			p.Hub.Publish(Notice{Workspace: workspace, Kind: Task})
			published++
		}
	}
	p.marks = marks
	p.checked(published, nil)
	return nil
}

// Run checks once per interval until ctx ends. The daemon runs the first
// check itself, before its readers' first reads, so Run starts with a wait.
func (p *PeerCheck) Run(ctx context.Context) {
	tick, stop := p.ticker()
	defer stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
			_ = p.Check(ctx)
		}
	}
}

func (p *PeerCheck) ticker() (<-chan time.Time, func()) {
	if p.Ticker != nil {
		return p.Ticker(p.Interval)
	}
	ticker := time.NewTicker(p.Interval)
	return ticker.C, ticker.Stop
}

func (p *PeerCheck) checked(published int, err error) {
	if p.Checked != nil {
		p.Checked(published, err)
	}
}

func (p *PeerCheck) logf(format string, args ...any) {
	if p.Logf != nil {
		p.Logf(format, args...)
	}
}

// SelectPeerCheck selects the daemon's peer mode from runtime.peers
// (component-runtime, Peers; DEC-99(2), (3)). Off: no listener and no peer
// check. On a backend that notifies peers (PostgreSQL): no check, its
// listener republishes other daemons' notices. Otherwise: one peer check for
// the whole daemon every interval on the backend's PeerMarks read. It
// returns nil when no check runs.
func SelectPeerCheck(ctx context.Context, peers bool, backend PeerBackend, hub *Hub, interval time.Duration, logf func(string, ...any)) *PeerCheck {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if !peers {
		logf("runtime peers: off")
		return nil
	}
	if backend.EnablePeers(ctx) {
		logf("runtime peers: PostgreSQL change notification")
		return nil
	}
	reader, ok := backend.(PeerReader)
	if !ok {
		logf(`level=error msg="runtime peers: the backend neither notifies peers nor offers a peer check; no peer check runs"`)
		return nil
	}
	logf("runtime peers: one peer check every %s for the whole daemon", interval)
	return &PeerCheck{Reader: reader, Hub: hub, Interval: interval, Logf: logf}
}

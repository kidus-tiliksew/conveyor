package dbmetrics

import (
	"context"
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
)

// UnlabeledRead is the label of bytes received outside any operation, such as
// connection setup and pool pings, and of operations whose context carries no
// read label (component-persistence, Database read accounting).
const UnlabeledRead = "unlabeled"

type readLabelKey struct{}

// WithReadLabel returns ctx carrying label. A context that already carries a
// label, or an empty label, returns ctx unchanged, so the outermost store
// method of a call chain keeps the attribution.
func WithReadLabel(ctx context.Context, label string) context.Context {
	if label == "" {
		return ctx
	}
	if _, ok := ctx.Value(readLabelKey{}).(string); ok {
		return ctx
	}
	return context.WithValue(ctx, readLabelKey{}, label)
}

// ReadLabel returns the context's read label, or the empty string.
func ReadLabel(ctx context.Context) string {
	label, _ := ctx.Value(readLabelKey{}).(string)
	return label
}

// ReadAccount accumulates one label's received bytes, result rows, and query
// operations. It holds no SQL, argument, identifier, actor, or workspace.
type ReadAccount struct {
	label   string
	bytes   atomic.Uint64
	rows    atomic.Uint64
	queries atomic.Uint64
}

// ReadTotal is one label's counts for a drained interval.
type ReadTotal struct {
	Label   string
	Bytes   uint64
	Rows    uint64
	Queries uint64
}

// ReadInterval is everything received since the previous drain: the totals,
// the number of labels that received bytes, and the top labels by bytes.
type ReadInterval struct {
	Bytes   uint64
	Rows    uint64
	Queries uint64
	Labels  int
	Top     []ReadTotal
}

// ReadLedger maps read labels to their accounts for one opened store. Its
// methods are safe for concurrent use; a nil ledger records nothing.
type ReadLedger struct {
	accounts  sync.Map // label -> *ReadAccount
	unlabeled *ReadAccount
}

// NewReadLedger returns an empty ledger.
func NewReadLedger() *ReadLedger {
	ledger := &ReadLedger{unlabeled: &ReadAccount{label: UnlabeledRead}}
	ledger.accounts.Store(UnlabeledRead, ledger.unlabeled)
	return ledger
}

// Account returns the label's account, creating it on first use. The empty
// label is the unlabeled account.
func (l *ReadLedger) Account(label string) *ReadAccount {
	if l == nil {
		return nil
	}
	if label == "" {
		return l.unlabeled
	}
	if account, ok := l.accounts.Load(label); ok {
		return account.(*ReadAccount)
	}
	account, _ := l.accounts.LoadOrStore(label, &ReadAccount{label: label})
	return account.(*ReadAccount)
}

// Drain swaps every counter to zero and returns the interval's totals with
// at most top labels, ordered by bytes received and then by label. An
// addition racing the drain lands in exactly one interval.
func (l *ReadLedger) Drain(top int) ReadInterval {
	var interval ReadInterval
	if l == nil {
		return interval
	}
	var totals []ReadTotal
	l.accounts.Range(func(_, value any) bool {
		account := value.(*ReadAccount)
		total := ReadTotal{
			Label:   account.label,
			Bytes:   account.bytes.Swap(0),
			Rows:    account.rows.Swap(0),
			Queries: account.queries.Swap(0),
		}
		interval.Bytes += total.Bytes
		interval.Rows += total.Rows
		interval.Queries += total.Queries
		if total.Bytes > 0 {
			interval.Labels++
			totals = append(totals, total)
		}
		return true
	})
	sort.Slice(totals, func(i, j int) bool {
		if totals[i].Bytes != totals[j].Bytes {
			return totals[i].Bytes > totals[j].Bytes
		}
		return totals[i].Label < totals[j].Label
	})
	if top < 0 {
		top = 0
	}
	if len(totals) > top {
		totals = totals[:top]
	}
	interval.Top = totals
	return interval
}

// WrapConn returns c counting every byte read into the ledger. The wrapper
// implements syscall.Conn only when c does. A nil ledger returns c.
func (l *ReadLedger) WrapConn(c net.Conn) net.Conn {
	if l == nil || c == nil {
		return c
	}
	counted := &ReadConn{Conn: c, ledger: l}
	if raw, ok := c.(syscall.Conn); ok {
		return &readSyscallConn{ReadConn: counted, raw: raw}
	}
	return counted
}

// ReadConn counts the bytes read from a database connection into the account
// of the operation in flight on it, or into the unlabeled account.
type ReadConn struct {
	net.Conn
	ledger *ReadLedger
	active atomic.Pointer[ReadAccount]
}

// Read counts the bytes read.
func (c *ReadConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		account := c.active.Load()
		if account == nil {
			account = c.ledger.unlabeled
		}
		account.bytes.Add(uint64(n))
	}
	return n, err
}

// Begin makes the label's account the operation in flight. Safe on nil.
func (c *ReadConn) Begin(label string) {
	if c == nil {
		return
	}
	c.active.Store(c.ledger.Account(label))
}

// End adds the operation's query and row counts to the account in flight
// and clears it. Safe on nil and without a Begin.
func (c *ReadConn) End(queries, rows uint64) {
	if c == nil {
		return
	}
	account := c.active.Swap(nil)
	if account == nil {
		return
	}
	if queries > 0 {
		account.queries.Add(queries)
	}
	if rows > 0 {
		account.rows.Add(rows)
	}
}

func (c *ReadConn) readConn() *ReadConn { return c }

// readSyscallConn forwards syscall.Conn for a wrapped connection that has
// it, such as the TCP connection whose liveness the MySQL driver checks.
type readSyscallConn struct {
	*ReadConn
	raw syscall.Conn
}

func (c *readSyscallConn) SyscallConn() (syscall.RawConn, error) { return c.raw.SyscallConn() }

type readConnHolder interface{ readConn() *ReadConn }

// ReadConnOf returns the counting wrapper of c, looking under a TLS
// connection, or nil when c is not counted.
func ReadConnOf(c net.Conn) *ReadConn {
	for range 4 {
		switch conn := c.(type) {
		case nil:
			return nil
		case readConnHolder:
			return conn.readConn()
		case interface{ NetConn() net.Conn }:
			c = conn.NetConn()
		default:
			return nil
		}
	}
	return nil
}

package dbmetrics

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"reflect"
	"sync"
	"syscall"
	"testing"
)

func TestWithReadLabelKeepsOutermost(t *testing.T) {
	ctx := context.Background()
	if got := ReadLabel(ctx); got != "" {
		t.Fatalf("unlabeled context reads %q", got)
	}
	if WithReadLabel(ctx, "") != ctx {
		t.Fatal("an empty label changed the context")
	}
	outer := WithReadLabel(ctx, "ListWorkOrders")
	if got := ReadLabel(outer); got != "ListWorkOrders" {
		t.Fatalf("label = %q", got)
	}
	if WithReadLabel(outer, "GetTask") != outer {
		t.Fatal("an inner store method replaced the outer label")
	}
}

func TestReadLedgerDrainOrdersAndResets(t *testing.T) {
	ledger := NewReadLedger()
	add := func(label string, bytes, rows, queries uint64) {
		account := ledger.Account(label)
		account.bytes.Add(bytes)
		account.rows.Add(rows)
		account.queries.Add(queries)
	}
	add("GetTask", 300, 1, 1)
	add("ListWorkOrders", 900, 40, 2)
	add("ListEvents", 300, 9, 3)
	add("", 50, 0, 4)
	add("CreateTask", 0, 0, 2) // queries without bytes are totalled but not ranked
	if ledger.Account("") != ledger.Account(UnlabeledRead) {
		t.Fatal("the empty label is not the unlabeled account")
	}
	got := ledger.Drain(3)
	want := ReadInterval{Bytes: 1550, Rows: 50, Queries: 12, Labels: 4, Top: []ReadTotal{
		{Label: "ListWorkOrders", Bytes: 900, Rows: 40, Queries: 2},
		{Label: "GetTask", Bytes: 300, Rows: 1, Queries: 1},
		{Label: "ListEvents", Bytes: 300, Rows: 9, Queries: 3},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("drain = %+v", got)
	}
	if again := ledger.Drain(3); again.Bytes != 0 || again.Rows != 0 || again.Queries != 0 || again.Labels != 0 || len(again.Top) != 0 {
		t.Fatalf("second drain = %+v", again)
	}
	if empty := ledger.Drain(-1); len(empty.Top) != 0 {
		t.Fatalf("negative top = %+v", empty)
	}
	var none *ReadLedger
	if got := none.Drain(5); !reflect.DeepEqual(got, ReadInterval{}) || none.Account("x") != nil {
		t.Fatalf("nil ledger = %+v", got)
	}
}

// TestReadLedgerConcurrentAdditions reads through counting connections from
// many goroutines while drains run; the drained sums equal the bytes written.
func TestReadLedgerConcurrentAdditions(t *testing.T) {
	const writers, chunk, chunks = 8, 1024, 64
	ledger := NewReadLedger()
	var drained sync.Mutex
	var bytes, queries, rows uint64
	collect := func() {
		interval := ledger.Drain(writers + 1)
		drained.Lock()
		bytes += interval.Bytes
		queries += interval.Queries
		rows += interval.Rows
		drained.Unlock()
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range writers {
		server, client := net.Pipe()
		counted := ReadConnOf(ledger.WrapConn(client))
		label := string(rune('a' + i))
		wg.Add(2)
		go func() {
			defer wg.Done()
			defer server.Close()
			<-start
			payload := make([]byte, chunk)
			for range chunks {
				if _, err := server.Write(payload); err != nil {
					t.Error(err)
					return
				}
			}
		}()
		go func() {
			defer wg.Done()
			defer counted.Close()
			<-start
			buffer := make([]byte, chunk)
			for range chunks {
				counted.Begin(label)
				if _, err := io.ReadFull(counted, buffer); err != nil {
					t.Error(err)
					return
				}
				counted.End(1, 1)
			}
		}()
	}
	stop := make(chan struct{})
	drainerDone := make(chan struct{})
	go func() {
		defer close(drainerDone)
		for {
			select {
			case <-stop:
				return
			default:
				collect()
			}
		}
	}()
	close(start)
	wg.Wait()
	close(stop)
	<-drainerDone
	collect()
	if bytes != writers*chunk*chunks || queries != writers*chunks || rows != writers*chunks {
		t.Fatalf("drained bytes %d queries %d rows %d", bytes, queries, rows)
	}
}

func TestReadConnAttributesOperations(t *testing.T) {
	ledger := NewReadLedger()
	server, client := net.Pipe()
	defer server.Close()
	counted := ReadConnOf(ledger.WrapConn(client))
	defer counted.Close()
	write := func(n int) {
		go func() { _, _ = server.Write(make([]byte, n)) }()
		if _, err := io.ReadFull(counted, make([]byte, n)); err != nil {
			t.Fatal(err)
		}
	}
	write(11) // before any operation: connection setup
	counted.Begin("GetTask")
	write(500)
	counted.End(1, 3)
	write(7)          // after the operation: a notification or ping
	counted.End(1, 1) // End without Begin records nothing
	got := ledger.Drain(5)
	want := ReadInterval{Bytes: 518, Rows: 3, Queries: 1, Labels: 2, Top: []ReadTotal{
		{Label: "GetTask", Bytes: 500, Rows: 3, Queries: 1},
		{Label: UnlabeledRead, Bytes: 18},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("drain = %+v", got)
	}
	var none *ReadConn
	none.Begin("x")
	none.End(1, 1)
}

func TestReadConnForwardsSyscallConnOnlyWhenPresent(t *testing.T) {
	ledger := NewReadLedger()
	_, pipe := net.Pipe()
	if _, ok := ledger.WrapConn(pipe).(syscall.Conn); ok {
		t.Fatal("a pipe wrapper claims syscall.Conn")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		if conn, acceptErr := listener.Accept(); acceptErr == nil {
			conn.Close()
		}
	}()
	tcp, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	wrapped := ledger.WrapConn(tcp)
	defer wrapped.Close()
	raw, ok := wrapped.(syscall.Conn)
	if !ok {
		t.Fatal("a TCP wrapper hides syscall.Conn")
	}
	if _, err := raw.SyscallConn(); err != nil {
		t.Fatal(err)
	}
	if ReadConnOf(wrapped) == nil {
		t.Fatal("the TCP wrapper is not found")
	}
	if ReadConnOf(tcp) != nil || ReadConnOf(nil) != nil {
		t.Fatal("an uncounted connection reports a wrapper")
	}
	var none *ReadLedger
	if none.WrapConn(tcp) != tcp {
		t.Fatal("a nil ledger wrapped the connection")
	}
}

func TestReadConnOfFindsWrapperUnderTLS(t *testing.T) {
	ledger := NewReadLedger()
	_, pipe := net.Pipe()
	wrapped := ledger.WrapConn(pipe)
	client := tls.Client(wrapped, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // never handshakes
	if ReadConnOf(client) != ReadConnOf(wrapped) || ReadConnOf(client) == nil {
		t.Fatal("the wrapper under TLS is not found")
	}
}

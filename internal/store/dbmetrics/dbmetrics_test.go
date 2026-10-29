package dbmetrics

import (
	"context"
	"math"
	"sync"
	"testing"
	"time"
	"unsafe"
)

func TestCollectorIsolation(t *testing.T) {
	first, second := NewCollector(), NewCollector()
	firstCtx := WithCollector(context.Background(), first)
	secondCtx := WithCollector(context.Background(), second)
	FromContext(firstCtx).AddQuery(3 * time.Millisecond)
	FromContext(firstCtx).AddAcquire(time.Millisecond)
	FromContext(secondCtx).AddQuery(5 * time.Millisecond)
	if got := first.Snapshot(); got != (Snapshot{Queries: 1, QueryDuration: 3 * time.Millisecond, AcquireDuration: time.Millisecond}) {
		t.Fatalf("first = %+v", got)
	}
	if got := second.Snapshot(); got != (Snapshot{Queries: 1, QueryDuration: 5 * time.Millisecond}) {
		t.Fatalf("second = %+v", got)
	}
	if FromContext(context.Background()) != nil {
		t.Fatal("a context without a collector must yield nil")
	}
}

func TestCollectorConcurrentAccumulation(t *testing.T) {
	const workers, perWorker = 16, 1000
	c := NewCollector()
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range perWorker {
				c.AddQuery(time.Microsecond)
				c.AddAcquire(2 * time.Microsecond)
			}
		}()
	}
	close(start)
	wg.Wait()
	want := Snapshot{Queries: workers * perWorker, QueryDuration: workers * perWorker * time.Microsecond, AcquireDuration: 2 * workers * perWorker * time.Microsecond}
	if got := c.Snapshot(); got != want {
		t.Fatalf("snapshot = %+v, want %+v", got, want)
	}
}

func TestCollectorSaturates(t *testing.T) {
	c := NewCollector()
	c.queries.Store(math.MaxUint64 - 1)
	c.AddQuery(math.MaxInt64)
	c.AddQuery(time.Hour)
	c.AddQuery(time.Hour)
	c.AddAcquire(math.MaxInt64 - 1)
	c.AddAcquire(time.Hour)
	c.AddAcquire(-time.Hour)
	got := c.Snapshot()
	if got.Queries != math.MaxUint64 || got.QueryDuration != math.MaxInt64 || got.AcquireDuration != math.MaxInt64 {
		t.Fatalf("saturated snapshot = %+v", got)
	}
	negative := NewCollector()
	negative.AddQuery(-time.Second)
	negative.AddAcquire(-time.Second)
	if got := negative.Snapshot(); got != (Snapshot{Queries: 1}) {
		t.Fatalf("negative durations must add zero, got %+v", got)
	}
}

func TestAddQueryDurationAddsNoQuery(t *testing.T) {
	c := NewCollector()
	c.AddQueryDuration(4 * time.Millisecond)
	c.AddQueryDuration(-time.Millisecond)
	c.AddQuery(time.Millisecond)
	if got := c.Snapshot(); got != (Snapshot{Queries: 1, QueryDuration: 5 * time.Millisecond}) {
		t.Fatalf("snapshot = %+v", got)
	}
	c.queryNanos.Store(math.MaxInt64 - 1)
	c.AddQueryDuration(time.Hour)
	if got := c.Snapshot(); got.Queries != 1 || got.QueryDuration != math.MaxInt64 {
		t.Fatalf("saturated snapshot = %+v", got)
	}
}

func TestNilCollectorIsNoOp(t *testing.T) {
	var c *Collector
	c.AddQuery(time.Second)
	c.AddQueryDuration(time.Second)
	c.AddAcquire(time.Second)
	if got := c.Snapshot(); got != (Snapshot{}) {
		t.Fatalf("nil snapshot = %+v", got)
	}
	if FromContext(WithCollector(context.Background(), nil)) != nil {
		t.Fatal("a nil collector must stay nil")
	}
}

type measurer Capability

func (m measurer) RequestMetrics() Capability { return Capability(m) }

func TestCapabilityOf(t *testing.T) {
	if got := CapabilityOf(measurer(QueryCount | QueryDuration)); got != QueryCount|QueryDuration {
		t.Fatalf("capability = %b", got)
	}
	if got := CapabilityOf(struct{}{}); got != 0 {
		t.Fatalf("unsupported capability = %b", got)
	}
	if got := CapabilityOf(nil); got != 0 {
		t.Fatalf("nil capability = %b", got)
	}
}

func TestCollectorSizeIsConstant(t *testing.T) {
	if size := unsafe.Sizeof(Collector{}); size != 24 {
		t.Fatalf("collector size = %d bytes, want three 8-byte counters", size)
	}
	c := NewCollector()
	allocations := testing.AllocsPerRun(10, func() {
		for range 100_000 {
			c.AddQuery(time.Microsecond)
			c.AddAcquire(time.Microsecond)
		}
		_ = c.Snapshot()
	})
	if allocations != 0 {
		t.Fatalf("recording allocated %.0f times", allocations)
	}
}

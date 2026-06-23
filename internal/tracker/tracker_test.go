package tracker

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeStore records BatchIncrementClicks calls for assertions.
type fakeStore struct {
	mu     sync.Mutex
	calls  [][]string
	err    error
	delay  time.Duration
}

func (f *fakeStore) BatchIncrementClicks(ctx context.Context, codes []string) error {
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	cp := make([]string, len(codes))
	copy(cp, codes)
	f.calls = append(f.calls, cp)
	return nil
}

func (f *fakeStore) totalCodes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		n += len(c)
	}
	return n
}

func TestTracker_BatchesBySize(t *testing.T) {
	t.Parallel()
	store := &fakeStore{}
	tr := New(store, nil, Config{
		Workers:       1,
		BufferSize:    100,
		BatchSize:     10,
		FlushInterval: time.Hour, // disable time-based flush
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tr.Run(ctx)

	const N = 25
	for i := 0; i < N; i++ {
		tr.Record(Event{Code: "c", Timestamp: time.Now()})
	}

	shCtx, shCancel := context.WithTimeout(context.Background(), time.Second)
	defer shCancel()
	if err := tr.Shutdown(shCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	if got := tr.Metrics.Recorded.Load(); got != N {
		t.Errorf("Recorded: got %d, want %d", got, N)
	}
	if got := tr.Metrics.Persisted.Load(); got != N {
		t.Errorf("Persisted: got %d, want %d", got, N)
	}
	if got := store.totalCodes(); got != N {
		t.Errorf("store saw %d codes, want %d", got, N)
	}
}

func TestTracker_FlushesByTime(t *testing.T) {
	t.Parallel()
	store := &fakeStore{}
	tr := New(store, nil, Config{
		Workers:       1,
		BufferSize:    100,
		BatchSize:     1000,                // very high -> never batch-size flush
		FlushInterval: 50 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tr.Run(ctx)

	for i := 0; i < 3; i++ {
		tr.Record(Event{Code: "x"})
	}
	time.Sleep(150 * time.Millisecond) // > 1 flush interval

	shCtx, shCancel := context.WithTimeout(context.Background(), time.Second)
	defer shCancel()
	if err := tr.Shutdown(shCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	if got := tr.Metrics.Persisted.Load(); got != 3 {
		t.Errorf("Persisted: got %d, want 3", got)
	}
}

func TestTracker_DropsOnFullBuffer(t *testing.T) {
	t.Parallel()
	// Use a tiny buffer and no workers to guarantee the channel is full.
	store := &fakeStore{}
	tr := New(store, nil, Config{
		Workers:       0, // no workers means nothing drains
		BufferSize:    2,
		BatchSize:     1,
		FlushInterval: time.Hour,
	})
	// Manually fill the channel without starting workers.
	tr.events <- Event{Code: "a"}
	tr.events <- Event{Code: "b"}

	// Next record should drop.
	tr.Record(Event{Code: "c"})
	if got := tr.Metrics.Dropped.Load(); got != 1 {
		t.Errorf("Dropped: got %d, want 1", got)
	}
}

func TestTracker_StoreErrorRecorded(t *testing.T) {
	t.Parallel()
	store := &fakeStore{err: errors.New("db down")}
	tr := New(store, nil, Config{
		Workers:       1,
		BufferSize:    10,
		BatchSize:     2,
		FlushInterval: time.Hour,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tr.Run(ctx)

	tr.Record(Event{Code: "a"})
	tr.Record(Event{Code: "b"})

	shCtx, shCancel := context.WithTimeout(context.Background(), time.Second)
	defer shCancel()
	if err := tr.Shutdown(shCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	if got := tr.Metrics.Failed.Load(); got != 1 {
		t.Errorf("Failed: got %d, want 1", got)
	}
	if got := tr.Metrics.Persisted.Load(); got != 0 {
		t.Errorf("Persisted: got %d, want 0", got)
	}
}

func TestTracker_ConcurrentRecord(t *testing.T) {
	t.Parallel()
	store := &fakeStore{}
	tr := New(store, nil, Config{
		Workers:       4,
		BufferSize:    1024,
		BatchSize:     25,
		FlushInterval: 100 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tr.Run(ctx)

	const writers = 16
	const perWriter = 200
	var wg sync.WaitGroup
	var recorded atomic.Int64
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				tr.Record(Event{Code: "code", Timestamp: time.Now()})
				recorded.Add(1)
			}
		}(w)
	}
	wg.Wait()

	shCtx, shCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shCancel()
	if err := tr.Shutdown(shCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	// Recorded = accepted; may be < writers*perWriter if any were dropped.
	// We don't assert == writers*perWriter, but the persisted count
	// must match whatever was recorded.
	if got := tr.Metrics.Persisted.Load(); got != tr.Metrics.Recorded.Load() {
		t.Errorf("Persisted (%d) != Recorded (%d) — events lost in tracker",
			got, tr.Metrics.Recorded.Load())
	}
	if tr.Metrics.Persisted.Load() == 0 {
		t.Errorf("expected at least some events persisted")
	}
}

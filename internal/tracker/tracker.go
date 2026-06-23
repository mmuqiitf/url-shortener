// Package tracker is the click-tracking worker pool.
//
// The redirect handler is the hottest path in a URL shortener. Doing
// "UPDATE links SET clicks = clicks + 1" synchronously on every hit
// serializes all redirects on the SQLite write lock, capping
// throughput. The tracker decouples write latency from request
// latency: the handler pushes an event onto a buffered channel and
// returns; a pool of worker goroutines drains the channel in batches.
//
// Design highlights (the full walkthrough is in docs/CONCURRENCY.md):
//
//   - Buffered channel = bounded queue, natural backpressure
//   - non-blocking enqueue with `select { case ch <- ev: default: }` so
//     a full queue drops events instead of stalling the redirect
//   - WaitGroup waits for workers to drain on shutdown
//   - context.Context is the cancellation primitive for graceful exit
//   - batched DB writes amortize the cost across many events
package tracker

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Event describes a single redirect hit. The fields are best-effort —
// the tracker does not retry failures.
type Event struct {
	Code      string
	Timestamp time.Time
	IP        string
	UserAgent string
	Referer   string
}

// Store is the persistence interface the tracker depends on.
// Defined here (consumer-side) so the tracker can be unit-tested
// against an in-memory fake.
type Store interface {
	BatchIncrementClicks(ctx context.Context, codes []string) error
}

// Config tunes the worker's runtime behavior.
type Config struct {
	Workers       int           // number of worker goroutines
	BufferSize    int           // capacity of the events channel
	BatchSize     int           // flush after this many events
	FlushInterval time.Duration // …or after this much time, whichever first
}

// Metrics is the tracker's observability surface, populated atomically.
// Tests assert against it; production code can scrape it.
type Metrics struct {
	Recorded    atomic.Int64 // events accepted into the queue
	Dropped     atomic.Int64 // events dropped because the queue was full
	Persisted   atomic.Int64 // events successfully written to the store
	Failed      atomic.Int64 // batch writes that returned an error
	Batches     atomic.Int64 // total batch writes attempted
}

// Tracker owns the worker pool and the events channel.
type Tracker struct {
	cfg     Config
	store   Store
	log     *slog.Logger
	events  chan Event
	wg      sync.WaitGroup
	Metrics Metrics
}

// New creates a Tracker. It does not start the workers — call Run.
func New(store Store, log *slog.Logger, cfg Config) *Tracker {
	if log == nil {
		log = slog.Default()
	}
	if cfg.Workers < 1 {
		cfg.Workers = 1
	}
	if cfg.BufferSize < 1 {
		cfg.BufferSize = 1024
	}
	if cfg.BatchSize < 1 {
		cfg.BatchSize = 50
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = time.Second
	}
	return &Tracker{
		cfg:    cfg,
		store:  store,
		log:    log.With("component", "tracker"),
		events: make(chan Event, cfg.BufferSize),
	}
}

// Run starts the worker pool. It returns immediately; the workers
// exit when ctx is cancelled AND the events channel is drained.
func (t *Tracker) Run(ctx context.Context) {
	for i := 0; i < t.cfg.Workers; i++ {
		t.wg.Add(1)
		go t.worker(ctx, i)
	}
}

// Record enqueues an event. Non-blocking: if the queue is full, the
// event is dropped and a metric is incremented.
//
// We chose drop-on-full over block-on-full because blocking on the
// redirect path would couple user-visible latency to the write path.
// The alternative — a much larger buffer — costs memory and can hide
// sustained back-pressure from operators.
func (t *Tracker) Record(ev Event) {
	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now().UTC()
	}
	select {
	case t.events <- ev:
		t.Metrics.Recorded.Add(1)
	default:
		t.Metrics.Dropped.Add(1)
		t.log.Warn("click event dropped: queue full",
			"code", ev.Code, "buffer", t.cfg.BufferSize)
	}
}

// Shutdown closes the events channel and waits for workers to finish
// processing whatever is already buffered. It honours ctx's deadline —
// if the deadline expires before the workers drain, it returns the
// context error and leaves the workers running (they will exit on
// their own when the context is cancelled by the caller).
func (t *Tracker) Shutdown(ctx context.Context) error {
	close(t.events)
	done := make(chan struct{})
	go func() { t.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// worker is the main loop of a single tracker goroutine. It batches
// incoming events and flushes them to the store on either size or
// time threshold.
//
// Important: the worker's `ctx` is used only to drive the select
// loop. The actual store call uses a fresh context with a short
// timeout, so a root-context cancellation (graceful shutdown) does
// not cause the in-flight batch to fail.
func (t *Tracker) worker(ctx context.Context, id int) {
	defer t.wg.Done()
	log := t.log.With("worker", id)

	batch := make([]Event, 0, t.cfg.BatchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		codes := make([]string, len(batch))
		for i, ev := range batch {
			codes[i] = ev.Code
		}
		t.Metrics.Batches.Add(1)
		// Use a fresh context with a bounded timeout so a
		// cancelled root context (e.g. SIGTERM) does not abort
		// the in-flight write.
		writeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := t.store.BatchIncrementClicks(writeCtx, codes)
		cancel()
		if err != nil {
			t.Metrics.Failed.Add(1)
			log.Error("batch increment failed", "err", err, "size", len(batch))
		} else {
			t.Metrics.Persisted.Add(int64(len(batch)))
		}
		batch = batch[:0]
	}

	ticker := time.NewTicker(t.cfg.FlushInterval)
	defer ticker.Stop()

	for {
		select {
		case ev, ok := <-t.events:
			if !ok {
				// channel closed: drain any remaining events first,
				// then do a final flush, then exit.
				for ev := range t.events {
					batch = append(batch, ev)
					if len(batch) >= t.cfg.BatchSize {
						flush()
					}
				}
				flush()
				return
			}
			batch = append(batch, ev)
			if len(batch) >= t.cfg.BatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-ctx.Done():
			// On cancellation we still flush the in-flight batch,
			// then return. The store call uses its own context so
			// the write completes even if rootCtx is cancelled.
			flush()
			log.Info("worker exiting on context cancel")
			return
		}
	}
}

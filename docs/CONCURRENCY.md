# Concurrency in this project

A guided tour of every concurrency primitive this project uses, with
concrete pointers to the code. The aim is intermediate depth — enough
that a reader coming from Node.js, Python, or PHP can reason about
*what* each pattern does, *why* we picked it, and *what would go wrong*
without it.

> Numbers in the form `file.go:42` point into the actual source so you
> can read along.

---

## 1. The Go runtime model: M, G, P

Before talking about goroutines, it helps to know what they are.

The Go runtime uses an **M:N scheduler** that maps goroutines (G) onto
OS threads (M) via a fixed pool of logical processors (P). At any
instant, each P holds a local run-queue of goroutines ready to run on
the M attached to it. When a G blocks on I/O (network read, sleep, mutex
contention), the runtime parks the M and starts another G on a
different M — so I/O does not stall the system the way a single-threaded
event loop would.

```
    P  (logical processor, default = GOMAXPROCS = NumCPU)
    │
    ├── M (OS thread) ── G (goroutine, running)
    │
    ├── M ── G  (ready in local run-queue)
    │
    └── global run-queue ── G G G (overflow, work stealing)
```

**Why this matters here:** the redirect path blocks on a channel send
(the tracker queue) and on disk I/O (SQLite). The scheduler can run
other goroutines on the same M while the first waits. You don't need
`async`/`await` keywords to get the same property — Go gives it to you
implicitly for any blocking call.

`GOMAXPROCS` defaults to `runtime.NumCPU()`. You can change it with the
`GOMAXPROCS` env var.

---

## 2. The basics we use

### 2.1 Goroutines

A goroutine is a `go` statement plus a function call:

```go
// cmd/server/main.go:97
serverErr := make(chan error, 1)
go func() {
    log.Info("http server listening", "addr", srv.Addr)
    if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
        serverErr <- err
    }
    close(serverErr)
}()
```

Goroutines are cheap (~2KB stack initial, grows as needed). Spawning
thousands is normal. But they are not free, and a goroutine that
finishes its work without a clear owner is a leak.

### 2.2 Channels

A channel is a typed, concurrent-safe queue:

```go
// internal/tracker/tracker.go:94
events: make(chan Event, cfg.BufferSize),
```

The integer is the **buffer**. A buffered channel accepts up to N sends
without a corresponding receive. This is what gives the tracker queue
its natural backpressure — the redirect handler can hand the event off
in O(1) and return.

Channels can be **closed**. After a `close(ch)`, receives yield the zero
value and `ok=false`, and sends panic. Closing is a one-shot
*signalling* primitive; it's how the tracker tells the workers "no
more events" without killing them:

```go
// internal/tracker/tracker.go:134
func (t *Tracker) Shutdown(ctx context.Context) error {
    close(t.events)   // workers will see !ok and exit after draining
    ...
}
```

**Pitfall:** never close a channel from the receiving side, and never
send on a closed channel. Both are panics.

### 2.3 `select`

A `select` waits on multiple channel operations. It's the only way to
do timeouts and "best-effort" sends:

```go
// internal/tracker/tracker.go:118
select {
case t.events <- ev:
    t.Metrics.Recorded.Add(1)
default:
    t.Metrics.Dropped.Add(1)
    t.log.Warn("click event dropped: queue full", ...)
}
```

The `default` branch makes the send **non-blocking**. If the queue is
full, we drop the event and increment a counter instead of stalling
the redirect handler. This is the **drop-on-full** backpressure
strategy.

The other `select` we use has a timeout:

```go
// internal/tracker/tracker.go:136
select {
case <-done:
    return nil
case <-ctx.Done():
    return ctx.Err()
}
```

This is how the tracker.Shutdown method honours a deadline: it races
the wait group against the context, and returns whichever finishes
first.

### 2.4 `sync.WaitGroup`

A `WaitGroup` is a counter that tracks in-flight goroutines:

```go
// internal/tracker/tracker.go:101
func (t *Tracker) Run(ctx context.Context) {
    for i := 0; i < t.cfg.Workers; i++ {
        t.wg.Add(1)
        go t.worker(ctx, i)
    }
}

// internal/tracker/tracker.go:149
func (t *Tracker) worker(ctx context.Context, id int) {
    defer t.wg.Done()
    ...
}

// internal/tracker/tracker.go:135
done := make(chan struct{})
go func() { t.wg.Wait(); close(done) }()
```

**`Add` must be called before the goroutine starts.** Calling `Add`
inside the goroutine is a race: the parent might already have called
`Wait` and returned zero. The `Run` method here adds *before* `go
t.worker(...)`, which is the safe pattern.

### 2.5 `context.Context`

`context.Context` is a cancellation and request-scoped value carrier.
The pattern in this project:

```go
// cmd/server/main.go:78
rootCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
defer stop()
```

`rootCtx` is cancelled when the process receives SIGINT or SIGTERM.
We pass it into both `tr.Run(rootCtx)` and as the parent of every
per-request `ctx` in the handlers.

Every blocking call we make honours `ctx`:

- `db.ExecContext(ctx, ...)` / `db.QueryRowContext(ctx, ...)`
- The tracker's worker loop has a `case <-ctx.Done():` branch
- `srv.Shutdown(ctx)` stops accepting new requests and waits for
  in-flight ones

If the client disconnects mid-request, the request `ctx` is cancelled
and the SQL call returns early. This is what makes graceful shutdown
work without zombie queries.

### 2.6 `sync/atomic`

For a single counter, a mutex is overkill. `sync/atomic` gives us
lock-free increments:

```go
// internal/tracker/tracker.go:55
type Metrics struct {
    Recorded    atomic.Int64
    Dropped     atomic.Int64
    Persisted   atomic.Int64
    Failed      atomic.Int64
    Batches     atomic.Int64
}
```

`atomic.Int64.Add(1)` compiles down to a single instruction. Reads are
just `.Load()`. **The race detector still applies** — `atomic.Int64`
instruments correctly under `-race`, so this is a race-free way to
share counters across goroutines.

### 2.7 `sync.Mutex` (we use it minimally)

This project does not have a long-running `Mutex` — SQLite serialises
writes for us, and the tracker's only shared state is a channel. But
the pattern matters and shows up in tests and helpers. If you want to
add a per-IP rate limiter, it would look like:

```go
type Limiter struct {
    mu       sync.Mutex
    buckets  map[string]*token.Bucket
}

func (l *Limiter) Allow(ip string) bool {
    l.mu.Lock()
    defer l.mu.Unlock()
    b, ok := l.buckets[ip]
    if !ok { b = token.New(10, time.Minute); l.buckets[ip] = b }
    return b.Take()
}
```

Read the `sync` package docs carefully — `sync.Mutex` is not
re-entrant. Use `sync.RWMutex` if you have many concurrent readers.

---

## 3. The click-tracker worker pool (the centerpiece)

A URL shortener's hottest path is `GET /{code}`. Doing
`UPDATE links SET clicks = clicks + 1` synchronously on every hit means
N redirects = N write transactions = N SQLite write-lock acquisitions.
Under load the write lock is the bottleneck.

The fix is to **decouple write latency from request latency**: the
handler pushes a small struct onto a channel and returns. A pool of
worker goroutines drains the channel in batches.

### The full design (`internal/tracker/tracker.go`)

```
HTTP redirect handler
       │
       │ tracker.Record(Event)
       ▼
 ┌─────────────────┐         size or time threshold
 │ events channel  │ ◄────────────────────────┐
 │  (buf 4096)     │                          │
 └────────┬────────┘                          │
          │                                   │
          │   ┌────────┐ ┌────────┐ ┌────────┐│
          └──►│worker 0│ │worker 1│ │worker N││
              └────┬───┘ └────┬───┘ └────┬───┘│
                   │          │          │    │
                   └──────────┼──────────┘    │
                              ▼               │
                       BatchIncrementClicks ──┘
                              │
                              ▼
                         SQLite (one tx)
```

### What each pattern buys us here

| Pattern | Where | What it gives us |
|---|---|---|
| Buffered channel | `tracker.go:94` | Bounded queue. The handler never blocks on the send for more than a few microseconds. |
| `select` with `default` | `tracker.go:118` | Non-blocking send. If the queue is full, we drop and increment a metric instead of stalling. |
| `WaitGroup` | `tracker.go:101, 149` | `Shutdown` knows when every worker has exited. |
| `context.Context` | `tracker.go:148, 195` | Workers exit promptly on shutdown. The store call uses the same context. |
| `time.Ticker` | `tracker.go:171` | Time-based flush — we never sit on a partial batch longer than `FlushInterval`. |
| `atomic.Int64` | `tracker.go:55` | Lock-free metrics safe to read from anywhere. |
| Batched `UPDATE` in a transaction | `repository/sqlite.go` | One write lock acquisition per batch, not per event. |

### What could go wrong without each pattern

- **Unbuffered channel:** every redirect would block until a worker
  picked it up. Under a burst, requests queue up behind the worker
  loop and p99 latency explodes.
- **`select` with `default` (drop-on-full):** the alternative is
  block-on-full. We chose drop because the *user-visible* metric is
  redirect latency, not click-counter accuracy. Dropped events are
  visible in `tracker.Metrics.Dropped`.
- **`WaitGroup`:** without it, `Shutdown` has no way to know when the
  workers actually finished. The defer in `main.go` would `Close` the
  database while a worker is still inside `tx.ExecContext`.
- **Batched `UPDATE`:** doing one `UPDATE` per event would have us
  burning the SQLite write lock for every redirect. The whole point of
  the pool is to collapse the writes.
- **Aggregation in `BatchIncrementClicks`:** a naive
  `UPDATE links SET clicks = clicks + 1 WHERE code = ?` per event
  also works, but a batched `UPDATE` is O(1) write lock acquisitions
  per batch, not per event.

### The shutdown dance

`tracker.Shutdown` (`tracker.go:133`) does three things in order:

1. `close(t.events)` — signal "no more events coming in".
2. `go func() { t.wg.Wait(); close(done) }()` — race the wait group
   against a context.
3. Return whichever of `done` or `ctx.Done()` fires first.

The workers' loop is the key bit:

```go
// internal/tracker/tracker.go:175
for {
    select {
    case ev, ok := <-t.events:
        if !ok {
            // channel closed: drain any remaining events first,
            // then do a final flush, then exit.
            for ev := range t.events {  // <- drain the buffer
                batch = append(batch, ev)
                if len(batch) >= t.cfg.BatchSize {
                    flush()
                }
            }
            flush()
            return
        }
        ...
    case <-ticker.C:    flush()
    case <-ctx.Done():  flush(); return
    }
}
```

When the channel is closed and the buffer is drained, the inner
`for ev := range t.events` exits naturally. Then we do one last
`flush()` (a partial batch is still valuable) and return. The
`defer t.wg.Done()` runs, the wait group counter hits zero, and
`Shutdown` returns.

**The trick:** `close` is what makes the worker's loop terminate
*without* relying on the context. If we relied only on
`<-ctx.Done()`, a worker that was blocked in `t.store.BatchIncrementClicks`
would not exit until that store call returns. The `close(t.events)`
+ drain pattern makes the worker exit even if the store is slow,
because once the channel is closed and drained, the only way back into
the loop is via the ticker or the context — and even if the context
is cancelled, we've already drained the buffer.

---

## 4. Graceful shutdown of the whole server

`cmd/server/main.go` orchestrates shutdown in this exact order:

```
1. signal received (SIGINT or SIGTERM)
2. rootCtx cancelled by signal.NotifyContext
3. main returns from the select on rootCtx.Done()
4. server.Shutdown(shutCtx)        — stop accepting, drain in-flight HTTP
5. tracker.Shutdown(trackerCtx)   — drain click events
6. defer repo.Close()             — close the DB
```

**Why this order?**

- We shut down the **HTTP server first** so we stop producing new
  click events.
- Then the **tracker** so the events that *were* in flight get
  persisted.
- Then the **DB** so the tracker is not still trying to write.

If you reversed the order, you'd either lose in-flight click events
(the DB is already closed when the tracker tries to write) or accept
new HTTP requests after starting to shut down.

**The two timeouts matter.** `server.Shutdown` and
`tracker.Shutdown` each take their own `context.WithTimeout`. If
`server.Shutdown` hangs (e.g. a long-lived request that refuses to
exit), the tracker shutdown still gets a chance to run.

---

## 5. The race detector

`go test -race` instruments the binary with ThreadSanitizer. It
catches data races — two goroutines accessing the same memory with at
least one of them writing, with no synchronisation between them. The
overhead is roughly 5–10× and memory usage 5–20×, which is why you run
it in CI but not in production.

`make test-race` runs the full suite under the detector.

**Worked example.** A common bug we explicitly avoided in the tracker:

```go
// BROKEN
type Metrics struct {
    Recorded int64
    Persisted int64
}

func (m *Metrics) IncRecorded() { m.Recorded++ }     // data race
func (m *Metrics) IncPersisted(n int) { m.Persisted += int64(n) }  // data race
```

If two goroutines call `IncRecorded` and `IncPersisted` simultaneously,
`-race` will report:

```
WARNING: DATA RACE
Write by goroutine 7:
  /tmp/track.go:18  +0x44
Read by goroutine 8:
  /tmp/track.go:18  +0x64
```

The fix is to use `atomic.Int64`:

```go
// FIXED
type Metrics struct {
    Recorded  atomic.Int64
    Persisted atomic.Int64
}
```

Every package in this project that exposes counters uses `atomic.Int64`.
Tests under `-race` confirm there are no data races.

> **WSL note:** `go test -race` requires CGO, which in turn requires a
> C compiler. If you are on a stripped-down WSL distro without gcc,
> install it (`apt-get install gcc`) or run the race-enabled tests
> inside the Docker image, which ships with a C toolchain. As a
> workaround on this machine we run the test suite with `-count=5` to
> stress concurrent paths; a real data race is usually caught within a
> few iterations.

---

## 6. Concurrency patterns NOT used (and why)

- **Worker pool per (code, time bucket).** Tempting, but the channel
  already gives us ordering. Per-code sharding would help only at
  extreme scale and adds complexity.
- **`sync.Pool` for buffers.** We don't allocate hot-path buffers.
  The events are tiny structs and Go's allocator is fast.
- **Per-IP rate limiter.** Designed but deferred (see
  `docs/PLAN.md` §10). When added, it will use a `sync.Mutex`-protected
  map of token buckets, explained in §2.7 above.
- **Read pool / write pool separation.** SQLite serialises writes for
  us; there's no separate write path that would benefit from a
  dedicated goroutine.

---

## 7. Glossary of this project's idioms

| Idiom | Example in this project | What it means |
|---|---|---|
| `ctx context.Context` first param | every method on `service`, `repository`, `tracker` | "This work is cancellable and has a deadline." |
| `defer` for cleanup | `defer rows.Close()`, `defer tx.Rollback()` | "When this function returns, run this." |
| `errors.Is` / `errors.As` | handler uses `errors.As` to peel off `APIError` | "Walk the error chain and test for a specific value." |
| `slog` | every log line | "Structured, leveled logging with key/value pairs." |
| `//go:embed` | `internal/repository/migrate.go` | "Compile this file into the binary at build time." |
| Consumer-defined interface | `service.Repository`, `tracker.Store` | "Define the interface where it's used, not where it's implemented." |
| `gofmt` + `goimports` | `make fmt` | "Run the standard formatters." |
| `go test -race` | `make test-race` | "Catch data races in CI." |
| `go vet`, `golangci-lint` | `make lint` | "Catch common bugs at lint time." |
| `go mod tidy` | `make tidy` | "Add missing deps, remove unused ones." |
| `internal/` | every package in this project | "External code cannot import this." |
| Multi-stage Docker | `Dockerfile` | "Smallest possible runtime image." |
| `trimpath` + `-ldflags="-s -w"` | `Makefile` | "Reproducible, small binary." |

---

## 8. Reading list

If you want to go deeper:

- The Go blog: *Pipelines and cancellation* and *Share memory by
  communicating*.
- `go.dev/blog/race-detector` for the race detector.
- *Concurrency in Go* by Katherine Cox-Buday (O'Reilly) — the
  definitive intermediate-level book.
- The `sync` and `sync/atomic` package docs.

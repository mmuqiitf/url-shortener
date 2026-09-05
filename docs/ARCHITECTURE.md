# Architecture

## Layer diagram

```
                ┌──────────────────────────────────────┐
HTTP request →  │ middleware: requestid, recover, log, │
                │           cors                      │
                └────────────┬─────────────────────────┘
                             ▼
                ┌──────────────────────────────────────┐
                │  handler (chi routes)                │
                │   - decode JSON                      │
                │   - map errors → APIError            │
                │   - call service                     │
                │   - emit click event on redirect     │
                └────────────┬─────────────────────────┘
                             ▼
                ┌──────────────────────────────────────┐
                │  service.Shortener                   │
                │   - normalize URL                    │
                │   - generate / validate alias        │
                │   - retry on collision               │
                └────────────┬─────────────────────────┘
                             ▼
                ┌──────────────────────────────────────┐
                │  repository (GORM + SQLite)          │
                │   - CRUD on LinkModel                │
                │   - batch increment clicks (GORM tx) │
                └────────────┬─────────────────────────┘
                             ▼
                ┌──────────────────────────────────────┐
                │  SQLite (glebarez/sqlite pure-Go)    │
                └──────────────────────────────────────┘

Background:  tracker ──consumes──► repository.BatchIncrementClicks
```

## Request flow: shorten

1. Chi router matches `POST /api/v1/links`.
2. Middleware chain adds a `X-Request-Id`, recovers from panics, logs the
   request, sets CORS headers.
3. Handler decodes the JSON body into `model.CreateLinkInput`.
4. Handler calls `service.Shortener.Create(ctx, input)`.
5. Service normalizes the URL (`http://`/`https://` only), validates the
   custom alias (if any), and asks the codec for a fresh 7-char base62
   code.
6. Service calls `repository.Create(ctx, link)`. If the code collides,
   the service retries up to 8 times with a new random code.
7. Repository runs `INSERT INTO links ...` and returns the persisted
   `Link` (with `is_active = 1`).
8. Handler renders the response as `{ code, short_url, long_url, ... }`
   with status `201 Created`.

## Request flow: redirect

1. Chi router matches `GET /{code}`.
2. Middleware chain runs.
3. Handler calls `service.Shortener.Resolve(ctx, code)`.
4. Service asks the repository for the row. If the row is missing,
   inactive, or expired, the service returns a typed `APIError` that
   the handler maps to `404` or `410`.
5. On success, the handler calls `tracker.Record(Event{Code, ...})` —
   this is **non-blocking**: it enqueues onto a buffered channel and
   returns immediately. Workers batch the writes.
6. Handler returns `301 Moved Permanently` with `Location: <long_url>`.

## Package responsibilities

| Package | Responsibility |
|---|---|
| `config` | Reads env vars, applies defaults, validates. The only place that touches `os.Getenv`. |
| `model` | Domain types and the `APIError` type with an `Is()` method so `errors.Is` works across clones. |
| `codec` | Base62 alphabet, random short-code generation via `crypto/rand`, regex validation. |
| `repository` | All SQL. Defines the schema and runs embedded migrations. Implements the interfaces consumed by `service` and `tracker`. |
| `service` | Business rules: URL normalization, alias validation, collision retry, TTL filtering. |
| `tracker` | Click-event worker pool. The concurrency centerpiece. |
| `handler` | HTTP boundary. Decodes requests, encodes responses, maps errors. |
| `middleware` | Cross-cutting: request-id, structured logging, panic recovery, CORS. |
| `cmd/server` | Composition root. Wires everything together and handles graceful shutdown. |

## Go-specific patterns

- **Errors as values.** `APIError` is a struct, not a panic. Each layer
  wraps with `fmt.Errorf("...: %w", err)` so the call chain is
  inspectable. The handler does `errors.As` to peel the typed error off
  the chain and render it as a JSON envelope with the right status code.

- **Context propagation.** Every layer takes `ctx context.Context` as
  its first parameter. The request context carries the request id and
  is cancelled when the client disconnects. The root context is
  cancelled on SIGINT/SIGTERM.

- **Consumer-defined interfaces.** `service` defines the `Repository`
  interface it needs; the `repository` package implements it
  structurally without importing `service`. Same for `tracker.Store`.
  This makes the project easy to test with in-memory fakes — see
  `service/shortener_test.go` for an example.

- **Internal packages.** Every package is under `internal/`, so external
  code cannot import them. This is the conventional way to draw an
  API boundary in Go.

- **AutoMigrate with GORM.** The `sqlite.go` repository uses
  GORM's `AutoMigrate(&LinkModel{})` to automatically maintain table
  schemas, column types, and indexes on startup.

- **`slog` for logs.** `log/slog` (stdlib, Go 1.21+) gives us
  structured JSON logs without a third-party dependency. Every log
  line has key/value pairs.

- **Table-driven tests.** `codec`, `service`, and `tracker` tests all
  use the `tests := []struct{...}{...}` + `t.Run` pattern. The
  rationale is in `docs/DEVELOPMENT.md`.

## Data lifecycle

```
        ┌────────────────────┐
POST →  │ links (write)      │
        └────────┬───────────┘
                 │
GET  /{code}  ──►│ read + check is_active/expires_at
                 │
        ┌────────▼───────────┐
        │ events channel     │  (in-memory, bounded, drop on full)
        └────────┬───────────┘
                 │
        ┌────────▼───────────┐
        │ worker goroutines  │  batched UPDATEs
        └────────┬───────────┘
                 │
        ┌────────▼───────────┐
        │ links.clicks++     │
        └────────────────────┘
```

The redirect path is **never** blocked on the click write. The cost is
that we may drop events under extreme load — the trade-off is
documented in `internal/tracker/tracker.go` and explained in
`docs/CONCURRENCY.md`.

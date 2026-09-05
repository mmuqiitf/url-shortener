# URL Shortener — Project Plan

> Saved as a deliverable so the design rationale stays close to the code.

## Stack & key decisions

| Concern | Choice | Why |
|---|---|---|
| Router | `go-chi/chi/v5` | Idiomatic, stdlib `net/http` compatible, no hidden magic |
| DB | SQLite via `modernc.org/sqlite` | Pure Go (no CGO), cross-compile friendly, perfect for Docker |
| Logging | `log/slog` (stdlib, Go 1.21+) | Structured, leveled — no need for zap/logrus |
| Config | Env vars + small wrapper | 12-factor, simple |
| UUIDs | `github.com/google/uuid` | For request IDs |
| Testing | stdlib `testing` + `httptest` | No framework overhead for a beginner project |
| Linter | `golangci-lint` | De-facto standard |
| Short codes | 7-char base62 random + DB unique-constraint retry | Opaque, scalable, teaches error-handling |
| Features | shorten, redirect, custom alias, click count, TTL expiry | Enough to exercise concurrency meaningfully |
| Concurrency doc | Intermediate depth | goroutines, channels, mutex, WaitGroup, context, race detector, worker pool |

---

## 1. Project layout

```
url-shortener/
├── cmd/
│   └── server/
│       └── main.go                 # entrypoint: wires config → DB → service → router → server
├── internal/
│   ├── config/
│   │   └── config.go               # env loading with defaults
│   ├── cache/
│   │   ├── cache.go                # Cache interface + in-memory LRU
│   │   ├── cache_test.go
│   │   └── redis.go                # Redis distributed cache implementation
│   ├── model/
│   │   └── link.go                 # domain types: Link, CreateLinkRequest, APIError
│   ├── codec/
│   │   ├── base62.go               # random short-code generation + validation
│   │   └── base62_test.go
│   ├── repository/
│   │   ├── sqlite.go               # GORM + SQLite implementation
│   │   └── sqlite_test.go          # temporary SQLite DB tests
│   ├── service/
│   │   ├── shortener.go            # business logic
│   │   └── shortener_test.go
│   ├── tracker/
│   │   ├── tracker.go              # click-event worker pool (concurrency centerpiece)
│   │   └── tracker_test.go
│   ├── handler/
│   │   ├── handler.go              # chi handlers
│   │   └── handler_test.go         # httptest-based integration tests
│   └── middleware/
│       ├── requestid.go
│       ├── logging.go
│       ├── recover.go
│       ├── cors.go
│       ├── ratelimit.go            # token bucket rate limiter
│       └── ratelimit_test.go
├── docs/
│   ├── PLAN.md                     # this file
│   ├── ARCHITECTURE.md
│   ├── CONCURRENCY.md              # the deep-dive doc
│   ├── API.md                      # endpoint reference + curl examples
│   └── DEVELOPMENT.md              # local setup, testing, linting
├── scripts/
│   └── smoke.sh                    # curl-based smoke test
├── .env.example
├── .gitignore
├── .golangci.yml
├── Dockerfile
├── docker-compose.yml
├── Makefile
├── go.mod
├── go.sum
└── README.md
```

**Why this layout?** The `cmd/server` → `handler` → `service` → `repository` → `model` flow is idiomatic Go. It mirrors the layered architecture you're used to from PHP/Laravel/Symfony or Python/Django, but with explicit interfaces defined at the consumer side (Go's "accept interfaces, return structs" rule).

---

## 2. Database schema

Single `links` table, kept deliberately simple:

```sql
CREATE TABLE IF NOT EXISTS links (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    code        TEXT    NOT NULL UNIQUE,
    long_url    TEXT    NOT NULL,
    created_at  TEXT    NOT NULL DEFAULT (datetime('now')),
    expires_at  TEXT,
    clicks      INTEGER NOT NULL DEFAULT 0,
    is_active   INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX IF NOT EXISTS idx_links_code       ON links(code);
CREATE INDEX IF NOT EXISTS idx_links_expires_at ON links(expires_at);
CREATE INDEX IF NOT EXISTS idx_links_is_active  ON links(is_active);
```

Migrations live in `internal/repository/migrations/` and are embedded into the binary via `//go:embed`. They run on startup inside a single transaction. No `golang-migrate` dependency.

---

## 3. API surface (v1)

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/api/v1/links` | Create short link |
| `GET`  | `/api/v1/links/{code}` | Get link metadata |
| `GET`  | `/api/v1/links` | List links (paginated `?limit=&offset=`) |
| `DELETE` | `/api/v1/links/{code}` | Soft-delete (sets `is_active = 0`) |
| `GET`  | `/{code}` | Public redirect → 301 to long URL, fires click event |
| `GET`  | `/healthz` | Liveness (always 200) |
| `GET`  | `/readyz` | Readiness (pings DB) |

**Request example:**
```bash
curl -X POST http://localhost:8080/api/v1/links \
  -H 'Content-Type: application/json' \
  -d '{"url":"https://example.com/very/long/path","custom_alias":"ex","expires_at":"2026-12-31T23:59:59Z"}'
```

**Error contract:** consistent JSON `{ "error": { "code": "INVALID_URL", "message": "..." } }` with appropriate HTTP status codes. Handled in `internal/handler/handler.go` via a small `appError` type + `errors.As`.

---

## 4. Concurrency design (the headline feature)

The plan deliberately puts several real concurrency points in the project, each documented in `docs/CONCURRENCY.md`. The doc explains *what* each pattern does, *why* it's used here, and *what would go wrong* without it.

### 4.1 Concurrent request handling (free)
`http.Server` spawns a goroutine per request automatically. `docs/CONCURRENCY.md` explains the GMP scheduler model briefly so a reader coming from Node/Python understands "Go does not block on I/O the way Node blocks on the event loop" — there's a real OS thread pool underneath.

### 4.2 Click-tracker worker pool (★ the centerpiece)
The redirect path is the hottest route. Doing a synchronous `UPDATE links SET clicks = clicks + 1` per redirect serializes on the SQLite write lock under load.

**Design:**
- `tracker.Tracker` owns a buffered `chan ClickEvent{Code, Timestamp, IP, UserAgent, Referer}`.
- N worker goroutines (`N = runtime.NumCPU()` by default) consume events.
- Workers batch up to N events OR 1 second, then run a single `UPDATE ... CASE WHEN code = ?` to commit.
- Graceful shutdown: `close(events)` + `WaitGroup.Wait()` drains remaining events.

The doc walks through: buffered vs unbuffered channels, the `select` with `default` for backpressure, the trade-off of dropping events vs blocking the redirect, and how the race detector catches misuse.

### 4.3 Rate limiter (out of scope for v1; documented as next step)
The plan defers an in-memory rate limiter to a later iteration. The concurrency doc explains the design so a reader can implement it later: in-memory token bucket keyed by client IP, protected by `sync.Mutex` (kept simple — explained why we don't shard yet). Returns `429 Too Many Requests` when exhausted.

### 4.4 Graceful shutdown
In `cmd/server/main.go`:
1. `signal.NotifyContext` cancels the root context on SIGINT/SIGTERM.
2. `server.Shutdown(ctx)` stops accepting + drains in-flight requests.
3. `tracker.Shutdown(ctx)` flushes click events.
4. DB is closed by the deferred call.
5. Order matters — see `docs/CONCURRENCY.md`.

### 4.5 Race detector
`make test-race` → `go test -race ./...`. The doc explains what `-race` does (Tsan instrumentation, ~5–10× overhead, catches data races at the cost of CI time) and shows a deliberately broken example + its fix.

> **Note:** Running `go test -race` requires CGO. If your environment has no C compiler (e.g. minimal WSL distros), install `gcc` (e.g. `apt-get install gcc`) or run the tests inside the Docker image where the toolchain is present.

---

## 5. Go best practices baked in

These are the rules a beginner in Go needs to internalize, and the project demonstrates each:

| Practice | Where it shows up |
|---|---|
| Errors as values, no exceptions | `APIError` type, `errors.Is`/`errors.As`, wrapping with `fmt.Errorf("...: %w", err)` |
| Context propagation through every layer | `ctx context.Context` is the first param of every repo/service/handler method |
| Interfaces defined at the consumer | `service` defines the `Repository` interface it needs; `repository` package doesn't know about it |
| `defer` for cleanup | `defer rows.Close()`, `defer cancel()` |
| `slog` for structured logging | every log line has key/value pairs |
| Table-driven tests | `service`, `codec`, and `tracker` tests all use `t.Run(tc.name, ...)` |
| `t.Helper()` in test helpers | in test builders |
| Prepared statements | all SQL uses `db.ExecContext` / `db.QueryRowContext` |
| `go vet` + `golangci-lint` clean | linter config in `.golangci.yml` |
| `go fmt` / `goimports` | Makefile target `make fmt` |
| `internal/` packages | none of `internal/*` is importable from outside this module |
| Configuration via env + defaults | `config.Load()` reads `PORT`, `DB_PATH`, `TRACKER_WORKERS`, `BASE_URL` etc. |
| `go.mod` tidy and minimal | only the deps we actually need |
| 12-factor | config in env, logs to stdout, DB path configurable |
| Multi-stage Docker build | build stage with Go toolchain, final stage distroless or alpine, runs as non-root |
| `trimpath` + `-ldflags="-s -w"` | reproducible, small binary |

**Notes for your background (PHP/JS/Python → Go):** the README includes a short "Coming from X" sidebar covering: no exceptions (errors are values), no classes (structs + methods), no `async/await` (goroutines + channels), no virtualenv (just `go.mod`), and `go run` is like `node --watch` but compiled.

---

## 6. Implementation phases

| # | Phase | Status |
|---|---|---|
| 0 | **Setup** | ✅ `go mod init`, deps, `.gitignore`, Makefile, linter config, dir skeleton |
| 1 | **Config + DB** | ✅ `config` package, `repository` package, embedded migrations, in-memory DB tests |
| 2 | **Domain + codec** | ✅ `model.Link`, `codec.Generate()` with collision retry, unit tests |
| 3 | **Service layer** | ✅ `service.Shortener` with create/resolve/delete, mock-repo tests |
| 4 | **HTTP layer** | ✅ Chi router, handlers, middleware, error mapping, `httptest` integration tests |
| 5 | **Click tracker** | ✅ `tracker.Tracker` worker pool, wired into redirect handler, drop-on-full behavior tested |
| 6 | **Graceful shutdown** | ✅ `signal.NotifyContext` flow in `main.go` |
| 7 | **Docker** | 🔜 Multi-stage Dockerfile, `docker-compose.yml` with named volume for SQLite, `.env.example` |
| 8 | **Documentation** | 🔜 `README.md`, `docs/ARCHITECTURE.md`, `docs/CONCURRENCY.md`, `docs/API.md`, `docs/DEVELOPMENT.md` |

Each phase ends with a runnable + testable state. You can stop at any phase and have a working (partial) app.

---

## 7. Docker setup (planned)

**Dockerfile** (multi-stage, distroless final image, non-root):
```dockerfile
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/server /server
EXPOSE 8080
ENTRYPOINT ["/server"]
```

**docker-compose.yml** mounts a named volume for the SQLite file so data survives container restarts.

**Makefile** targets: `build`, `test`, `test-race`, `lint`, `fmt`, `run`, `docker-build`, `docker-up`, `docker-down`, `tidy`.

---

## 8. Documentation deliverables

| File | Content |
|---|---|
| `README.md` | Quickstart (Docker + local), "coming from PHP/JS/Python" sidebar, link to other docs |
| `docs/ARCHITECTURE.md` | Layer diagram, request flow, package responsibilities, why each Go pattern is used |
| `docs/CONCURRENCY.md` | **The deep doc.** Sections: GMP model, goroutines, channels, mutex, WaitGroup, context, the click-tracker worker pool (with line numbers in code), graceful shutdown order, race detector, common pitfalls. Includes one "broken → fixed" example per pattern. |
| `docs/API.md` | Every endpoint, request/response schema, curl examples, error codes |
| `docs/DEVELOPMENT.md` | Local setup, env vars, testing strategy, linting, adding a new endpoint walkthrough |

---

## 9. Dependencies (final list)

```
github.com/go-chi/chi/v5        # router
github.com/google/uuid          # request IDs
modernc.org/sqlite              # pure-Go SQLite driver
```

That's it. No ORM, no validation library, no config library — the project is small enough to be a good showcase of stdlib-first Go.

---

## 10. Open items

1. **API authentication** — open API (loopback / single-tenant). Adding a key-based middleware is documented as a future step.
2. **Analytics depth** — the click counter increments on every redirect. Referrer/user-agent breakdown is *captured* in the click event but not persisted in v1; a future migration can add a `clicks` table.
3. **HTTPS** — terminated at a reverse proxy (Caddy/Nginx). Documented in README.
4. **CORS** — permissive default with env var to lock it down (planned).
5. **Tests vs. time** — unit tests for `codec`, `service`, `tracker` and `httptest` integration tests for handlers, but skip E2E browser tests.

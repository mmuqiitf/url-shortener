# Development

Everything you need to set up, test, lint, and extend the project.

## Requirements

- **Go 1.25.x** (the `go.mod` declares `go 1.25.0`; older Go versions
  are auto-downloaded by the toolchain).
- **make** for the build targets.
- **Docker** (optional) for the container build.
- **CGO / gcc** (optional) for `go test -race`. macOS has `clang`
  bundled; on Linux you may need `apt-get install gcc` (or your distro's
  equivalent). On WSL you may need to install it manually.

## Local setup

```bash
git clone <repo>
cd url-shortener
go mod download
```

That's it. There is no virtualenv, no `npm install`, no global
state. The whole dependency graph is in `go.mod` and `go.sum`.

## Environment variables

All settings have sensible defaults. See `.env.example` for the full
list. The most useful ones:

| Var | Default | What it does |
|---|---|---|
| `PORT` | `8080` | HTTP listen port |
| `LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |
| `DB_PATH` | `./data/shortener.db` | SQLite file path |
| `REDIS_ADDR` | `localhost:6379` | Redis server address (falls back to memory LRU if unreachable) |
| `REDIS_PASSWORD` | `""` | Redis authentication password |
| `REDIS_DB` | `0` | Redis database number |
| `REDIS_TTL_SEC` | `600` | Redis link cache TTL in seconds |
| `BASE_URL` | `http://localhost:<port>` | Public URL prefix used in `short_url` |
| `TRACKER_WORKERS` | `2` | Click-event worker count |
| `TRACKER_BATCH_SIZE` | `50` | Max events per batch flush |
| `TRACKER_FLUSH_INTERVAL_MS` | `1000` | Time-based flush interval (ms) |
| `TRACKER_BUFFER` | `4096` | Channel capacity (= max in-flight events) |
| `READ_TIMEOUT_SEC` | `10` | HTTP read timeout |
| `WRITE_TIMEOUT_SEC` | `10` | HTTP write timeout |
| `IDLE_TIMEOUT_SEC` | `120` | HTTP idle timeout |

## Common commands

```bash
make build          # ./bin/server
make run            # build + run
make test           # go test ./...
make test-race      # go test -race ./...  (needs gcc)
make lint           # golangci-lint run
make fmt            # gofmt + goimports
make tidy           # go mod tidy
make smoke          # end-to-end curl test
```

## Testing strategy

We have four kinds of tests:

| Kind | Where | What it covers |
|---|---|---|
| Pure unit | `codec/base62_test.go` | Code generation uniqueness and validation |
| Repo unit | `repository/sqlite_test.go` | SQL on an in-memory DB |
| Service unit | `service/shortener_test.go` | Business logic with a mock repository |
| Tracker unit | `tracker/tracker_test.go` | Worker pool, drop-on-full, shutdown drain |
| Handler integration | `handler/handler_test.go` | `httptest` against a wired-up handler |

### Table-driven tests

Most tests follow the table-driven pattern:

```go
func TestSomething(t *testing.T) {
    tests := []struct {
        name string
        in   string
        want string
    }{
        {"empty", "", ""},
        {"plain", "https://example.com", "https://example.com"},
        {"trims", "  https://example.com  ", "https://example.com"},
    }
    for _, tc := range tests {
        tc := tc
        t.Run(tc.name, func(t *testing.T) {
            t.Helper()
            got := normalizeURL(tc.in)
            if got != tc.want {
                t.Fatalf("got %q, want %q", got, tc.want)
            }
        })
    }
}
```

Why this shape: it scales linearly with cases, every case has a name
that shows up in test output, and the `tc := tc` line guards against
the loop-variable-capture bug in pre-1.22 Go.

### Stress testing concurrent code

`go test -race` is the gold standard. If you cannot run the race
detector (no CGO/gcc), fall back to:

```bash
go test -count=10 -run TestTracker_Concurrent ./...
```

A real data race is almost always caught within a few iterations of
the same test.

## Linting

`.golangci.yml` configures the linter. To install:

```bash
go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
make lint
```

## Adding a new endpoint

A walkthrough. Suppose we want to add `GET /api/v1/links/{code}/stats`
that returns click history.

1. **Add a method to the service interface (or the struct).**
   In `internal/service/shortener.go`:

   ```go
   func (s *Shortener) Stats(ctx context.Context, code string) (model.Stats, error) {
       // ... fetch and aggregate from repo
   }
   ```

2. **Extend the repository (or add a new method).**
   In `internal/repository/sqlite.go`, add a `Stats(ctx, code)` method
   that runs the SQL. Write a test against the in-memory DB.

3. **Add a handler.** In `internal/handler/handler.go`:

   ```go
   func (h *Handler) getStats(w http.ResponseWriter, r *http.Request) {
       code := chi.URLParam(r, "code")
       stats, err := h.svc.Stats(r.Context(), code)
       if err != nil {
           h.writeError(w, r, err)
           return
       }
       h.writeJSON(w, http.StatusOK, stats)
   }
   ```

   And register the route inside `Routes()`:

   ```go
   r.Get("/api/v1/links/{code}/stats", h.getStats)
   ```

4. **Update the API doc.** Add a section to `docs/API.md`.

5. **Update the smoke test.** Append a `curl` case to
   `scripts/smoke.sh`.

That's the whole flow. The pattern is identical for every endpoint:
service method, repo method, handler, route, test, doc, smoke.

## Adding a new field to a link

1. Add a column to the schema in
   `internal/repository/migrations/0002_add_xxx.sql`.
2. Add the field to `model.Link`.
3. Update the SQL `INSERT` and `SELECT` in `repository/sqlite.go`.
4. Update the JSON tags in the handler's response struct.
5. Update the service layer's input struct if user-supplied.

## Database

SQLite via `modernc.org/sqlite` (pure Go). The database file lives at
`$DB_PATH` (default `./data/shortener.db`). The schema is applied on
startup from the embedded `migrations/*.sql` files.

To inspect the DB:

```bash
sqlite3 data/shortener.db 'SELECT * FROM links;'
```

or, if you don't have `sqlite3` installed:

```bash
python3 -c 'import sqlite3; c=sqlite3.connect("data/shortener.db"); print(list(c.execute("SELECT * FROM links")))'
```

## Docker

```bash
make docker-build
make docker-up       # http://localhost:8080
make docker-down
make docker-logs
```

The image is multi-stage:

- **build** — `golang:1.25-alpine`, full toolchain
- **runtime** — `gcr.io/distroless/static-debian12:nonroot`,
  no shell, runs as uid 65532

The SQLite file is persisted in the named volume `url-shortener-data`.
Inspect it with `docker volume inspect url-shortener-data`.

## Releasing

1. `make test-race && make lint`
2. `git tag v0.1.0`
3. `docker build -t your-org/url-shortener:v0.1.0 .`
4. `docker push your-org/url-shortener:v0.1.0`

(No CI is wired up in this repo. Add GitHub Actions / GitLab CI as you
like.)

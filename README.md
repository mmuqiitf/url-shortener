# URL Shortener

A small, production-shaped URL shortener written in Go. It's deliberately
kept simple enough to read end-to-end in an afternoon, but uses the same
patterns you would see in a real service:

- Chi router + `net/http`
- GORM + pure-Go SQLite driver (`github.com/glebarez/sqlite`, no CGO)
- Click-tracking worker pool
- Graceful shutdown
- Multi-stage Docker build (distroless final image)
- Structured logging with `log/slog`

## Quickstart

### Run with Docker

```bash
docker compose up --build
# -> http://localhost:8080/healthz
```

### Run locally

```bash
go run ./cmd/server
# -> http://localhost:8080/healthz
```

The server creates `./data/shortener.db` on first run. To reset it, just
delete the file.

## API at a glance

```bash
# shorten a URL
curl -X POST http://localhost:8080/api/v1/links \
  -H 'Content-Type: application/json' \
  -d '{"url":"https://example.com/very/long","custom_alias":"ex"}'
# -> 201 { "code":"ex", "short_url":"http://localhost:8080/ex", ... }

# follow a short link
curl -i http://localhost:8080/ex
# -> 301 Location: https://example.com/very/long

# inspect a link
curl http://localhost:8080/api/v1/links/ex

# list all links
curl http://localhost:8080/api/v1/links

# soft-delete a link
curl -X DELETE http://localhost:8080/api/v1/links/ex
```

Full reference: see [`docs/API.md`](docs/API.md).

## Project layout

```
cmd/server         HTTP entrypoint
internal/config    env-var loading
internal/model     domain types (Link, APIError)
internal/codec     base62 short-code generation
internal/repository  GORM + SQLite (AutoMigrate)
internal/service   business logic
internal/tracker   click-event worker pool
internal/handler   HTTP handlers (chi)
internal/middleware  request-id, logging, recover, CORS
docs/              ARCHITECTURE, CONCURRENCY, API, DEVELOPMENT, PLAN
scripts/           smoke.sh
```

See [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) for the request flow.

## Documentation

| File | What's in it |
|---|---|
| [`docs/PLAN.md`](docs/PLAN.md) | The design plan and why each decision was made |
| [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) | Layer diagram, request flow, package responsibilities |
| [`docs/CONCURRENCY.md`](docs/CONCURRENCY.md) | The deep doc — GMP, goroutines, channels, the click-tracker pool, race detector |
| [`docs/API.md`](docs/API.md) | Every endpoint, request/response schema, curl examples |
| [`docs/DEVELOPMENT.md`](docs/DEVELOPMENT.md) | Local setup, env vars, testing, linting, adding endpoints |

## Commands

```bash
make build         # binary at ./bin/server
make run           # build + run
make test          # go test ./...
make test-race     # go test -race ./...  (requires CGO/gcc)
make lint          # golangci-lint
make fmt           # gofmt + goimports
make smoke         # curl-based end-to-end smoke test
make docker-build  # build the image
make docker-up     # docker compose up -d
make docker-down   # docker compose down
```

## Coming from another language

If you know PHP, JavaScript/TypeScript, or Python, here is a quick map to
the Go bits:

| Concept | Go equivalent | Where in this project |
|---|---|---|
| Try/catch | `if err != nil { return err }` | every function that can fail |
| Class | `struct` + methods on pointer receiver | `service.Shortener`, `tracker.Tracker` |
| Inheritance / DI | Interfaces defined where they're consumed | `service.Repository`, `tracker.Store` |
| `async`/`await` | `go func()` + channels | `tracker.Run`, `serverErr` channel in `main.go` |
| `pip install -r` | `go mod tidy` | `go.mod`, `go.sum` |
| `npm run dev` | `go run ./cmd/server` | `cmd/server/main.go` |
| `var_dump(x)` | `slog.Info("x", "value", x)` | every log line |
| `set -e` | errors propagate via `return err` | the `run()` function in `main.go` |

## License

MIT

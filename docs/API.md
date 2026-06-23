# API Reference

Base URL (default local): `http://localhost:8080`

All responses are JSON. Errors use the envelope:

```json
{
  "error": { "code": "NOT_FOUND", "message": "link not found" }
}
```

## Endpoints

### POST /api/v1/links — create a short link

```bash
curl -X POST http://localhost:8080/api/v1/links \
  -H 'Content-Type: application/json' \
  -d '{
    "url": "https://example.com/very/long/path",
    "custom_alias": "ex",
    "expires_at": "2026-12-31T23:59:59Z"
  }'
```

**Request body**

| Field | Type | Required | Notes |
|---|---|---|---|
| `url` | string | yes | http or https URL. Other schemes (javascript, file, data) are rejected. |
| `custom_alias` | string | no | 3–32 chars, `[A-Za-z0-9_-]+`. Returns 409 if taken. |
| `expires_at` | string | no | RFC3339 timestamp. Expired links return 410. |

**Responses**

| Status | Body | When |
|---|---|---|
| `201` | `Link` JSON | Created |
| `400` | `error: { code: "INVALID_URL", ... }` | Bad URL or alias |
| `409` | `error: { code: "CODE_TAKEN", ... }` | Custom alias already exists |

**Link JSON shape**

```json
{
  "code": "ex",
  "short_url": "http://localhost:8080/ex",
  "long_url": "https://example.com/very/long/path",
  "created_at": "2026-06-23T13:53:40Z",
  "clicks": 0,
  "is_active": true
}
```

### GET /api/v1/links/{code} — fetch link metadata

```bash
curl http://localhost:8080/api/v1/links/ex
```

Returns the same `Link` JSON. `404 NOT_FOUND` if the code does not
exist.

### GET /api/v1/links — list links

```bash
curl 'http://localhost:8080/api/v1/links?limit=50&offset=0'
```

| Query | Type | Default | Notes |
|---|---|---|---|
| `limit` | int | 50 | Max 500 |
| `offset` | int | 0 | |

Response:

```json
{
  "items": [ /* Link objects, newest first */ ],
  "total": 42
}
```

### DELETE /api/v1/links/{code} — soft-delete

```bash
curl -X DELETE http://localhost:8080/api/v1/links/ex
```

Sets `is_active = 0`. The row is kept for click history. `204 No
Content` on success. `404 NOT_FOUND` if the code does not exist.

### GET /{code} — public redirect

```bash
curl -i http://localhost:8080/ex
```

| Status | When |
|---|---|
| `301 Moved Permanently` | Active, not expired. `Location: <long_url>` |
| `404 Not Found` | Code does not exist |
| `410 Gone` | Code is inactive or expired |

Redirect responses include a tiny HTML body for clients that don't
follow the `Location` header (e.g. `curl` without `-L`).

### GET /healthz — liveness

Always `200 OK`. Body: `{"status":"ok"}`.

### GET /readyz — readiness

`200 OK` if the DB ping succeeds, `503 Service Unavailable` otherwise.

## Error codes

| Code | HTTP | Meaning |
|---|---|---|
| `INVALID_URL` | 400 | The long URL is missing, malformed, or uses a non-http(s) scheme. |
| `INVALID_ALIAS` | 400 | Custom alias does not match the allowed pattern. |
| `INVALID_EXPIRY` | 400 | `expires_at` is not a valid RFC3339 timestamp. |
| `NOT_FOUND` | 404 | No link with that code. |
| `CODE_TAKEN` | 409 | A link with that custom alias already exists. |
| `EXPIRED` | 410 | The link is past its `expires_at`. |
| `INACTIVE` | 410 | The link has been soft-deleted. |
| `INTERNAL` | 500 | Anything we did not anticipate. |

## CORS

The default `Access-Control-Allow-Origin: *` is permissive. Lock it
down by setting the `ALLOWED_ORIGINS` env var (TODO: env var will be
added in a follow-up; the middleware currently allows `*`).

## Authentication

The API is unauthenticated in v1. It is intended to run inside a
trusted network or behind a reverse proxy that handles auth.

## Rate limiting

Not enforced in v1. The architecture doc and the concurrency doc
describe how to add an in-memory token-bucket limiter per IP.

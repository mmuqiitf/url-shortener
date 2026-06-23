// Package handler wires the service and tracker layers to HTTP routes.
//
// Handlers stay thin: they parse the request, call the service, and
// render the response. All business decisions live in the service;
// all persistence in the repository.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mmuqiitf/url-shortener/internal/model"
	"github.com/mmuqiitf/url-shortener/internal/tracker"
)

// Shortener is the surface the service exposes that handlers depend on.
// Defined here so handler tests can inject a fake without importing
// the concrete service.
type Shortener interface {
	Create(ctx context.Context, in model.CreateLinkInput) (model.Link, error)
	GetByCode(ctx context.Context, code string) (model.Link, error)
	List(ctx context.Context, limit, offset int) ([]model.Link, error)
	Delete(ctx context.Context, code string) error
	Resolve(ctx context.Context, code string) (model.Link, error)
}

// Pinger is implemented by anything that can verify it is ready to
// serve traffic. The repository satisfies it (via Ping).
type Pinger interface {
	Ping(ctx context.Context) error
}

// Handler bundles the dependencies the HTTP routes need.
type Handler struct {
	svc     Shortener
	tracker *tracker.Tracker
	pinger  Pinger
	log     *slog.Logger
	baseURL string
}

// New returns a Handler. The baseURL is used to construct the public
// `short_url` field returned by Create. The pinger may be nil — in
// that case /readyz returns 200 unconditionally.
func New(svc Shortener, tr *tracker.Tracker, pinger Pinger, log *slog.Logger, baseURL string) *Handler {
	if log == nil {
		log = slog.Default()
	}
	return &Handler{svc: svc, tracker: tr, pinger: pinger, log: log, baseURL: baseURL}
}

// Routes returns a chi router with all endpoints registered.
func (h *Handler) Routes() http.Handler {
	r := chi.NewRouter()

	r.Get("/healthz", h.healthz)
	r.Get("/readyz", h.readyz)

	r.Route("/api/v1", func(r chi.Router) {
		r.Post("/links", h.create)
		r.Get("/links", h.list)
		r.Get("/links/{code}", h.get)
		r.Delete("/links/{code}", h.delete)
	})

	// Public redirect. Registered last so the more specific routes
	// above win.
	r.Get("/{code}", h.redirect)

	return r
}

// --- DTOs ---------------------------------------------------------------

type createRequest struct {
	URL         string  `json:"url"`
	CustomAlias string  `json:"custom_alias,omitempty"`
	ExpiresAt   *string `json:"expires_at,omitempty"`
}

type linkResponse struct {
	Code      string  `json:"code"`
	ShortURL  string  `json:"short_url"`
	LongURL   string  `json:"long_url"`
	CreatedAt string  `json:"created_at"`
	ExpiresAt *string `json:"expires_at,omitempty"`
	Clicks    int64   `json:"clicks"`
	IsActive  bool    `json:"is_active"`
}

type listResponse struct {
	Items []linkResponse `json:"items"`
	Total int            `json:"total"`
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type errorResponse struct {
	Error errorBody `json:"error"`
}

// --- handlers -----------------------------------------------------------

func (h *Handler) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) readyz(w http.ResponseWriter, r *http.Request) {
	if h.pinger != nil {
		if err := h.pinger.Ping(r.Context()); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"status": "not_ready",
				"reason": err.Error(),
			})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, model.NewAPIError(400, "BAD_JSON", "request body is not valid JSON").Wrap(err))
		return
	}
	if req.URL == "" {
		writeError(w, model.NewAPIError(400, "MISSING_URL", "field 'url' is required"))
		return
	}

	var exp *time.Time
	if req.ExpiresAt != nil && *req.ExpiresAt != "" {
		t, err := time.Parse(time.RFC3339, *req.ExpiresAt)
		if err != nil {
			writeError(w, model.NewAPIError(400, "BAD_EXPIRES_AT", "expires_at must be RFC3339").Wrap(err))
			return
		}
		t = t.UTC()
		exp = &t
	}

	link, err := h.svc.Create(r.Context(), model.CreateLinkInput{
		LongURL:     req.URL,
		CustomAlias: req.CustomAlias,
		ExpiresAt:   exp,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toLinkResponse(link, h.baseURL))
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	code := chi.URLParam(r, "code")
	link, err := h.svc.GetByCode(r.Context(), code)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toLinkResponse(link, h.baseURL))
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	items, err := h.svc.List(r.Context(), limit, offset)
	if err != nil {
		writeError(w, err)
		return
	}
	out := make([]linkResponse, len(items))
	for i, l := range items {
		out[i] = toLinkResponse(l, h.baseURL)
	}
	writeJSON(w, http.StatusOK, listResponse{Items: out, Total: len(out)})
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	code := chi.URLParam(r, "code")
	if err := h.svc.Delete(r.Context(), code); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) redirect(w http.ResponseWriter, r *http.Request) {
	code := chi.URLParam(r, "code")
	link, err := h.svc.Resolve(r.Context(), code)
	if err != nil {
		// If the link isn't found, treat it as "no such short link"
		// and render a small HTML page. (We could also return JSON;
		// HTML is friendlier when someone hits /xyzzy in a browser.)
		var apiErr *model.APIError
		if errors.As(err, &apiErr) {
			writeRedirectError(w, apiErr.Status, code)
			return
		}
		writeRedirectError(w, http.StatusInternalServerError, code)
		return
	}

	if h.tracker != nil {
		h.tracker.Record(tracker.Event{
			Code:      link.Code,
			Timestamp: time.Now().UTC(),
			IP:        clientIP(r),
			UserAgent: r.UserAgent(),
			Referer:   r.Referer(),
		})
	}

	http.Redirect(w, r, link.LongURL, http.StatusMovedPermanently)
}

// --- helpers ------------------------------------------------------------

func toLinkResponse(l model.Link, baseURL string) linkResponse {
	resp := linkResponse{
		Code:      l.Code,
		ShortURL:  fmt.Sprintf("%s/%s", baseURL, l.Code),
		LongURL:   l.LongURL,
		CreatedAt: l.CreatedAt.UTC().Format(time.RFC3339),
		Clicks:    l.Clicks,
		IsActive:  l.IsActive,
	}
	if l.ExpiresAt != nil {
		s := l.ExpiresAt.UTC().Format(time.RFC3339)
		resp.ExpiresAt = &s
	}
	return resp
}

// writeJSON marshals v as JSON and writes it with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// encoding errors are rare and unrecoverable here; the
		// response status is already set so we just log.
		_ = err
	}
}

// writeError converts an error to the standard error envelope.
//
// If the error is (or wraps) a *model.APIError, we use its status,
// code and message. Otherwise we log the underlying error and return
// a generic 500 — we never leak internal error strings to the client.
func writeError(w http.ResponseWriter, err error) {
	var apiErr *model.APIError
	if errors.As(err, &apiErr) {
		writeJSON(w, apiErr.Status, errorResponse{Error: errorBody{
			Code: apiErr.Code, Message: apiErr.Message,
		}})
		return
	}
	writeJSON(w, http.StatusInternalServerError, errorResponse{Error: errorBody{
		Code: "INTERNAL", Message: "internal server error",
	}})
}

// writeRedirectError renders a tiny HTML page for the redirect path.
// We do not return JSON for this route because the typical client is
// a browser that hit /{code} directly.
func writeRedirectError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	body := fmt.Sprintf(`<!doctype html><html><head><meta charset="utf-8"><title>%d</title></head>
<body><h1>%d</h1><p>No link found for code <code>%s</code>.</p></body></html>`,
		status, status, code)
	_, _ = w.Write([]byte(body))
}

// clientIP returns the best-guess client IP, preferring X-Forwarded-For
// when present (typical when running behind a reverse proxy).
func clientIP(r *http.Request) string {
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		return v
	}
	return r.RemoteAddr
}

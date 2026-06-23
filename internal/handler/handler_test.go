package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mmuqiitf/url-shortener/internal/model"
	"github.com/mmuqiitf/url-shortener/internal/tracker"
)

// fakeSvc is a controllable in-memory implementation of handler.Shortener.
type fakeSvc struct {
	mu    sync.Mutex
	items map[string]model.Link
}

func newFakeSvc() *fakeSvc { return &fakeSvc{items: map[string]model.Link{}} }

func (f *fakeSvc) Create(_ context.Context, in model.CreateLinkInput) (model.Link, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if in.CustomAlias != "" {
		if _, ok := f.items[in.CustomAlias]; ok {
			return model.Link{}, model.ErrCodeExists
		}
		f.items[in.CustomAlias] = model.Link{
			Code: in.CustomAlias, LongURL: in.LongURL,
			CreatedAt: time.Now().UTC(), ExpiresAt: in.ExpiresAt, IsActive: true,
		}
		return f.items[in.CustomAlias], nil
	}
	// auto-generate a fake code
	code := "gen1"
	if _, ok := f.items[code]; ok {
		return model.Link{}, model.ErrCodeExists
	}
	f.items[code] = model.Link{
		Code: code, LongURL: in.LongURL,
		CreatedAt: time.Now().UTC(), ExpiresAt: in.ExpiresAt, IsActive: true,
	}
	return f.items[code], nil
}

func (f *fakeSvc) GetByCode(_ context.Context, code string) (model.Link, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	l, ok := f.items[code]
	if !ok {
		return model.Link{}, model.ErrNotFound
	}
	if !l.IsActive {
		return l, model.ErrLinkInactive
	}
	if l.ExpiresAt != nil && !l.ExpiresAt.After(time.Now()) {
		return l, model.ErrLinkExpired
	}
	return l, nil
}

func (f *fakeSvc) Resolve(_ context.Context, code string) (model.Link, error) {
	return f.GetByCode(context.Background(), code)
}

func (f *fakeSvc) List(_ context.Context, limit, offset int) ([]model.Link, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]model.Link, 0, len(f.items))
	for _, l := range f.items {
		out = append(out, l)
	}
	return out, nil
}

func (f *fakeSvc) Delete(_ context.Context, code string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.items[code]; !ok {
		return model.ErrNotFound
	}
	delete(f.items, code)
	return nil
}

// noopStore is a tracker.Store that swallows calls.
type noopStore struct{}

func (noopStore) BatchIncrementClicks(context.Context, []string) error { return nil }

func newTestHandler(t *testing.T) (*Handler, *fakeSvc) {
	t.Helper()
	svc := newFakeSvc()
	tr := tracker.New(noopStore{}, slog.New(slog.NewTextHandler(io.Discard, nil)), tracker.Config{
		Workers: 1, BufferSize: 16, BatchSize: 1, FlushInterval: time.Hour,
	})
	t.Cleanup(func() { _ = tr.Shutdown(context.Background()) })
	return New(svc, tr, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), "http://test.local"), svc
}

func TestCreate_Success(t *testing.T) {
	t.Parallel()
	h, _ := newTestHandler(t)

	body, _ := json.Marshal(map[string]string{"url": "https://example.com", "custom_alias": "hello"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/links", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status: got %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var got linkResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Code != "hello" {
		t.Errorf("code: %q", got.Code)
	}
	if got.ShortURL != "http://test.local/hello" {
		t.Errorf("short_url: %q", got.ShortURL)
	}
}

func TestCreate_MissingURL(t *testing.T) {
	t.Parallel()
	h, _ := newTestHandler(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/links",
		strings.NewReader(`{"custom_alias":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", rec.Code)
	}
}

func TestCreate_DuplicateAlias(t *testing.T) {
	t.Parallel()
	h, _ := newTestHandler(t)
	body := []byte(`{"url":"https://a.example","custom_alias":"same"}`)
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/links", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.Routes().ServeHTTP(rec, req)
		if i == 0 && rec.Code != http.StatusCreated {
			t.Fatalf("first create: got %d", rec.Code)
		}
		if i == 1 && rec.Code != http.StatusConflict {
			t.Errorf("second create: got %d, want 409", rec.Code)
		}
	}
}

func TestGet_NotFound(t *testing.T) {
	t.Parallel()
	h, _ := newTestHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/links/missing", nil)
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", rec.Code)
	}
}

func TestRedirect_Success(t *testing.T) {
	t.Parallel()
	h, svc := newTestHandler(t)
	_, _ = svc.Create(context.Background(), model.CreateLinkInput{
		LongURL: "https://target.example", CustomAlias: "abc",
	})
	req := httptest.NewRequest(http.MethodGet, "/abc", nil)
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusMovedPermanently {
		t.Fatalf("status: got %d, want 301; body=%s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if loc != "https://target.example" {
		t.Errorf("Location: %q", loc)
	}
}

func TestRedirect_NotFound(t *testing.T) {
	t.Parallel()
	h, _ := newTestHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/nope", nil)
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type: %q", ct)
	}
}

func TestHealthz(t *testing.T) {
	t.Parallel()
	h, _ := newTestHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200", rec.Code)
	}
}

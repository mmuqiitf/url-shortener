package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mmuqiitf/url-shortener/internal/model"
)

// fakeRepo is an in-memory Repository for service tests. It uses a
// sync.RWMutex so tests can call it from multiple goroutines, which
// matters when we exercise the redirect path under concurrency.
type fakeRepo struct {
	mu   sync.RWMutex
	data map[string]model.Link
}

func newFakeRepo() *fakeRepo { return &fakeRepo{data: map[string]model.Link{}} }

func (f *fakeRepo) Create(_ context.Context, l model.Link) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.data[l.Code]; ok {
		return model.ErrCodeExists
	}
	f.data[l.Code] = l
	return nil
}

func (f *fakeRepo) GetByCode(_ context.Context, code string) (model.Link, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	l, ok := f.data[code]
	if !ok {
		return model.Link{}, model.ErrNotFound
	}
	return l, nil
}

func (f *fakeRepo) List(_ context.Context, limit, offset int) ([]model.Link, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make([]model.Link, 0, len(f.data))
	for _, l := range f.data {
		out = append(out, l)
	}
	if offset > len(out) {
		offset = len(out)
	}
	out = out[offset:]
	if limit > 0 && limit < len(out) {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeRepo) DeactivateByCode(_ context.Context, code string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	l, ok := f.data[code]
	if !ok || !l.IsActive {
		return model.ErrNotFound
	}
	l.IsActive = false
	f.data[code] = l
	return nil
}

func TestCreate_Success(t *testing.T) {
	t.Parallel()
	svc := New(newFakeRepo(), nil)
	got, err := svc.Create(context.Background(), model.CreateLinkInput{
		LongURL: "https://example.com/long/path",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got.Code == "" {
		t.Errorf("expected non-empty code")
	}
	if !strings.HasPrefix(got.LongURL, "https://") {
		t.Errorf("LongURL: %q", got.LongURL)
	}
	if !got.IsActive {
		t.Errorf("expected IsActive=true")
	}
}

func TestCreate_CustomAlias(t *testing.T) {
	t.Parallel()
	svc := New(newFakeRepo(), nil)
	got, err := svc.Create(context.Background(), model.CreateLinkInput{
		LongURL:     "https://example.com",
		CustomAlias: "promo",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got.Code != "promo" {
		t.Errorf("Code: got %q, want promo", got.Code)
	}
}

func TestCreate_InvalidURL(t *testing.T) {
	t.Parallel()
	svc := New(newFakeRepo(), nil)
	cases := []string{
		"",
		"   ",
		"not-a-url",
		"javascript:alert(1)",
		"file:///etc/passwd",
		"https://", // missing host
	}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			t.Parallel()
			_, err := svc.Create(context.Background(), model.CreateLinkInput{LongURL: c})
			if !errors.Is(err, model.ErrInvalidURL) {
				t.Errorf("LongURL=%q: got %v, want ErrInvalidURL", c, err)
			}
		})
	}
}

func TestCreate_InvalidAlias(t *testing.T) {
	t.Parallel()
	svc := New(newFakeRepo(), nil)
	_, err := svc.Create(context.Background(), model.CreateLinkInput{
		LongURL: "https://example.com", CustomAlias: "bad alias!",
	})
	if !errors.Is(err, model.ErrInvalidCode) {
		t.Errorf("got %v, want ErrInvalidCode", err)
	}
}

func TestCreate_AliasCollision(t *testing.T) {
	t.Parallel()
	svc := New(newFakeRepo(), nil)
	in := model.CreateLinkInput{LongURL: "https://example.com", CustomAlias: "x"}
	if _, err := svc.Create(context.Background(), in); err != nil {
		t.Fatalf("first create: %v", err)
	}
	_, err := svc.Create(context.Background(), in)
	if !errors.Is(err, model.ErrCodeExists) {
		t.Errorf("got %v, want ErrCodeExists", err)
	}
}

func TestResolve_Expired(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	repo := newFakeRepo()
	expired := now.Add(-time.Hour)
	_ = repo.Create(context.Background(), model.Link{
		Code: "exp", LongURL: "https://e", ExpiresAt: &expired, IsActive: true,
	})
	svc := New(repo, clock)
	_, err := svc.Resolve(context.Background(), "exp")
	if !errors.Is(err, model.ErrLinkExpired) {
		t.Errorf("got %v, want ErrLinkExpired", err)
	}
}

func TestResolve_Inactive(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	_ = repo.Create(context.Background(), model.Link{
		Code: "off", LongURL: "https://e", IsActive: false,
	})
	svc := New(repo, nil)
	_, err := svc.Resolve(context.Background(), "off")
	if !errors.Is(err, model.ErrLinkInactive) {
		t.Errorf("got %v, want ErrLinkInactive", err)
	}
}

func TestResolve_Success(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	_ = repo.Create(context.Background(), model.Link{
		Code: "ok", LongURL: "https://e", IsActive: true,
	})
	svc := New(repo, nil)
	got, err := svc.Resolve(context.Background(), "ok")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.LongURL != "https://e" {
		t.Errorf("LongURL: %q", got.LongURL)
	}
}

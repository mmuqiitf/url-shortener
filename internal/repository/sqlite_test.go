package repository

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/mmuqiitf/url-shortener/internal/model"
)

// newTestRepo returns a Repository backed by a temporary on-disk SQLite
// file. Using a file (not :memory:) lets us run concurrent readers and
// writers across multiple connections in tests.
func newTestRepo(t *testing.T) *Repository {
	t.Helper()
	dsn := "file:" + filepath.Join(t.TempDir(), "test.db") + "?_pragma=busy_timeout(5000)"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	repo, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	return repo
}

func TestCreateAndGet(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	ctx := context.Background()

	exp := time.Now().Add(1 * time.Hour).UTC()
	in := model.Link{
		Code:      "abc123",
		LongURL:   "https://example.com",
		ExpiresAt: &exp,
		IsActive:  true,
	}
	if err := repo.Create(ctx, in); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := repo.GetByCode(ctx, "abc123")
	if err != nil {
		t.Fatalf("GetByCode: %v", err)
	}
	if got.LongURL != in.LongURL {
		t.Errorf("LongURL: got %q, want %q", got.LongURL, in.LongURL)
	}
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(exp) {
		t.Errorf("ExpiresAt: got %v, want %v", got.ExpiresAt, exp)
	}
	if !got.IsActive {
		t.Errorf("IsActive: got false, want true")
	}
}

func TestCreate_DuplicateCode(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	ctx := context.Background()
	first := model.Link{Code: "dup", LongURL: "https://a.example"}
	second := model.Link{Code: "dup", LongURL: "https://b.example"}
	if err := repo.Create(ctx, first); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	err := repo.Create(ctx, second)
	if err == nil {
		t.Fatalf("expected error on duplicate code")
	}
}

func TestGetByCode_NotFound(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	_, err := repo.GetByCode(context.Background(), "missing")
	if err == nil {
		t.Fatalf("expected error")
	}
}

func TestList_Pagination(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		link := model.Link{
			Code:    "code" + string(rune('a'+i)),
			LongURL: "https://example.com/" + string(rune('a'+i)),
		}
		if err := repo.Create(ctx, link); err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
	}
	all, err := repo.List(ctx, 10, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 5 {
		t.Errorf("got %d links, want 5", len(all))
	}
	page, err := repo.List(ctx, 2, 1)
	if err != nil {
		t.Fatalf("List page: %v", err)
	}
	if len(page) != 2 {
		t.Errorf("page size: got %d, want 2", len(page))
	}
}

func TestDeactivateByCode(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	ctx := context.Background()
	if err := repo.Create(ctx, model.Link{Code: "x", LongURL: "https://e"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := repo.DeactivateByCode(ctx, "x"); err != nil {
		t.Fatalf("Deactivate: %v", err)
	}
	got, err := repo.GetByCode(ctx, "x")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.IsActive {
		t.Errorf("expected IsActive=false after deactivate")
	}
	// deactivating again -> NotFound
	if err := repo.DeactivateByCode(ctx, "x"); err == nil {
		t.Errorf("expected error on second deactivate")
	}
}

func TestBatchIncrementClicks(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	ctx := context.Background()
	for _, code := range []string{"a", "b", "c"} {
		if err := repo.Create(ctx, model.Link{Code: code, LongURL: "https://" + code}); err != nil {
			t.Fatalf("Create %s: %v", code, err)
		}
	}
	if err := repo.BatchIncrementClicks(ctx, []string{"a", "b", "c", "missing"}); err != nil {
		t.Fatalf("BatchIncrement: %v", err)
	}
	for _, code := range []string{"a", "b", "c"} {
		got, err := repo.GetByCode(ctx, code)
		if err != nil {
			t.Fatalf("Get %s: %v", code, err)
		}
		if got.Clicks != 1 {
			t.Errorf("%s clicks: got %d, want 1", code, got.Clicks)
		}
	}
}

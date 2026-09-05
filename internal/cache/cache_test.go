package cache

import (
	"context"
	"testing"
	"time"

	"github.com/mmuqiitf/url-shortener/internal/model"
)

func TestMemoryCache_SetAndGet(t *testing.T) {
	c := NewMemoryCache(10)
	ctx := context.Background()

	link := model.Link{Code: "test1", LongURL: "https://example.com"}
	c.Set(ctx, link, 1*time.Minute)

	got, ok := c.Get(ctx, "test1")
	if !ok {
		t.Fatalf("expected cache hit")
	}
	if got.LongURL != link.LongURL {
		t.Errorf("got %q, want %q", got.LongURL, link.LongURL)
	}
}

func TestMemoryCache_Expiration(t *testing.T) {
	c := NewMemoryCache(10)
	ctx := context.Background()

	link := model.Link{Code: "exp1", LongURL: "https://example.com"}
	c.Set(ctx, link, 10*time.Millisecond)

	time.Sleep(25 * time.Millisecond)

	_, ok := c.Get(ctx, "exp1")
	if ok {
		t.Fatalf("expected expired cache miss")
	}
}

func TestMemoryCache_Eviction(t *testing.T) {
	c := NewMemoryCache(2)
	ctx := context.Background()

	c.Set(ctx, model.Link{Code: "a", LongURL: "https://a"}, 1*time.Minute)
	c.Set(ctx, model.Link{Code: "b", LongURL: "https://b"}, 1*time.Minute)
	c.Set(ctx, model.Link{Code: "c", LongURL: "https://c"}, 1*time.Minute)

	// 'a' should be evicted
	if _, ok := c.Get(ctx, "a"); ok {
		t.Errorf("expected 'a' to be evicted")
	}
	if _, ok := c.Get(ctx, "b"); !ok {
		t.Errorf("expected 'b' to be present")
	}
	if _, ok := c.Get(ctx, "c"); !ok {
		t.Errorf("expected 'c' to be present")
	}
}

func TestMemoryCache_Delete(t *testing.T) {
	c := NewMemoryCache(10)
	ctx := context.Background()

	link := model.Link{Code: "del1", LongURL: "https://example.com"}
	c.Set(ctx, link, 1*time.Minute)
	c.Delete(ctx, "del1")

	if _, ok := c.Get(ctx, "del1"); ok {
		t.Errorf("expected cache miss after delete")
	}
}

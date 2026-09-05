// Package service contains the URL-shortener business logic.
//
// It defines its own Repository interface (consumer-defined) so the
// service does not import the concrete repository package. This is the
// idiomatic Go pattern: "accept interfaces, return structs". The
// benefit is twofold:
//
//  1. Tests can supply an in-memory fake without spinning up SQLite.
//  2. The repository implementation can be swapped (e.g., Postgres)
//     without touching this file.
package service

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/mmuqiitf/url-shortener/internal/codec"
	"github.com/mmuqiitf/url-shortener/internal/model"
)

// Repository is the subset of persistence the service needs.
// Defined here, not in internal/repository, to keep dependencies
// pointing inward.
type Repository interface {
	Create(ctx context.Context, l model.Link) error
	GetByCode(ctx context.Context, code string) (model.Link, error)
	List(ctx context.Context, limit, offset int) ([]model.Link, error)
	DeactivateByCode(ctx context.Context, code string) error
}

// Cache is the subset of caching functionality the service needs.
type Cache interface {
	Get(ctx context.Context, code string) (model.Link, bool)
	Set(ctx context.Context, link model.Link, ttl time.Duration)
	Delete(ctx context.Context, code string)
}

// Clock is an indirection over time.Now. Used by tests to inject a
// deterministic clock; the production wiring uses time.Now.
type Clock func() time.Time

// Shortener is the business-logic facade for the URL shortener.
type Shortener struct {
	repo   Repository
	cache  Cache
	clock  Clock
	codeLn int
	maxGen int // max generation attempts before giving up
}

// New constructs a Shortener. The clock defaults to time.Now if nil.
// Cache can be nil if caching is disabled.
func New(repo Repository, cache Cache, clock Clock) *Shortener {
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &Shortener{
		repo:   repo,
		cache:  cache,
		clock:  clock,
		codeLn: codec.DefaultLength,
		maxGen: 8,
	}
}

// Create validates the input, generates or normalizes the code, and
// persists the link.
//
// Behavior:
//   - If CustomAlias is empty, a random 7-char code is generated.
//   - If a generated code collides (ErrCodeExists), the call retries up
//     to maxGen times before bubbling the error up.
//   - If a custom alias is provided, it must pass codec.Validate and is
//     stored verbatim; collision is reported as ErrCodeExists.
func (s *Shortener) Create(ctx context.Context, in model.CreateLinkInput) (model.Link, error) {
	longURL, err := normalizeURL(in.LongURL)
	if err != nil {
		return model.Link{}, model.ErrInvalidURL.Wrap(err)
	}

	code := in.CustomAlias
	if code != "" {
		if err := codec.Validate(code); err != nil {
			return model.Link{}, model.ErrInvalidCode.Wrap(err)
		}
	} else {
		code, err = s.generateUnique(ctx)
		if err != nil {
			return model.Link{}, err
		}
	}

	link := model.Link{
		Code:      code,
		LongURL:   longURL,
		CreatedAt: s.clock(),
		ExpiresAt: in.ExpiresAt,
		Clicks:    0,
		IsActive:  true,
	}
	if err := s.repo.Create(ctx, link); err != nil {
		return model.Link{}, err
	}
	return link, nil
}

// GetByCode fetches a link by code, checking the cache first.
func (s *Shortener) GetByCode(ctx context.Context, code string) (model.Link, error) {
	if err := codec.Validate(code); err != nil {
		return model.Link{}, model.ErrInvalidCode.Wrap(err)
	}
	if s.cache != nil {
		if l, hit := s.cache.Get(ctx, code); hit {
			if apiErr := s.checkAvailability(l); apiErr != nil {
				return model.Link{}, apiErr
			}
			return l, nil
		}
	}
	l, err := s.repo.GetByCode(ctx, code)
	if err != nil {
		return model.Link{}, err
	}
	if apiErr := s.checkAvailability(l); apiErr != nil {
		return model.Link{}, apiErr
	}
	if s.cache != nil {
		s.cache.Set(ctx, l, 10*time.Minute)
	}
	return l, nil
}

// Resolve fetches a link for the redirect path. It applies the same
// availability rules as GetByCode and is the canonical entry point
// for `GET /{code}`.
func (s *Shortener) Resolve(ctx context.Context, code string) (model.Link, error) {
	return s.GetByCode(ctx, code)
}

// List returns up to `limit` links starting at `offset`, with both
// values clamped to safe bounds.
func (s *Shortener) List(ctx context.Context, limit, offset int) ([]model.Link, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	return s.repo.List(ctx, limit, offset)
}

// Delete soft-deletes a link and clears it from the cache.
func (s *Shortener) Delete(ctx context.Context, code string) error {
	if err := codec.Validate(code); err != nil {
		return model.ErrInvalidCode.Wrap(err)
	}
	if s.cache != nil {
		s.cache.Delete(ctx, code)
	}
	return s.repo.DeactivateByCode(ctx, code)
}

// --- helpers ------------------------------------------------------------

// generateUnique produces a fresh code, retrying on collision. With a
// 62-character alphabet and length 7 the collision probability is
// < 10^-9 even at 1M links, so 8 attempts is comfortably enough.
func (s *Shortener) generateUnique(ctx context.Context) (string, error) {
	for i := 0; i < s.maxGen; i++ {
		code, err := codec.Generate(s.codeLn)
		if err != nil {
			return "", fmt.Errorf("service: generate code: %w", err)
		}
		_, err = s.repo.GetByCode(ctx, code)
		if errors.Is(err, model.ErrNotFound) {
			return code, nil
		}
		if err != nil {
			// a real DB error (not just "not found"); bubble it up
			return "", err
		}
		// collision — loop and try again
	}
	return "", fmt.Errorf("service: exhausted %d code-generation attempts", s.maxGen)
}

// checkAvailability returns nil if the link is usable, or an APIError
// describing why it isn't. The returned error is a clone so callers
// can attach their own context with %w.
func (s *Shortener) checkAvailability(l model.Link) *model.APIError {
	if !l.IsActive {
		return model.ErrLinkInactive
	}
	if l.ExpiresAt != nil && !l.ExpiresAt.After(s.clock()) {
		return model.ErrLinkExpired
	}
	return nil
}

// normalizeURL validates and lightly normalizes the input URL.
//
// Rules:
//   - Scheme must be http or https (no javascript:, data:, file:).
//   - Host must be present.
//   - Trailing whitespace is trimmed.
func normalizeURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("empty URL")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("unsupported scheme %q (use http or https)", u.Scheme)
	}
	if u.Host == "" {
		return "", errors.New("missing host")
	}
	return raw, nil
}

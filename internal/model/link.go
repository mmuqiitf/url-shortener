// Package model contains the domain types and sentinel errors used by
// every other layer. Keep this package dependency-free so any layer can
// import it without creating cycles.
package model

import "time"

// Link is the core domain entity: a mapping from a short code to a long URL.
type Link struct {
	Code      string     // unique short code (e.g., "aB3xZ9")
	LongURL   string     // target URL
	CreatedAt time.Time
	ExpiresAt *time.Time // nil = never expires
	Clicks    int64
	IsActive  bool
}

// CreateLinkInput is the service-layer input for creating a new link.
type CreateLinkInput struct {
	LongURL     string
	CustomAlias string     // empty -> server generates a code
	ExpiresAt   *time.Time // nil -> no expiry
}

// APIError is a typed error that maps cleanly to an HTTP response.
//
// Handlers use errors.As to extract the code/message; the rest of the
// stack can keep wrapping with %w and still surface a useful API message.
type APIError struct {
	Status  int    // HTTP status code
	Code    string // stable machine-readable code (e.g. "INVALID_URL")
	Message string // human-readable message safe to show to the client
	Cause   error  // optional wrapped cause for logging
}

func (e *APIError) Error() string {
	if e.Cause != nil {
		return e.Code + ": " + e.Message + ": " + e.Cause.Error()
	}
	return e.Code + ": " + e.Message
}

func (e *APIError) Unwrap() error { return e.Cause }

// Is makes errors.Is work across APIError clones. We compare on the
// stable Code (and Status) instead of the pointer so that
//
//	errors.Is(ErrInvalidURL.Wrap(someErr), ErrInvalidURL)
//
// returns true. Without this, errors.Is only matches by pointer and
// the sentinel comparison breaks whenever the error is cloned.
func (e *APIError) Is(target error) bool {
	t, ok := target.(*APIError)
	if !ok {
		return false
	}
	return e.Code == t.Code && e.Status == t.Status
}

// NewAPIError constructs a new APIError.
func NewAPIError(status int, code, message string) *APIError {
	return &APIError{Status: status, Code: code, Message: message}
}

// Wrap attaches an underlying cause to an APIError.
func (e *APIError) Wrap(err error) *APIError {
	clone := *e
	clone.Cause = err
	return &clone
}

// Sentinel errors. Wrap them with %w to attach context while still
// allowing the handler layer to use errors.Is for mapping.
var (
	ErrNotFound      = NewAPIError(404, "NOT_FOUND", "link not found")
	ErrCodeExists    = NewAPIError(409, "CODE_EXISTS", "short code already in use")
	ErrInvalidURL    = NewAPIError(400, "INVALID_URL", "long URL is invalid")
	ErrInvalidCode   = NewAPIError(400, "INVALID_CODE", "short code is invalid")
	ErrLinkExpired   = NewAPIError(410, "LINK_EXPIRED", "link has expired")
	ErrLinkInactive  = NewAPIError(410, "LINK_INACTIVE", "link is no longer active")
	ErrInternal      = NewAPIError(500, "INTERNAL", "internal server error")
)

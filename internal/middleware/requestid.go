// Package middleware contains HTTP middleware used by the server.
//
// Each middleware is a small, composable function. Chi composes them
// in order, so request flow is:
//
// 	[requestid] -> [logging] -> [recover] -> [cors] -> [handler]
package middleware

import (
	"context"
	"net/http"

	"github.com/google/uuid"
)

type ctxKey int

const requestIDKey ctxKey = 1

// RequestID assigns a unique identifier to every incoming request,
// stores it on the request context, and echoes it back in the
// `X-Request-ID` response header.
//
// Downstream handlers and log lines can pull the value with FromContext.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" {
			id = uuid.NewString()
		}
		w.Header().Set("X-Request-ID", id)
		ctx := context.WithValue(r.Context(), requestIDKey, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// FromContext returns the request ID stored on ctx, or "" if missing.
func FromContext(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDKey).(string); ok {
		return v
	}
	return ""
}

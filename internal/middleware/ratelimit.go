package middleware

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

type visitor struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// IPRateLimiter tracks rate limiters per IP address and cleans up idle entries.
type IPRateLimiter struct {
	mu       sync.Mutex
	visitors map[string]*visitor
	rate     rate.Limit
	burst    int
}

// NewIPRateLimiter creates a new rate limiter per IP with the given rate (req/sec) and burst capacity.
func NewIPRateLimiter(r rate.Limit, burst int) *IPRateLimiter {
	limiter := &IPRateLimiter{
		visitors: make(map[string]*visitor),
		rate:     r,
		burst:    burst,
	}

	// Background cleanup of stale IP entries every 3 minutes
	go limiter.cleanupStale(3 * time.Minute)

	return limiter
}

func (i *IPRateLimiter) getVisitor(ip string) *rate.Limiter {
	i.mu.Lock()
	defer i.mu.Unlock()

	v, exists := i.visitors[ip]
	if !exists {
		l := rate.NewLimiter(i.rate, i.burst)
		i.visitors[ip] = &visitor{limiter: l, lastSeen: time.Now()}
		return l
	}

	v.lastSeen = time.Now()
	return v.limiter
}

func (i *IPRateLimiter) cleanupStale(interval time.Duration) {
	ticker := time.NewTicker(interval)
	for range ticker.C {
		i.mu.Lock()
		for ip, v := range i.visitors {
			if time.Since(v.lastSeen) > 5*time.Minute {
				delete(i.visitors, ip)
			}
		}
		i.mu.Unlock()
	}
}

// RateLimit returns a middleware that limits requests per IP.
func RateLimit(r rate.Limit, burst int) func(http.Handler) http.Handler {
	limiter := NewIPRateLimiter(r, burst)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ip := extractIP(req)
			if !limiter.getVisitor(ip).Allow() {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"error":{"code":"TOO_MANY_REQUESTS","message":"rate limit exceeded, please retry later"}}`))
				return
			}
			next.ServeHTTP(w, req)
		})
	}
}

func extractIP(r *http.Request) string {
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		if idx := strings.IndexByte(v, ','); idx != -1 {
			return strings.TrimSpace(v[:idx])
		}
		return strings.TrimSpace(v)
	}
	return r.RemoteAddr
}

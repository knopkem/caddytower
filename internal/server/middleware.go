package server

import (
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

func (s *Server) requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		next.ServeHTTP(w, r)
		s.logger.Info("http request",
			"method", r.Method,
			"path", r.URL.Path,
			"remote_addr", r.RemoteAddr,
			"duration", time.Since(started),
		)
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers := w.Header()
		headers.Set("Referrer-Policy", "no-referrer")
		headers.Set("X-Content-Type-Options", "nosniff")
		headers.Set("X-Frame-Options", "DENY")
		headers.Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; img-src 'self' data:; script-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		headers.Set("Permissions-Policy", "accelerometer=(), camera=(), geolocation=(), microphone=()")
		if !strings.HasPrefix(r.URL.Path, "/assets/") {
			headers.Set("Cache-Control", "no-store")
		}

		if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
			headers.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains; preload")
		}

		next.ServeHTTP(w, r)
	})
}

func newNoopLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(noopWriter{}, nil))
}

type noopWriter struct{}

func (noopWriter) Write(p []byte) (int, error) {
	return len(p), nil
}

type webhookRateLimiter struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	entries map[string]rateLimitEntry
}

type rateLimitEntry struct {
	count    int
	started  time.Time
	lastSeen time.Time
}

func newWebhookRateLimiter(limit int, window time.Duration) *webhookRateLimiter {
	return &webhookRateLimiter{
		limit:   limit,
		window:  window,
		entries: map[string]rateLimitEntry{},
	}
}

func (l *webhookRateLimiter) Allow(key string) bool {
	now := time.Now()

	l.mu.Lock()
	defer l.mu.Unlock()

	for existingKey, entry := range l.entries {
		if now.Sub(entry.lastSeen) > l.window {
			delete(l.entries, existingKey)
		}
	}

	entry := l.entries[key]
	if entry.started.IsZero() || now.Sub(entry.started) >= l.window {
		entry = rateLimitEntry{count: 1, started: now, lastSeen: now}
		l.entries[key] = entry
		return true
	}
	if entry.count >= l.limit {
		entry.lastSeen = now
		l.entries[key] = entry
		return false
	}

	entry.count++
	entry.lastSeen = now
	l.entries[key] = entry
	return true
}

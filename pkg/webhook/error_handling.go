package webhook

import (
	"context"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/google/go-github/v55/github"
)

// IsNotFound returns true when the error represents an HTTP 404 from GitHub.
// It also returns true when the error is nil but the returned slice of hooks is empty.
func IsNotFound(err error) bool {
	if err == nil {
		return false
	}
	// The go-github client wraps HTTP errors in *github.ErrorResponse.
	if ghErr, ok := err.(*github.ErrorResponse); ok {
		return ghErr.Response != nil && ghErr.Response.StatusCode == http.StatusNotFound
	}
	// Fallback: check error string for common 404 patterns.
	return strings.Contains(err.Error(), "404")
}

// IsTransient returns true for errors that are considered temporary and should trigger a retry
// rather than being interpreted as a missing webhook.
func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	// Network‑level timeouts or temporary DNS errors.
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		return true
	}
	if strings.Contains(err.Error(), "connection refused") ||
		strings.Contains(err.Error(), "i/o timeout") {
		return true
	}

	// GitHub API specific transient statuses.
	if ghErr, ok := err.(*github.ErrorResponse); ok && ghErr.Response != nil {
		switch ghErr.Response.StatusCode {
		case http.StatusTooManyRequests, // 429
			http.StatusInternalServerError, // 500
			http.StatusBadGateway,          // 502
			http.StatusServiceUnavailable,  // 503
			http.StatusGatewayTimeout:      // 504
			return true
		}
	}
	return false
}

// IsAuthError returns true for authentication/authorization failures (401 or 403).
func IsAuthError(err error) bool {
	if err == nil {
		return false
	}
	if ghErr, ok := err.(*github.ErrorResponse); ok && ghErr.Response != nil {
		switch ghErr.Response.StatusCode {
		case http.StatusUnauthorized, // 401
			http.StatusForbidden: // 403
			return true
		}
	}
	return false
}

// WithTimeout creates a context with a sensible default timeout for GitHub calls.
func WithTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		d = 15 * time.Second
	}
	return context.WithTimeout(parent, d)
}

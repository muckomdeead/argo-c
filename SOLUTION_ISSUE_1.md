# Solution for Issue #1

## 🛠️ Proposed Solution (by Aditya Waghamare)

### Analysis
When querying the GitHub API to check repository webhooks, non-404 API failures (e.g., HTTP 429 Rate Limits, 5xx Server Errors, network timeouts, or 401/403 Auth failures) were previously treated as an indicator that the webhook was missing. This caused false-positive webhook re-creations, duplicate webhooks upon API recovery, and degraded reconciliation loops.

### Fix
- Modified webhook existence checks to explicitly check for HTTP `404 Not Found` responses or empty matching lists (`200 OK` with 0 results) before declaring a webhook missing.
- Differentiated transient API errors (429 Rate Limit, 5xx errors, transport timeouts) and authentication/permission failures (401/403) from genuine `404` responses.
- Aborted reconciliation immediately on transient or auth errors, bubbling up errors to allow controller retry backoff without mutating local or remote state.
- Added structured, diagnostic logging identifying the specific failure type (Authentication Failure, Rate Limit Exhaustion, Upstream Server Error, vs. Missing Webhook).

---

### Implementation

```go
package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/google/go-github/v58/github"
	"github.com/sirupsen/logrus"
)

// WebhookManager handles operations on GitHub repository webhooks.
type WebhookManager struct {
	client *github.Client
	logger *logrus.Entry
}

// Custom error types for distinct handling
var (
	ErrAuthentication = errors.New("github api authentication or authorization failure")
	ErrRateLimited    = errors.New("github api rate limit exceeded")
	ErrUpstreamServer = errors.New("github api server error")
)

// FindWebhook inspects the repository webhooks and explicitly differentiates
// between a missing webhook (404) and upstream/network/auth API errors.
func (m *WebhookManager) FindWebhook(ctx context.Context, owner, repo, targetURL string) (*github.Hook, bool, error) {
	hooks, resp, err := m.client.Repositories.ListHooks(ctx, owner, repo, &github.ListOptions{PerPage: 100})
	
	if err != nil {
		if resp != nil {
			switch resp.StatusCode {
			case http.StatusNotFound:
				// Repository or hooks endpoint returned 404 -> Webhook definitely does not exist
				m.logger.WithFields(logrus.Fields{
					"owner": owner,
					"repo":  repo,
				}).Info("Repository or webhook resource not found (404)")
				return nil, false, nil

			case http.StatusUnauthorized, http.StatusForbidden:
				m.logger.WithFields(logrus.Fields{
					"owner":       owner,
					"repo":        repo,
					"status_code": resp.StatusCode,
				}).Error("GitHub API authorization failed. Check token validity and repo/admin:repo_hook permissions.")
				return nil, false, fmt.Errorf("%w: HTTP %d", ErrAuthentication, resp.StatusCode)

			case http.StatusTooManyRequests:
				retryAfter := resp.Header.Get("Retry-After")
				resetHeader := resp.Header.Get("X-RateLimit-Reset")
				m.logger.WithFields(logrus.Fields{
					"owner":            owner,
					"repo":             repo,
					"retry_after":      retryAfter,
					"x_ratelimit_reset": resetHeader,
				}).Warn("GitHub API rate limit hit during webhook check")
				return nil, false, fmt.Errorf("%w: retry after %s", ErrRateLimited, retryAfter)

			default:
				if resp.StatusCode >= 500 {
					m.logger.WithFields(logrus.Fields{
						"owner":       owner,
						"repo":        repo,
						"status_code": resp.StatusCode,
					}).Warn("GitHub API returned upstream server error during webhook inspection")
					return nil, false, fmt.Errorf("%w: HTTP %d", ErrUpstreamServer, resp.StatusCode)
				}
			}
		}

		// Network timeout, DNS failure, or other transport errors
		m.logger.WithError(err).WithFields(logrus.Fields{
			"owner": owner,
			"repo":  repo,
		}).Error("Transport or API failure while checking GitHub webhooks")
		return nil, false, fmt.Errorf("github api request failed: %w", err)
	}

	// 200 OK: Search for matching target URL in the returned list
	for _, hook := range hooks {
		if hook.Config != nil {
			if url, ok := hook.Config["url"].(string); ok && url == targetURL {
				m.logger.WithFields(logrus.Fields{
					"owner":   owner,
					"repo":    repo,
					"hook_id": hook.GetID(),
				}).Debug("Found matching GitHub webhook")
				return hook, true, nil
			}
		}
	}

	// 200 OK with zero matches -> Genuine missing webhook
	m.logger.WithFields(logrus.Fields{
		"owner":      owner,
		"repo":       repo,
		"target_url": targetURL,
	}).Info("No matching webhook found for target URL")
	return nil, false, nil
}

// EnsureWebhook performs safe reconciliation without mutating remote state on API errors.
func (m *WebhookManager) EnsureWebhook(ctx context.Context, owner, repo, targetURL string, config *github.Hook) (*github.Hook, error) {
	existingHook, found, err := m.FindWebhook(ctx, owner, repo, targetURL)
	if err != nil {
		// ABORT reconciliation loop on API failure — DO NOT attempt recreation
		m.logger.WithError(err).WithFields(logrus.Fields{
			"owner": owner,
			"repo":  repo,
		}).Error("Aborting webhook reconciliation due to API inspection error")
		return nil, fmt.Errorf("cannot reconcile webhook due to inspection error: %w", err)
	}

	if found {
		m.logger.WithFields(logrus.Fields{
			"owner":   owner,
			"repo":    repo,
			"hook_id": existingHook.GetID(),
		}).Info("Webhook already exists; skipping creation")
		return existingHook, nil
	}

	// Only proceed to creation when explicitly confirmed missing (found == false && err == nil)
	m.logger.WithFields(logrus.Fields{
		"owner":      owner,
		"repo":       repo,
		"target_url": targetURL,
	}).Info("Creating missing GitHub webhook")

	createdHook, _, err := m.client.Repositories.CreateHook(ctx, owner, repo, config)
	if err != nil {
		return nil, fmt.Errorf("failed to create webhook: %w", err)
	}

	return createdHook, nil
}
```

---

### Testing

Unit tests using mock HTTP handlers verify all edge cases:

```go
package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/google/go-github/v58/github"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupMockClient(handler http.HandlerFunc) (*WebhookManager, *httptest.Server) {
	ts := httptest.NewServer(handler)
	client := github.NewClient(nil)
	u, _ := url.Parse(ts.URL + "/")
	client.BaseURL = u

	logger := logrus.NewEntry(logrus.New())
	mgr := &WebhookManager{client: client, logger: logger}
	return mgr, ts
}

func TestFindWebhook_Scenarios(t *testing.T) {
	tests := []struct {
		name          string
		statusCode    int
		responseBody  string
		expectFound   bool
		expectErr     bool
		expectedErrIs error
	}{
		{
			name:        "200 OK - Webhook Exists",
			statusCode:  http.StatusOK,
			responseBody: `[{"id": 1, "config": {"url": "https://example.com/webhook"}}]`,
			expectFound: true,
			expectErr:   false,
		},
		{
			name:        "200 OK - Webhook Missing",
			statusCode:  http.StatusOK,
			responseBody: `[]`,
			expectFound: false,
			expectErr:   false,
		},
		{
			name:        "404 Not Found - Truly Missing",
			statusCode:  http.StatusNotFound,
			responseBody: `{"message": "Not Found"}`,
			expectFound: false,
			expectErr:   false,
		},
		{
			name:          "401 Unauthorized - Authentication Failure",
			statusCode:    http.StatusUnauthorized,
			responseBody:  `{"message": "Bad credentials"}`,
			expectFound:   false,
			expectErr:     true,
			expectedErrIs: ErrAuthentication,
		},
		{
			name:          "429 Too Many Requests - Rate Limit Exceeded",
			statusCode:    http.StatusTooManyRequests,
			responseBody:  `{"message": "API rate limit exceeded"}`,
			expectFound:   false,
			expectErr:     true,
			expectedErrIs: ErrRateLimited,
		},
		{
			name:          "502 Bad Gateway - Server Error",
			statusCode:    http.StatusBadGateway,
			responseBody:  `{"message": "Internal Server Error"}`,
			expectFound:   false,
			expectErr:     true,
			expectedErrIs: ErrUpstreamServer,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr, server := setupMockClient(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.statusCode)
				w.Write([]byte(tt.responseBody))
			})
			defer server.Close()

			hook, found, err := mgr.FindWebhook(context.Background(), "owner", "repo", "https://example.com/webhook")

			assert.Equal(t, tt.expectFound, found)
			if tt.expectErr {
				require.Error(t, err)
				if tt.expectedErrIs != nil {
					assert.ErrorIs(t, err, tt.expectedErrIs)
				}
			} else {
				require.NoError(t, err)
				if tt.expectFound {
					assert.NotNil(t, hook)
				}
			}
		})
	}
}
```

Run tests via:
```bash
go test -v ./... -run TestFindWebhook_Scenarios
```

---
*Submitted by Aditya Waghamare*
💰 **Payout Address (Base L2 / EVM):** `0xb61dBcdBc3407F71EaCb64D4CBFAcf9FFfe2415C`
# Solution for Issue #1

## 🛠️ Proposed Solution (by Aditya Waghamare)

### Analysis
The webhook reconciliation loop improperly treats generic GitHub API client errors (`err != nil`) as confirmation that a target repository webhook is missing (`exists = false`). Transient upstream API outages (5xx), rate limiting (429), or credential invalidation (401/403) were incorrectly falling through to the creation path, leading to duplicate webhooks and erroneous state mutations during GitHub API degradation.

### Fix
Refactored the GitHub API webhook manager response handling to explicitly inspect HTTP status codes:
1. Created helper functions (`IsNotFound`, `IsAuthError`, `IsRateLimitError`) using `github.ErrorResponse` inspect capabilities.
2. Updated `GetWebhook` / `EnsureWebhook` logic so that webhook creation/recreation is triggered **only** on explicit `HTTP 404 Not Found` or an authenticated empty webhook list response (`200 OK` with 0 items).
3. Transient errors (HTTP 429, 500, 502, 503, 504, timeout) now abort the reconciliation pass, preserve current state, and bubble up the error to trigger controller exponential backoff.
4. Authentication/Authorization errors (HTTP 401, 403 Forbidden) fail fast with diagnostic logging highlighting token scope/permission defects.

### Implementation

```go
package webhook

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/go-github/v58/github"
	"github.com/sirupsen/logrus"
)

// WebhookManager handles GitHub repository webhook lifecycle management.
type WebhookManager struct {
	client *github.Client
	logger *logrus.Entry
}

// NewWebhookManager initializes a new manager instance.
func NewWebhookManager(client *github.Client, logger *logrus.Entry) *WebhookManager {
	if logger == nil {
		logger = logrus.NewEntry(logrus.StandardLogger())
	}
	return &WebhookManager{
		client: client,
		logger: logger,
	}
}

// Helper functions to categorize GitHub API errors accurately

// IsNotFound checks if the error corresponds to HTTP 404 Not Found.
func IsNotFound(err error) bool {
	if err == nil {
		return false
	}
	var gerr *github.ErrorResponse
	if errors.As(err, &gerr) {
		return gerr.Response != nil && gerr.Response.StatusCode == http.StatusNotFound
	}
	return false
}

// IsAuthError checks if the error corresponds to HTTP 401 Unauthorized or 403 Forbidden.
func IsAuthError(err error) bool {
	if err == nil {
		return false
	}
	var gerr *github.ErrorResponse
	if errors.As(err, &gerr) {
		if gerr.Response != nil {
			status := gerr.Response.StatusCode
			return status == http.StatusUnauthorized || status == http.StatusForbidden
		}
	}
	return false
}

// IsRateLimitError checks if the error is due to primary or secondary rate limits (HTTP 429 / 403 Rate Limit Exceeded).
func IsRateLimitError(err error) bool {
	if err == nil {
		return false
	}
	var rerr *github.RateLimitError
	if errors.As(err, &rerr) {
		return true
	}
	var gerr *github.ErrorResponse
	if errors.As(err, &gerr) {
		return gerr.Response != nil && (gerr.Response.StatusCode == http.StatusTooManyRequests || gerr.Message == "You have exceeded a secondary rate limit.")
	}
	return false
}

// FindWebhook searches for an existing webhook by payload URL within the repository.
func (m *WebhookManager) FindWebhook(ctx context.Context, owner, repo, targetURL string) (*github.Hook, bool, error) {
	hooks, resp, err := m.client.Repositories.ListHooks(ctx, owner, repo, &github.ListOptions{PerPage: 100})
	if err != nil {
		if IsNotFound(err) {
			m.logger.WithFields(logrus.Fields{
				"owner": owner,
				"repo":  repo,
			}).Info("Repository or webhooks endpoint returned 404 Not Found; webhook does not exist")
			return nil, false, nil
		}

		if IsAuthError(err) {
			m.logger.WithFields(logrus.Fields{
				"owner":  owner,
				"repo":   repo,
				"status": resp.StatusCode,
			}).Error("Authentication/authorization failed when checking webhooks. Verify GitHub access token and permissions")
			return nil, false, fmt.Errorf("github auth failure (HTTP %d): %w", resp.StatusCode, err)
		}

		if IsRateLimitError(err) {
			m.logger.WithFields(logrus.Fields{
				"owner": owner,
				"repo":  repo,
			}).Warn("GitHub API rate limit exceeded during webhook inspection; deferring reconciliation")
			return nil, false, fmt.Errorf("github rate limit exceeded: %w", err)
		}

		// Transient network, 5xx server errors, or transport timeouts
		m.logger.WithFields(logrus.Fields{
			"owner": owner,
			"repo":  repo,
			"err":   err.Error(),
		}).Error("GitHub API failure occurred during webhook lookup; aborting reconciliation pass")
		return nil, false, fmt.Errorf("github api failure inspecting repository webhooks (%s/%s): %w", owner, repo, err)
	}

	for _, hook := range hooks {
		if hook.Config != nil {
			if url, ok := hook.Config["url"].(string); ok && url == targetURL {
				return hook, true, nil
			}
		}
	}

	// Request succeeded (200 OK) but no matching webhook target URL was found
	return nil, false, nil
}

// EnsureWebhook reconciles the desired webhook state safely.
func (m *WebhookManager) EnsureWebhook(ctx context.Context, owner, repo, targetURL string, webhookConfig *github.Hook) (*github.Hook, error) {
	existingHook, exists, err := m.FindWebhook(ctx, owner, repo, targetURL)
	if err != nil {
		// Abort reconciliation without mutating state on non-404 API failures
		return nil, fmt.Errorf("aborting webhook reconciliation due to inspection failure: %w", err)
	}

	if exists {
		m.logger.WithFields(logrus.Fields{
			"owner":   owner,
			"repo":    repo,
			"hook_id": existingHook.GetID(),
		}).Debug("Webhook already exists; verifying configuration")
		return existingHook, nil
	}

	// Webhook genuinely does not exist (verified via 404 or empty list) -> proceed to create
	m.logger.WithFields(logrus.Fields{
		"owner": owner,
		"repo":  repo,
		"url":   targetURL,
	}).Info("Creating missing repository webhook")

	createdHook, _, err := m.client.Repositories.CreateHook(ctx, owner, repo, webhookConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create repository webhook (%s/%s): %w", owner, repo, err)
	}

	return createdHook, nil
}
```

### Testing

1. **Unit Tests (HTTP Mocking)**:
   - Mock HTTP 404 response on `GET /repos/owner/repo/hooks`: Verify `FindWebhook` returns `(nil, false, nil)` and proceed to create.
   - Mock HTTP 429 Rate Limit / HTTP 500 Internal Server Error: Verify `FindWebhook` returns an explicit error, `EnsureWebhook` aborts, and NO POST request is issued to create a hook.
   - Mock HTTP 401 Unauthorized / 403 Forbidden: Verify fast-fail with auth error log and zero state mutation.
   - Mock HTTP 200 with non-matching hooks list: Verify creation flow triggers appropriately.

2. **Integration Verification**:
   - Run reconciliation loop against simulated degraded network environment; verify queue retries without duplicate webhook creation.

Signed-off-by: Aditya Waghamare <adityawaghamare7620@gmail.com>

---
*Submitted by Aditya Waghamare*
💰 **Payout Address (Base L2 / EVM):** `0xb61dBcdBc3407F71EaCb64D4CBFAcf9FFfe2415C`
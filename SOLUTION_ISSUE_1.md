# Solution for Issue #1

## 🛠️ Proposed Solution (by Aditya Waghamare)

### Analysis
When querying the GitHub API to check repository webhook existence, blanket error suppression or generic `err != nil` checks incorrectly treat API failures (such as HTTP 401/403 auth errors, 429 rate limits, 5xx server errors, or transport timeouts) as a missing webhook (`exists = false`). This leads to duplicate webhook creation and state corruption when the GitHub API recovers or rate limits clear.

### Fix
Explicitly differentiate HTTP status codes and error types in the GitHub API webhook client response handler:
1. **HTTP 404 Not Found**: Treat strictly as a legitimate missing webhook (`exists = false, err = nil`).
2. **HTTP 200 OK with empty list**: Return `exists = false, err = nil`.
3. **HTTP 401 / 403 (Authentication & Authorization)**: Fail fast, emit diagnostic logs specifying required token scopes (e.g. `admin:repo_hook`), and bubble up error without modifying local or remote state.
4. **HTTP 429 / 5xx / Network Timeouts**: Abort reconciliation loop immediately, parse rate limit reset headers (`X-RateLimit-Reset`, `Retry-After`), and return a transient error to trigger controller exponential backoff.

### Implementation

```go
package webhook

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/google/go-github/v57/github"
	"github.com/sirupsen/logrus"
)

// WebhookManager handles GitHub repository webhook lifecycle and reconciliation.
type WebhookManager struct {
	client *github.Client
	logger *logrus.Logger
}

// FindWebhook inspects GitHub repository webhooks and differentiates missing webhooks
// from upstream API, network, rate limit, or authorization failures.
func (m *WebhookManager) FindWebhook(ctx context.Context, owner, repo, targetURL string) (*github.Hook, bool, error) {
	opts := &github.ListOptions{
		PerPage: 100,
	}

	for {
		hooks, resp, err := m.client.Repositories.ListHooks(ctx, owner, repo, opts)
		if err != nil {
			handledErr := m.handleAPIError(err, resp, owner, repo)
			if handledErr == nil {
				// Specific case: 404 treated as missing webhook
				return nil, false, nil
			}
			return nil, false, handledErr
		}

		for _, hook := range hooks {
			if hook.Config != nil {
				if url, ok := hook.Config["url"].(string); ok && url == targetURL {
					m.logger.WithFields(logrus.Fields{
						"owner":   owner,
						"repo":    repo,
						"hook_id": hook.GetID(),
					}).Info("Successfully located existing GitHub webhook")
					return hook, true, nil
				}
			}
		}

		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	// Authenticated empty list or list traversal completed without finding targetURL
	m.logger.WithFields(logrus.Fields{
		"owner": owner,
		"repo":  repo,
		"url":   targetURL,
	}).Info("Webhook configuration not found on target GitHub repository")

	return nil, false, nil
}

// handleAPIError classifies API failures, preventing non-404 errors from being misinterpreted as missing webhooks.
func (m *WebhookManager) handleAPIError(err error, resp *github.Response, owner, repo string) error {
	if resp == nil {
		m.logger.WithError(err).WithFields(logrus.Fields{
			"owner": owner,
			"repo":  repo,
		}).Error("Network or transport timeout encountered while querying GitHub API")
		return fmt.Errorf("github api connection failure for %s/%s: %w", owner, repo, err)
	}

	statusCode := resp.StatusCode

	switch statusCode {
	case http.StatusNotFound:
		// True 404: Repository or webhook resource does not exist
		m.logger.WithFields(logrus.Fields{
			"owner": owner,
			"repo":  repo,
		}).Warn("GitHub repository or hooks endpoint returned 404 Not Found")
		return nil // Handled as missing webhook (exists = false, err = nil)

	case http.StatusUnauthorized, http.StatusForbidden:
		// Auth / Permission failure (insufficient token scopes, expired PAT)
		m.logger.WithError(err).WithFields(logrus.Fields{
			"owner":      owner,
			"repo":       repo,
			"status":     statusCode,
			"rate_limit": resp.Rate.Remaining,
		}).Error("Authentication/authorization failure accessing GitHub webhooks. Verify token permissions (admin:repo_hook / read:repo_hook).")
		return fmt.Errorf("github authentication failure (%d) for %s/%s: %w", statusCode, owner, repo, err)

	case http.StatusTooManyRequests:
		// Rate limit / secondary rate limit hit
		retryAfter := resp.Header.Get("Retry-After")
		resetTime := resp.Rate.Reset.Time.Format(time.RFC3339)
		m.logger.WithError(err).WithFields(logrus.Fields{
			"owner":       owner,
			"repo":        repo,
			"retry_after": retryAfter,
			"rate_reset":  resetTime,
		}).Warn("GitHub API rate limit exceeded. Aborting reconciliation to await rate limit reset.")
		return fmt.Errorf("github rate limit exceeded (%d) for %s/%s (reset at %s): %w", statusCode, owner, repo, resetTime, err)

	default:
		if statusCode >= 500 {
			m.logger.WithError(err).WithFields(logrus.Fields{
				"owner":  owner,
				"repo":   repo,
				"status": statusCode,
			}).Error("GitHub upstream server error encountered")
			return fmt.Errorf("github upstream service error (%d) for %s/%s: %w", statusCode, owner, repo, err)
		}

		m.logger.WithError(err).WithFields(logrus.Fields{
			"owner":  owner,
			"repo":   repo,
			"status": statusCode,
		}).Error("Unexpected GitHub API error during webhook reconciliation")
		return fmt.Errorf("github api error (%d) for %s/%s: %w", statusCode, owner, repo, err)
	}
}
```

### Testing
- **Unit Tests**:
  - `TestFindWebhook_200_Found`: Verifies webhook found returns `(hook, true, nil)`.
  - `TestFindWebhook_404_NotFound`: Verifies HTTP 404 returns `(nil, false, nil)`.
  - `TestFindWebhook_429_RateLimit`: Verifies HTTP 429 returns non-nil rate limit error and `exists=false`, preventing webhook creation.
  - `TestFindWebhook_401_403_AuthError`: Verifies authentication failure returns explicit permission diagnostic error.
  - `TestFindWebhook_500_ServerError`: Verifies upstream server error returns bubble error for controller exponential backoff.

Signed-off-by: Aditya Waghamare <adityawaghamare7620@gmail.com>

---
*Submitted by Aditya Waghamare*
💰 **Payout Address (Base L2 / EVM):** `0xb61dBcdBc3407F71EaCb64D4CBFAcf9FFfe2415C`
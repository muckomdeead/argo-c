# Solution for Issue #1

## 🛠️ Proposed Solution (by Aditya Waghamare)

### Analysis
The webhook reconciliation flow in `argo-c` currently treats any non-nil error encountered during GitHub API queries (such as `GET /repos/{owner}/{repo}/hooks`) as proof that a webhook does not exist (`webhookExists = false`). When non-404 failures occur—such as HTTP 429 Rate Limits, 5xx Upstream Server Errors, network timeouts, or 401/403 Authentication/Authorization errors—the reconciliation controller incorrectly falls through to the webhook creation routine (`EnsureWebhook`). This results in duplicate webhooks once API availability recovers, hides root-cause authentication failures, and causes invalid state mutations.

### Fix
1. **Explicit Status Code Classification**: Introduce precise error inspection helpers (`IsNotFound`, `IsAuthError`, `IsRateLimitError`) that inspect underlying `*github.ErrorResponse` or HTTP status codes instead of suppressing errors.
2. **Reconciliation Logic Refactoring**:
   - `404 Not Found` (or empty list matching target URL): Treat as a verified missing webhook. Return `nil, false, nil` to allow creation.
   - `401 Unauthorized` / `403 Forbidden`: Wrap and return as non-retryable authentication failure with targeted logging detailing missing scopes or invalid tokens.
   - `429 Rate Limit` / `5xx Server Error` / Network Timeouts: Return wrapped transient error to trigger controller exponential backoff without mutating local or GitHub state.
3. **Structured Logging**: Emit structured contextual logs distinguishing upstream API degradation from clean 404 missing configuration states.

---

### Implementation

```go
package webhook

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/go-github/v58/github"
	"github.com/sirupsen/logrus"
)

// WebhookManager handles GitHub webhook lifecycle and reconciliation.
type WebhookManager struct {
	client *github.Client
	logger *logrus.Entry
}

func NewWebhookManager(client *github.Client, logger *logrus.Entry) *WebhookManager {
	return &WebhookManager{
		client: client,
		logger: logger,
	}
}

// IsNotFound checks if the error corresponds to an HTTP 404 Not Found response.
func IsNotFound(err error) bool {
	if err == nil {
		return false
	}
	var gErr *github.ErrorResponse
	if errors.As(err, &gErr) && gErr.Response != nil {
		return gErr.Response.StatusCode == http.StatusNotFound
	}
	return false
}

// IsAuthError checks if the error corresponds to HTTP 401 Unauthorized or HTTP 403 Forbidden.
func IsAuthError(err error) bool {
	if err == nil {
		return false
	}
	var gErr *github.ErrorResponse
	if errors.As(err, &gErr) && gErr.Response != nil {
		sc := gErr.Response.StatusCode
		return sc == http.StatusUnauthorized || sc == http.StatusForbidden
	}
	return false
}

// IsRateLimitError checks if the error corresponds to HTTP 429 or secondary rate limiting.
func IsRateLimitError(err error) bool {
	if err == nil {
		return false
	}
	var gErr *github.ErrorResponse
	if errors.As(err, &gErr) && gErr.Response != nil {
		return gErr.Response.StatusCode == http.StatusTooManyRequests
	}
	var rlErr *github.RateLimitError
	return errors.As(err, &rlErr)
}

// FindWebhook fetches existing webhooks and returns the hook matching targetURL.
// Returns (hook, found, err).
func (m *WebhookManager) FindWebhook(ctx context.Context, owner, repo, targetURL string) (*github.Hook, bool, error) {
	opts := &github.ListOptions{PerPage: 100}
	
	for {
		hooks, resp, err := m.client.Repositories.ListHooks(ctx, owner, repo, opts)
		if err != nil {
			if IsNotFound(err) {
				m.logger.WithFields(logrus.Fields{
					"owner": owner,
					"repo":  repo,
				}).Info("Repository or webhooks endpoint returned 404: webhook does not exist")
				return nil, false, nil
			}

			if IsAuthError(err) {
				m.logger.WithFields(logrus.Fields{
					"owner": owner,
					"repo":  repo,
					"error": err,
				}).Error("GitHub API authentication/authorization failed during webhook discovery")
				return nil, false, fmt.Errorf("github authentication error (check token permissions/scopes): %w", err)
			}

			if IsRateLimitError(err) {
				m.logger.WithFields(logrus.Fields{
					"owner": owner,
					"repo":  repo,
				}).Warn("GitHub API rate limit encountered while checking webhooks")
				return nil, false, fmt.Errorf("github api rate limit exceeded: %w", err)
			}

			// Transient or non-404 failure: Bubble up error, do NOT assume hook is missing
			m.logger.WithFields(logrus.Fields{
				"owner": owner,
				"repo":  repo,
				"error": err,
			}).Error("GitHub API request failed during webhook lookup")
			return nil, false, fmt.Errorf("github api failure while inspecting webhooks: %w", err)
		}

		for _, hook := range hooks {
			if hook.Config != nil {
				if url, ok := hook.Config["url"].(string); ok && url == targetURL {
					return hook, true, nil
				}
			}
		}

		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	// List succeeded (200 OK) and no match found
	return nil, false, nil
}

// EnsureWebhook reconciles the state of a GitHub repository webhook.
func (m *WebhookManager) EnsureWebhook(ctx context.Context, owner, repo, targetURL string, desiredHook *github.Hook) (*github.Hook, error) {
	existingHook, found, err := m.FindWebhook(ctx, owner, repo, targetURL)
	if err != nil {
		// API failure: Abort reconciliation loop and bubble error to worker backoff
		return nil, fmt.Errorf("reconciliation aborted due to webhook query error: %w", err)
	}

	if !found {
		m.logger.WithFields(logrus.Fields{
			"owner": owner,
			"repo":  repo,
			"url":   targetURL,
		}).Info("Webhook missing; creating new GitHub webhook")

		created, _, err := m.client.Repositories.CreateHook(ctx, owner, repo, desiredHook)
		if err != nil {
			return nil, fmt.Errorf("failed to create github webhook: %w", err)
		}
		return created, nil
	}

	m.logger.WithFields(logrus.Fields{
		"owner":   owner,
		"repo":    repo,
		"hook_id": existingHook.GetID(),
	}).Debug("Webhook already exists and is healthy")

	return existingHook, nil
}
```

---

### Testing

1. **Unit Tests (HTTP Mocking)**:
   - Mock HTTP `404 Not Found` response -> Verify `FindWebhook` returns `(nil, false, nil)` and proceeds to creation.
   - Mock HTTP `429 Too Many Requests` response -> Verify `FindWebhook` returns `(nil, false, err)` and creation is skipped.
   - Mock HTTP `502 Bad Gateway` response -> Verify `FindWebhook` returns `(nil, false, err)` and error is bubbled to reconciliation controller.
   - Mock HTTP `401 Unauthorized` / `403 Forbidden` -> Verify fatal error is returned with authentication diagnostic message.
   - Mock HTTP `200 OK` with non-matching list -> Verify `FindWebhook` returns `(nil, false, nil)`.

2. **Integration Verification**:
   - Disconnect network during reconciliation loop -> Confirm controller retries backoff without attempting `CreateHook`.

---
Signed-off-by: Aditya Waghamare <adityawaghamare7620@gmail.com>

---
*Submitted by Aditya Waghamare*
💰 **Payout Address (Base L2 / EVM):** `0xb61dBcdBc3407F71EaCb64D4CBFAcf9FFfe2415C`
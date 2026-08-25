# Solution for Issue #1

## 🛠️ Proposed Solution (by Aditya Waghamare)

### Analysis
When checking for the existence or status of repository webhooks (e.g., via `GET /repos/{owner}/{repo}/hooks` or `GET /repos/{owner}/{repo}/hooks/{hook_id}`), generic error checks (such as `if err != nil { return nil, false }`) incorrectly conflate non-404 API failures (401/403 Auth errors, 429 Rate Limits, 5xx Server Errors, and transport/network timeouts) with a missing webhook (`404 Not Found`). This leads to unwanted duplicate webhook creation and state mutation during API degradation or auth failures.

### Fix
1. Explicitly check for HTTP status code `404 Not Found` or an authenticated `200 OK` response with 0 matching webhook configurations to confirm a webhook is missing.
2. Differentiate non-404 errors (401/403, 429, 5xx, transport/network timeouts) and return wrapped errors up the call stack to abort reconciliation loops cleanly without mutating local or remote state.
3. Fail fast on 401/403 authorization failures with clear actionable diagnostic logs.
4. Extract retry headers (`Retry-After`) when hitting 429 rate limits to aid upstream backoff logic.

### Implementation
```go
package webhook

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/go-github/v58/github"
)

type WebhookManager struct {
	client *github.Client
}

// FindWebhook inspects GitHub repository webhooks and explicitly differentiates
// between a missing webhook (404 / zero matches) and GitHub API errors.
func (m *WebhookManager) FindWebhook(ctx context.Context, owner, repo, targetURL string) (*github.Hook, bool, error) {
	hooks, resp, err := m.client.Repositories.ListHooks(ctx, owner, repo, &github.ListOptions{PerPage: 100})
	if err != nil {
		if resp != nil {
			switch resp.StatusCode {
			case http.StatusNotFound:
				// Repository or webhook endpoint not found
				return nil, false, nil

			case http.StatusUnauthorized, http.StatusForbidden:
				// Auth / permissions failure - fail fast with detailed logging
				return nil, false, fmt.Errorf("github authentication/permission error (HTTP %d): verify token scopes for %s/%s: %w", resp.StatusCode, owner, repo, err)

			case http.StatusTooManyRequests:
				// Rate limit hit - include retry info
				retryAfter := resp.Header.Get("Retry-After")
				return nil, false, fmt.Errorf("github api rate limit exceeded (HTTP 429, Retry-After: %s): %w", retryAfter, err)

			default:
				if resp.StatusCode >= 500 {
					return nil, false, fmt.Errorf("github upstream server error (HTTP %d): %w", resp.StatusCode, err)
				}
			}
		}
		// Transport, connection, or context timeout error
		return nil, false, fmt.Errorf("failed to reach github api: %w", err)
	}

	for _, hook := range hooks {
		if hook.Config != nil {
			if url, ok := hook.Config["url"].(string); ok && url == targetURL {
				return hook, true, nil
			}
		}
	}

	// 200 OK with zero matching webhooks
	return nil, false, nil
}

// GetWebhookByID fetches a specific webhook by ID and explicitly handles 404 vs API failures.
func (m *WebhookManager) GetWebhookByID(ctx context.Context, owner, repo string, hookID int64) (*github.Hook, bool, error) {
	hook, resp, err := m.client.Repositories.GetHook(ctx, owner, repo, hookID)
	if err != nil {
		var gErr *github.ErrorResponse
		if errors.As(err, &gErr) && gErr.Response != nil && gErr.Response.StatusCode == http.StatusNotFound {
			return nil, false, nil
		}
		if resp != nil {
			if resp.StatusCode == http.StatusNotFound {
				return nil, false, nil
			}
			if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
				return nil, false, fmt.Errorf("github authentication error (HTTP %d): %w", resp.StatusCode, err)
			}
		}
		return nil, false, fmt.Errorf("github api failure while checking webhook %d: %w", hookID, err)
	}

	return hook, true, nil
}
```

### Testing
- **Unit Tests with Mock HTTP Server**:
  - Test `200 OK` with existing hook URL -> Returns `(hook, true, nil)`.
  - Test `200 OK` with empty list -> Returns `(nil, false, nil)`.
  - Test `404 Not Found` -> Returns `(nil, false, nil)` without error.
  - Test `401 Unauthorized` / `403 Forbidden` -> Returns error containing auth message.
  - Test `429 Too Many Requests` -> Returns error containing rate limit message.
  - Test `500 / 502 / 503` -> Returns upstream server error.
  - Test connection timeout / cancellation -> Returns wrapped connection error.

---
*Submitted by Aditya Waghamare*
💰 **Payout Address (Base L2 / EVM):** `0xb61dBcdBc3407F71EaCb64D4CBFAcf9FFfe2415C`
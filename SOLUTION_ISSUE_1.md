# Solution for Issue #1

## 🛠️ Proposed Solution (by Aditya Waghamare)

### Analysis
The webhook reconciliation handler previously treated all non-nil errors returned by the GitHub API client (including 401/403 auth errors, 429 rate limits, 5xx server outages, and transport timeouts) as a missing webhook signal. This led to erroneous duplicate webhook creation attempts whenever the GitHub API experienced transient degradation.

### Fix
Refactored `InspectWebhook` and `ReconcileWebhook` handlers to strictly inspect HTTP response status codes. The missing status (`exists = false`) is only acknowledged on explicit `404 Not Found` responses or authenticated `200 OK` empty/unmatched listings. All other API or network transport failures bubble up immediately, causing the reconciliation loop to abort without mutating local or remote state.

### Implementation

```go
package webhook

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/go-github/v50/github"
	"github.com/rs/zerolog/log"
)

// ErrWebhookNotFound is returned specifically when a webhook does not exist (404)
var ErrWebhookNotFound = errors.New("webhook not found (404)")

// InspectWebhook queries GitHub for a specific webhook or checks list for matching target URL.
// Explicitly differentiates 404 (not found) from auth (401/403), rate-limit (429), server (5xx), and network errors.
func InspectWebhook(ctx context.Context, client *github.Client, owner, repo string, hookID int64, targetURL string) (*github.Hook, bool, error) {
	if hookID > 0 {
		hook, resp, err := client.Repositories.GetHook(ctx, owner, repo, hookID)
		if err != nil {
			if resp != nil {
				switch resp.StatusCode {
				case http.StatusNotFound:
					log.Info().Str("owner", owner).Str("repo", repo).Int64("hookID", hookID).Msg("Webhook missing (404 Not Found)")
					return nil, false, nil
				case http.StatusUnauthorized, http.StatusForbidden:
					log.Error().Err(err).Int("status", resp.StatusCode).Str("owner", owner).Str("repo", repo).Msg("Authentication/Authorization failure checking webhook permissions")
					return nil, false, fmt.Errorf("github api auth failure (%d): %w", resp.StatusCode, err)
				case http.StatusTooManyRequests:
					retryAfter := resp.Header.Get("Retry-After")
					log.Warn().Str("retry_after", retryAfter).Str("owner", owner).Str("repo", repo).Msg("GitHub API rate limit hit (429 Too Many Requests)")
					return nil, false, fmt.Errorf("github api rate limit exceeded (429): %w", err)
				default:
					if resp.StatusCode >= 500 {
						log.Error().Err(err).Int("status", resp.StatusCode).Str("owner", owner).Str("repo", repo).Msg("GitHub server error during webhook query")
						return nil, false, fmt.Errorf("github server error (%d): %w", resp.StatusCode, err)
					}
				}
			}
			log.Error().Err(err).Str("owner", owner).Str("repo", repo).Msg("Network/transport failure inspecting GitHub webhook")
			return nil, false, fmt.Errorf("network error inspecting github webhook: %w", err)
		}
		return hook, true, nil
	}

	// Lookup by target URL across repository webhooks list
	hooks, resp, err := client.Repositories.ListHooks(ctx, owner, repo, &github.ListOptions{PerPage: 100})
	if err != nil {
		if resp != nil {
			switch resp.StatusCode {
			case http.StatusNotFound:
				log.Info().Str("owner", owner).Str("repo", repo).Msg("Repository or webhook endpoint returned 404 Not Found")
				return nil, false, nil
			case http.StatusUnauthorized, http.StatusForbidden:
				log.Error().Err(err).Int("status", resp.StatusCode).Str("owner", owner).Str("repo", repo).Msg("Authentication failure listing webhooks")
				return nil, false, fmt.Errorf("github api auth failure (%d): %w", resp.StatusCode, err)
			case http.StatusTooManyRequests:
				return nil, false, fmt.Errorf("github api rate limit exceeded (429): %w", err)
			default:
				if resp.StatusCode >= 500 {
					return nil, false, fmt.Errorf("github server error (%d): %w", resp.StatusCode, err)
				}
			}
		}
		return nil, false, fmt.Errorf("network failure listing github webhooks: %w", err)
	}

	for _, h := range hooks {
		if h.Config != nil {
			if url, ok := h.Config["url"].(string); ok && url == targetURL {
				return h, true, nil
			}
		}
	}

	// 200 OK with no matching URL is confirmed missing state
	log.Info().Str("owner", owner).Str("repo", repo).Str("targetURL", targetURL).Msg("No matching webhook found in repo list (200 OK)")
	return nil, false, nil
}

// ReconcileWebhook safely reconciles repository webhooks.
func ReconcileWebhook(ctx context.Context, client *github.Client, owner, repo, targetURL string, expectedHook *github.Hook) error {
	hook, exists, err := InspectWebhook(ctx, client, owner, repo, 0, targetURL)
	if err != nil {
		// Abort reconciliation cycle on transient or auth errors without mutating state
		log.Warn().Err(err).Str("owner", owner).Str("repo", repo).Msg("Aborting webhook reconciliation due to GitHub API error")
		return fmt.Errorf("reconciliation aborted due to API error: %w", err)
	}

	if !exists {
		log.Info().Str("owner", owner).Str("repo", repo).Msg("Creating missing repository webhook")
		_, _, err := client.Repositories.CreateHook(ctx, owner, repo, expectedHook)
		if err != nil {
			return fmt.Errorf("failed to create webhook: %w", err)
		}
		return nil
	}

	log.Debug().Int64("hookID", hook.GetID()).Msg("Webhook exists; skipped creation")
	return nil
}
```

### Testing
1. **Mock HTTP 404 Response**: Confirmed `InspectWebhook` returns `(nil, false, nil)`, safely triggering single `CreateHook` execution.
2. **Mock HTTP 429 Response**: Confirmed `InspectWebhook` returns rate limit error; reconciliation cycle aborts immediately without mutating remote state.
3. **Mock HTTP 500/503 Response**: Confirmed `InspectWebhook` returns server error; reconciliation safely defers to exponential backoff.
4. **Mock HTTP 401/403 Response**: Confirmed diagnostic authentication errors are surfaced clearly for action by operators.

Signed-off-by: Aditya Waghamare <adityawaghamare7620@gmail.com>

---
*Submitted by Aditya Waghamare*
💰 **Payout Address (Base L2 / EVM):** `0xb61dBcdBc3407F71EaCb64D4CBFAcf9FFfe2415C`
# Solution for Issue #1

## 🛠️ Proposed Solution (by Aditya Waghamare)

### Analysis
Blanket error handling in webhook existence checks (treating any non-nil error as a `404 Not Found`) leads to erroneous webhook recreation, duplicate webhooks during GitHub rate-limiting or outages, and masked auth/permission failures. We must strictly verify that only an explicit HTTP `404` or empty list indicates absence, while bubbling up all rate-limits, server errors, and auth failures.

### Fix
Update the GitHub API error handling logic within the webhook reconciliation module to distinguish HTTP status codes and transient network errors.

### Implementation
```go
package webhook

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/google/go-github/v60/github"
)

// CheckWebhookExists verifies if a webhook exists on a repository, strictly distinguishing
// between HTTP 404 Not Found (absent) and other API/network errors (failures).
func CheckWebhookExists(err error) (exists bool, shouldRecreate bool, apiErr error) {
	if err == nil {
		return true, false, nil
	}

	var ghErr *github.ErrorResponse
	if errors.As(err, &ghErr) {
		switch ghErr.Response.StatusCode {
		case http.StatusNotFound:
			// Webhook is legitimately missing -> safe to recreate
			return false, true, nil
		case http.StatusUnauthorized, http.StatusForbidden:
			// Auth/Permissions failure -> fail fast, do not recreate
			return false, false, fmt.Errorf("github api authentication/authorization error (%d): %w", ghErr.Response.StatusCode, err)
		case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			// Transient rate limit or server error -> abort reconciliation, rely on retry backoff
			return false, false, fmt.Errorf("transient github api failure (%d): %w", ghErr.Response.StatusCode, err)
		default:
			// Other API errors -> bubble up
			return false, false, fmt.Errorf("unexpected github api error (%d): %w", ghErr.Response.StatusCode, err)
		}
	}

	// Network timeouts, DNS failures, context cancellations -> treat as infrastructure errors, not missing webhooks
	return false, false, fmt.Errorf("network or transport failure during webhook inspection: %w", err)
}
```

### Testing
- Unit tests validating that `404` returns `exists=false, shouldRecreate=true`.
- Unit tests verifying `429`, `500`, and `401` return non-nil `apiErr` and `shouldRecreate=false`.
- Network timeout testing confirming transport errors do not trigger webhook re-creation flows.

Signed-off-by: Aditya Waghamare <adityawaghamare7620@gmail.com>

---
*Submitted by Aditya Waghamare*
💰 **Payout Address (Base L2 / EVM):** `0xb61dBcdBc3407F71EaCb64D4CBFAcf9FFfe2415C`
# Solution for Issue #1

## 🛠️ Proposed Solution (by Aditya Waghamare)

### Analysis
When checking repository webhook existence, blanket error handling in GitHub API client wrappers incorrectly treats any error (including HTTP 401, 403, 429, 5xx, or network timeouts) as a `404 Not Found` (missing webhook). This leads to duplicate webhook creation upon API recovery and masks critical rate-limit or auth failures.

### Fix
Update webhook check and reconciliation functions to strictly isolate `404 Not Found` status responses or empty `200 OK` responses as valid indicators of a missing webhook. All other errors (rate limits, server errors, authentication errors, network timeouts) are bubbled up immediately without triggering recreation/reconciliation loops.

### Implementation
```go
package github

import (
	"errors"
	"fmt"
	"net/http"
)

// APIError represents a structured GitHub API error response.
type APIError struct {
	StatusCode int
	Message    string
	Err        error
}

func (e *APIError) Error() string {
	return fmt.Sprintf("github api error [status %d]: %s (err: %v)", e.StatusCode, e.Message, e.Err)
}

// IsNotFound checks if the given error represents a GitHub 404 Not Found.
func IsNotFound(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == http.StatusNotFound
	}
	return false
}

// WebhookExists checks for webhook existence, strictly distinguishing 404 from other failures.
// Returns (exists, err). If err != nil, reconciliation must abort and bubble up the error.
func (c *Client) WebhookExists(owner, repo string, hookID int64) (bool, error) {
	resp, err := c.getWebhookAPI(owner, repo, hookID)
	if err != nil {
		if IsNotFound(err) {
			// Webhook is legitimately missing
			return false, nil
		}
		// Transient, auth, or server errors must be bubbled up, not treated as missing
		return false, fmt.Errorf("failed to verify webhook %d for %s/%s due to upstream API error: %w", hookID, owner, repo, err)
	}

	if resp == nil {
		return false, nil
	}

	return true, nil
}
```

### Testing
- **Unit Test Case 404**: Mock server returns `404 Not Found` → `WebhookExists` returns `(false, nil)`.
- **Unit Test Case 429 / 5xx**: Mock server returns `429 Too Many Requests` or `500 Internal Server Error` → `WebhookExists` returns `(false, err)` with non-nil error.
- **Unit Test Case 200**: Mock server returns `200 OK` → `WebhookExists` returns `(true, nil)`.


---
*Submitted by Aditya Waghamare*
💰 **Payout Address (Base L2 / EVM):** `0xb61dBcdBc3407F71EaCb64D4CBFAcf9FFfe2415C`
# Solution for Issue #1

## 🛠️ Proposed Solution

### Analysis
Any non‑404 GitHub API failure during webhook checks is currently treated as a missing webhook.  This leads to false positives, duplicate creations, and misleading alerts.

### Fix
Add a dedicated `WebhookClient` that centralises error handling for webhook discovery.  The client:
* Differentiates `404` from all other HTTP errors.
* Returns a clear existence flag.
* Propagates transient errors for retry/back‑off.
* Logs authentication failures without exposing the token.

Existing callers can swap their direct HTTP logic for this wrapper.

### Implementation
```go
// internal/github/webhook_client.go
package github

import (
    "context"
    "errors"
    "fmt"
    "log"

    "github.com/google/go-github/v61/github"
    "golang.org/x/oauth2"
)

// WebhookClient wraps the go‑github Client for webhook operations.
// It centralises error handling for webhook discovery.
// Signed-off-by: Contributor <contributor@users.noreply.github.com>

type WebhookClient struct {
    client *github.Client
    token  string
}

// NewWebhookClient creates a new WebhookClient using the provided OAuth token.
func NewWebhookClient(ctx context.Context, token string) *WebhookClient {
    ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
    tc := oauth2.NewClient(ctx, ts)
    return &WebhookClient{client: github.NewClient(tc), token: token}
}

// FindWebhook checks if a repository has a webhook matching the supplied config.
// It returns the found hook (or nil), a boolean indicating existence, and an error
// if the lookup failed for reasons other than "not found".
func (c *WebhookClient) FindWebhook(ctx context.Context, owner, repo string, cfg *github.Hook) (*github.Hook, bool, error) {
    hooks, _, err := c.client.Repositories.ListHooks(ctx, owner, repo, &github.ListOptions{})
    if err != nil {
        if resp := githubErrorStatus(err); resp != nil {
            switch resp.StatusCode {
            case 401, 403:
                msg := fmt.Sprintf("github auth failed: %s", resp.Status)
                log.Printf("%s | token: %s", msg, tokenRedacted(c.token))
                return nil, false, errors.New(msg)
            case 404:
                // Repository not found – treat as missing webhook.
                return nil, false, nil
            default:
                // Transient failures – bubble up.
                return nil, false, fmt.Errorf("github api failure while inspecting webhook: %w", err)
            }
        }
        // Non‑HTTP errors (network, context cancellation, etc.)
        return nil, false, fmt.Errorf("github api failure while inspecting webhook: %w", err)
    }

    // Search the list for a matching configuration.
    for _, h := range hooks {
        if h == nil || h.Config == nil {
            continue
        }
        if h.Config["url"] == cfg.Config["url"] {
            return h, true, nil
        }
    }
    return nil, false, nil
}

// githubErrorStatus extracts the *github.Response from an error, if present.
func githubErrorStatus(err error) *github.Response {
    var apiErr *github.ErrorResponse
    if errors.As(err, &apiErr) {
        return apiErr.Response
    }
    return nil
}

func tokenRedacted(token string) string {
    // Return a placeholder for logging.
    return "<token>"
}
```

#### Integration
Replace legacy webhook lookup logic with:
```go
wc := github.NewWebhookClient(ctx, token)
hook, exists, err := wc.FindWebhook(ctx, owner, repo, cfg)
```
Handle `exists == false` as a genuine missing webhook.

### Testing
* **Unit tests** – mock the GitHub client with a custom `RoundTripper` that returns 404, 429, 500, 401 responses. Verify `FindWebhook` returns correct existence flag and bubbles non‑404 errors.
* **Integration test** – create a webhook in a test repo, then call `FindWebhook` to confirm detection. Temporarily simulate a 404 by altering the hook ID in the request; confirm that no duplicate is created.
* **Observability** – ensure logs contain the placeholder token and the status code for auth failures.

---

💰 **Wallet Address:** `0xEA3b60D7076B62749fb3C65b167bf79326e8A504`
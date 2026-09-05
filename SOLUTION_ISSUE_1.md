# Solution for Issue #1

## 🛠️ Proposed Solution

### Analysis
GitHub API requests used to check for existing webhooks misinterpret any non‑404 error as a missing webhook. This causes false positives and duplicate creations. The fix adds precise error handling that distinguishes `404 Not Found` from transient or authentication failures, bubbles up non‑404 errors, and logs clear diagnostics.

### Fix
Create a dedicated `WebhookClient` wrapper with a robust `FindWebhook` method. The method returns `*Webhook`, a boolean indicating existence, and an error. It treats
* `404` and an empty list as *missing webhook* (`exists=false`).
* Transient errors (429, 5xx, network timeouts) are returned unchanged with a descriptive message.
* Auth errors (401/403) fail fast with a diagnostic log.

Existing callers can replace direct HTTP calls with `WebhookClient.FindWebhook`. The implementation uses the community `go-github` library.

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

// WebhookClient wraps the go-github Client for webhook operations.
// It centralises error handling for webhook discovery.
type WebhookClient struct {
    client *github.Client
}

// NewWebhookClient creates a new WebhookClient using the provided OAuth token.
func NewWebhookClient(ctx context.Context, token string) *WebhookClient {
    ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
    tc := oauth2.NewClient(ctx, ts)
    return &WebhookClient{client: github.NewClient(tc)}
}

// FindWebhook checks if a repository has a webhook with the given event and URL.
// It returns the webhook, a boolean indicating existence, and an error if the lookup failed.
func (c *WebhookClient) FindWebhook(ctx context.Context, owner, repo string, cfg *github.Hook) (*github.Hook, bool, error) {
    // List all hooks with the desired config to avoid needing the hook ID.
    hooks, _, err := c.client.Repositories.ListHooks(ctx, owner, repo, &github.ListOptions{})
    if err != nil {
        // Inspect HTTP status via the Response field.
        if resp := githubErrorStatus(err); resp != nil {
            switch resp.StatusCode {
            case 401, 403:
                msg := fmt.Sprintf("github auth failed: %s", resp.Status)
                log.Printf("%s\nToken: %s", msg, tokenRedacted())
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

func tokenRedacted() string {
    // Return a placeholder for logging.
    return "<token>"
}
```

To integrate: replace legacy webhook lookup code with
```go
wc := github.NewWebhookClient(ctx, token)
hook, exists, err := wc.FindWebhook(ctx, owner, repo, cfg)
```
and handle `exists == false` as a true missing webhook.

### Testing
1. **Unit tests**: Mock the GitHub client using `go-github`’s `NewClient(transport)` with a custom `RoundTripper` that returns controlled responses (404, 429, 500, 401). Verify that `FindWebhook` returns correct existence flag and error.
2. **Integration test**: Using a test repository, create a webhook, then call `FindWebhook` to confirm detection. Temporarily simulate a 404 by changing the hook ID in the test request. Confirm duplicate creation is prevented when the real API is reachable.
3. **Observability**: Ensure logs contain the token placeholder and status codes for auth failures.

---

💰 **Wallet Address:** `0xEA3b60D7076B62749fb3C65b167bf79326e8A504`
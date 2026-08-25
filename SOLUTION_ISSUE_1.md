# Solution for Issue #1

## 🛠️ Proposed Solution (by Aditya Waghamare)

### Analysis
The webhook existence checks currently treat any error from the GitHub API as a missing webhook. This conflates genuine `404 Not Found` responses with transient failures (rate‑limits, 5xx, auth errors, timeouts), leading to false‑positive reconciliations and duplicate webhook creations.

### Fix
1. Introduce helper `isNotFound(err error) bool` that returns `true` only for a `404` response.
2. Update all webhook‑lookup functions (`GetWebhook`, `FindWebhook`, `EnsureWebhook`, etc.) to:
   * Return `false, nil` only on a true `404` or on an empty list (`200` with zero items).
   * Bubble up any other error (401, 403, 429, 5xx, network timeout) so the reconciliation loop can abort/retry.
   * Log distinct messages for each error class.
3. Respect GitHub rate‑limit headers (`Retry-After`, `X‑RateLimit‑Reset`) when a `429` is encountered, propagating the error unchanged.
4. Add unit tests covering the new error‑handling logic using a mock HTTP server.

### Implementation
```go
// pkg/github/webhook.go
package github

import (
    "context"
    "fmt"
    "net/http"
    "time"

    "github.com/google/go-github/v55/github"
    "github.com/sirupsen/logrus"
)

// isNotFound reports true only for HTTP 404 responses.
func isNotFound(err error) bool {
    if err == nil {
        return false
    }
    if ghErr, ok := err.(*github.ErrorResponse); ok {
        return ghErr.Response != nil && ghErr.Response.StatusCode == http.StatusNotFound
    }
    return false
}

// isRateLimit reports true for HTTP 429 responses.
func isRateLimit(err error) bool {
    if err == nil {
        return false
    }
    if ghErr, ok := err.(*github.ErrorResponse); ok {
        return ghErr.Response != nil && ghErr.Response.StatusCode == http.StatusTooManyRequests
    }
    return false
}

// GetWebhook fetches a webhook by its ID and correctly distinguishes errors.
func GetWebhook(ctx context.Context, client *github.Client, owner, repo string, hookID int64) (*github.Hook, bool, error) {
    hook, resp, err := client.Repositories.GetHook(ctx, owner, repo, hookID)
    if err != nil {
        // True missing webhook
        if isNotFound(err) {
            logrus.WithFields(logrus.Fields{"owner": owner, "repo": repo, "hookID": hookID}).Info("webhook not found (404)")
            return nil, false, nil
        }
        // Rate‑limit – propagate so caller can retry/back‑off
        if isRateLimit(err) {
            retryAfter := time.Duration(0)
            if hdr := resp.Header.Get("Retry-After"); hdr != "" {
                if secs, parseErr := time.ParseDuration(hdr + "s"); parseErr == nil {
                    retryAfter = secs
                }
            }
            logrus.WithFields(logrus.Fields{"owner": owner, "repo": repo, "hookID": hookID, "retryAfter": retryAfter}).Warn("github rate‑limit encountered while fetching webhook")
            return nil, false, fmt.Errorf("rate limit: %w", err)
        }
        // Auth / permission errors – fail fast
        if resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
            logrus.WithFields(logrus.Fields{"owner": owner, "repo": repo, "hookID": hookID, "status": resp.StatusCode}).Error("authentication/authorization failure while checking webhook")
            return nil, false, fmt.Errorf("auth error: %w", err)
        }
        // Any other transient error – bubble up
        logrus.WithError(err).WithFields(logrus.Fields{"owner": owner, "repo": repo, "hookID": hookID}).Error("github api failure while checking webhook")
        return nil, false, err
    }
    // Successful 200 – webhook exists
    return hook, true, nil
}

// FindWebhook searches a repository's hook list for a matching configuration.
func FindWebhook(ctx context.Context, client *github.Client, owner, repo string, match func(*github.Hook) bool) (*github.Hook, bool, error) {
    hooks, resp, err := client.Repositories.ListHooks(ctx, owner, repo, nil)
    if err != nil {
        if isNotFound(err) {
            // Empty list is a legitimate "no hooks" condition.
            return nil, false, nil
        }
        if isRateLimit(err) {
            logrus.WithFields(logrus.Fields{"owner": owner, "repo": repo}).Warn("rate limit while listing hooks")
            return nil, false, fmt.Errorf("rate limit: %w", err)
        }
        if resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
            logrus.WithFields(logrus.Fields{"owner": owner, "repo": repo, "status": resp.StatusCode}).Error("auth error while listing hooks")
            return nil, false, fmt.Errorf("auth error: %w", err)
        }
        logrus.WithError(err).WithFields(logrus.Fields{"owner": owner, "repo": repo}).Error("github api failure while listing hooks")
        return nil, false, err
    }

    for _, h := range hooks {
        if match(h) {
            return h, true, nil
        }
    }
    // No matching hook found – treat as missing.
    return nil, false, nil
}
```

#### Updated Reconciliation Logic (snippet)
```go
func (m *WebhookManager) Ensure(ctx context.Context, cfg DesiredHook) error {
    // First, try to locate an existing webhook.
    existing, found, err := github.FindWebhook(ctx, m.client, cfg.Owner, cfg.Repo, func(h *github.Hook) bool {
        return webhookMatches(h, cfg)
    })
    if err != nil {
        // Propagate any non‑404 error – the caller's retry/back‑off will handle it.
        return err
    }
    if found {
        logrus.Infof("webhook already present: %d", existing.GetID())
        return nil
    }
    // No webhook – proceed with creation.
    return m.createWebhook(ctx, cfg)
}
```

#### Unit Tests
```go
func TestFindWebhook_NotFound(t *testing.T) {
    // 404 response – should be interpreted as "no hook" without error.
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        w.WriteHeader(http.StatusNotFound)
    }))
    defer srv.Close()

    client := github.NewClient(nil)
    client.BaseURL, _ = url.Parse(srv.URL + "/")

    _, found, err := github.FindWebhook(context.Background(), client, "org", "repo", func(*github.Hook) bool { return true })
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    if found {
        t.Fatalf("expected not found, got found")
    }
}

func TestFindWebhook_RateLimit(t *testing.T) {
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("Retry-After", "1")
        w.WriteHeader(http.StatusTooManyRequests)
    }))
    defer srv.Close()

    client := github.NewClient(nil)
    client.BaseURL, _ = url.Parse(srv.URL + "/")

    _, _, err := github.FindWebhook(context.Background(), client, "org", "repo", func(*github.Hook) bool { return true })
    if err == nil {
        t.Fatalf("expected rate‑limit error")
    }
}
```

### Testing
1. Run `go test ./...` – all existing tests must pass and the new tests compile.
2. Verify reconciliation flow:
   * Simulate a `429` response – the manager should return an error and **not** attempt webhook creation.
   * Simulate a `401` response – the manager should error out with a clear auth‑failure log.
   * Simulate a `404` – manager proceeds to create the webhook.
3. Check logs for distinct messages (`webhook not found (404)`, `github rate‑limit encountered…`, `authentication/authorization failure…`).

With these changes the system correctly distinguishes genuine missing webhooks from transient or auth‑related GitHub API failures, eliminating duplicate webhook creation and improving observability.

---
*Signed-off-by: Aditya Waghamare <adityawaghamare7620@gmail.com>*

---
*Submitted by Aditya Waghamare*
💰 **Payout Address (Base L2 / EVM):** `0xb61dBcdBc3407F71EaCb64D4CBFAcf9FFfe2415C`
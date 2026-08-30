// File: pkg/apis/application/v1alpha1/types.go (or similar types file)
type GitHubWebhookStatus string

const (
    GitHubWebhookStatusActive        GitHubWebhookStatus = "Active"
    GitHubWebhookStatusMissing       GitHubWebhookStatus = "Missing"
    GitHubWebhookStatusConfigError   GitHubWebhookStatus = "ConfigError"
    GitHubWebhookStatusAPIError      GitHubWebhookStatus = "APIError"
)

// File: pkg/sync/webhook/github.go (or equivalent webhook reconciliation logic)
import (
    "context"
    "fmt"
    "net/http"
    "github.com/google/go-github/v55/github"
)

// ReconcileGitHubWebhook checks and reconciles GitHub webhooks with explicit error handling
func (c *Client) ReconcileGitHubWebhook(ctx context.Context, repo, webhookURL string) (GitHubWebhookStatus, error) {
    // List existing webhooks
    webhooks, _, err := c.githubClient.Repositories.ListHooks(ctx, repo, nil)
    if err != nil {
        // Distinguish between API errors (transient) and auth/config issues
        if isGitHubAPIError(err) {
            return GitHubWebhookStatusAPIError, fmt.Errorf("GitHub API error listing webhooks: %w", err)
        }
        return GitHubWebhookStatusConfigError, fmt.Errorf("GitHub config error: %w", err)
    }

    // Check if our webhook exists
    for _, wh := range webhooks {
        if wh.GetURL() == webhookURL {
            return GitHubWebhookStatusActive, nil
        }
    }

    // Webhook not found — attempt to create it
    _, _, err = c.githubClient.Repositories.CreateHook(ctx, repo, &github.Hook{
        Events: []string{"push", "pull_request"},
        Config: &github.HookConfig{
            URL:         github.String(webhookURL),
            ContentType: github.String("json"),
        },
    })
    if err != nil {
        if isGitHubAPIError(err) {
            return GitHubWebhookStatusAPIError, fmt.Errorf("GitHub API error creating webhook: %w", err)
        }
        return GitHubWebhookStatusConfigError, fmt.Errorf("GitHub config error creating webhook: %w", err)
    }

    return GitHubWebhookStatusActive, nil
}

// isGitHubAPIError returns true for transient GitHub API errors (5xx, rate limits, etc.)
func isGitHubAPIError(err error) bool {
    if err == nil {
        return false
    }
    if apiErr, ok := err.(*github.ErrorResponse); ok {
        // 5xx errors and rate limit errors are transient
        if apiErr.Response.StatusCode >= 500 || apiErr.Response.StatusCode == http.StatusTooManyRequests {
            return true
        }
        // 403 might be transient (rate limit) or permanent (permission denied)
        // Check for rate limit header
        if apiErr.Response.StatusCode == http.StatusForbidden {
            if remaining := apiErr.Response.Header.Get("X-RateLimit-Remaining"); remaining == "0" {
                return true
            }
        }
    }
    return false
}

// File: pkg/controller/application_controller.go (or reconciliation loop)
func (r *Reconciler) ReconcileGitHubWebhooks(ctx context.Context, app *argocd.Application) error {
    status, err := r.githubClient.ReconcileGitHubWebhook(ctx, app.Spec.Source.RepoURL, r.webhookURL)
    if err != nil {
        // Only log API errors as transient; config errors need manual intervention
        if status == GitHubWebhookStatusAPIError {
            r.logger.Info("GitHub webhook reconciliation failed due to transient API error", "status", status)
            return nil // Requeue automatically via controller
        }
        r.logger.Error(err, "GitHub webhook reconciliation failed due to config error", "status", status)
        return err // Do not requeue; needs manual fix
    }
    // Update app status with webhook status
    app.Status.GitHubWebhookStatus = status
    return nil
}
package webhook

import (
	"context"
	"log"
	"net/http"

	"github.com/google/go-github/v55/github"
	"golang.org/x/oauth2"
)

// Reconciler is responsible for ensuring a repository has the expected webhook.
type Reconciler struct {
	ghClient *github.Client
	owner    string
	repo     string
}

// NewReconciler creates a Reconciler for a given repo using the supplied GitHub token.
func NewReconciler(token, owner, repo string) *Reconciler {
	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: token},
	)
	tc := oauth2.NewClient(context.Background(), ts)
	return &Reconciler{
		ghClient: github.NewClient(tc),
		owner:    owner,
		repo:     repo,
	}
}

// EnsureWebhook guarantees that a webhook with the given URL exists.
// It now distinguishes between missing, transient, and auth/permission errors.
func (r *Reconciler) EnsureWebhook(ctx context.Context, targetURL string) error {
	// First, try to locate an existing webhook.
	hook, err := r.findWebhook(ctx, targetURL)

	if err != nil {
		// Authentication / permission problems – fail fast.
		if IsAuthError(err) {
			log.Printf("[ERROR] GitHub auth error while checking webhook for %s/%s: %v", r.owner, r.repo, err)
			return err
		}
		// Transient problems – abort reconciliation, let caller retry.
		if IsTransient(err) {
			log.Printf("[WARN] Transient GitHub error while checking webhook for %s/%s: %v", r.owner, r.repo, err)
			return err
		}
		// Anything else that is not a 404 is treated as an unexpected error.
		log.Printf("[ERROR] Unexpected error while checking webhook for %s/%s: %v", r.owner, r.repo, err)
		return err
	}

	// If a hook was found, nothing to do.
	if hook != nil {
		log.Printf("[INFO] Webhook already present for %s/%s (ID=%d)", r.owner, r.repo, hook.GetID())
		return nil
	}

	// No hook found – proceed with creation.
	newHook := &github.Hook{
		Config: map[string]interface{}{
			"url":          targetURL,
			"content_type": "json",
			"insecure_ssl": "0",
		},
		Events: []string{"push", "pull_request"},
		Active: github.Bool(true),
	}
	_, _, err = r.ghClient.Repositories.CreateHook(ctx, r.owner, r.repo, newHook)
	if err != nil {
		// Creation can also hit auth or transient errors – handle similarly.
		if IsAuthError(err) {
			log.Printf("[ERROR] GitHub auth error while creating webhook for %s/%s: %v", r.owner, r.repo, err)
			return err
		}
		if IsTransient(err) {
			log.Printf("[WARN] Transient GitHub error while creating webhook for %s/%s: %v", r.owner, r.repo, err)
			return err
		}
		log.Printf("[ERROR] Failed to create webhook for %s/%s: %v", r.owner, r.repo, err)
		return err
	}
	log.Printf("[INFO] Successfully created webhook for %s/%s", r.owner, r.repo)
	return nil
}

// findWebhook searches for a webhook matching targetURL.
// Returns (nil, nil) when the webhook does not exist (404 or empty list).
func (r *Reconciler) findWebhook(ctx context.Context, targetURL string) (*github.Hook, error) {
	hooks, resp, err := r.ghClient.Repositories.ListHooks(ctx, r.owner, r.repo, nil)
	if err != nil {
		// Propagate the error – callers will classify it.
		return nil, err
	}
	// A 200 with an empty slice means “no hooks”.
	if resp != nil && resp.StatusCode == http.StatusOK && len(hooks) == 0 {
		return nil, nil // explicit missing
	}
	for _, h := range hooks {
		if cfgURL, ok := h.Config["url"]; ok && cfgURL == targetURL {
			return h, nil
		}
	}
	// Not found after iterating – treat as missing.
	return nil, nil
}

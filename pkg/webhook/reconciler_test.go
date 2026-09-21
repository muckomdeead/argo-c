package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestReconciler_NotFound_CreatesWebhook(t *testing.T) {
	var createCalled int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
			return
		}
		if r.Method == http.MethodPost {
			atomic.AddInt32(&createCalled, 1)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":123,"active":true,"config":{"url":"https://example.com/hook"}}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	client := NewHTTPGitHubClient(server.URL, "test-token", server.Client())
	reconciler := NewReconciler(client, nil)

	desired := &Webhook{
		Active: true,
		Events: []string{"push"},
		Config: WebhookConfig{URL: "https://example.com/hook"},
	}

	hook, err := reconciler.Reconcile(context.Background(), "owner", "repo", desired)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hook == nil || hook.ID != 123 {
		t.Fatalf("expected hook ID 123, got: %+v", hook)
	}
	if atomic.LoadInt32(&createCalled) != 1 {
		t.Fatalf("expected CreateWebhook to be called once, got %d", createCalled)
	}
}

func TestReconciler_ServerError_HaltsWithoutCreation(t *testing.T) {
	testCases := []struct {
		name       string
		statusCode int
	}{
		{"InternalServerError", http.StatusInternalServerError},
		{"BadGateway", http.StatusBadGateway},
		{"ServiceUnavailable", http.StatusServiceUnavailable},
		{"GatewayTimeout", http.StatusGatewayTimeout},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var createCalled int32

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					w.WriteHeader(tc.statusCode)
					_, _ = w.Write([]byte(`{"message":"GitHub error"}`))
					return
				}
				if r.Method == http.MethodPost {
					atomic.AddInt32(&createCalled, 1)
					w.WriteHeader(http.StatusCreated)
					return
				}
			}))
			defer server.Close()

			client := NewHTTPGitHubClient(server.URL, "test-token", server.Client())
			reconciler := NewReconciler(client, nil)

		desired := &Webhook{
			Active: true,
			Config: WebhookConfig{URL: "https://example.com/hook"},
		}

		_, err := reconciler.Reconcile(context.Background(), "owner", "repo", desired)
		if err == nil {
			t.Fatalf("expected error for status %d, got nil", tc.statusCode)
		}
		if !errors.Is(err, ErrServerError) {
			t.Fatalf("expected ErrServerError, got: %v", err)
		}
		if atomic.LoadInt32(&createCalled) != 0 {
			t.Fatalf("CreateWebhook should NOT be called on server error %d", tc.statusCode)
		}
		})
	}
}

func TestReconciler_RateLimited_HaltsWithoutCreation(t *testing.T) {
	var createCalled int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"message":"API rate limit exceeded"}`))
			return
		}
		if r.Method == http.MethodPost {
			atomic.AddInt32(&createCalled, 1)
			w.WriteHeader(http.StatusCreated)
			return
		}
	}))
	defer server.Close()

	client := NewHTTPGitHubClient(server.URL, "test-token", server.Client())
	reconciler := NewReconciler(client, nil)

	desired := &Webhook{
		Active: true,
		Config: WebhookConfig{URL: "https://example.com/hook"},
	}

	_, err := reconciler.Reconcile(context.Background(), "owner", "repo", desired)
	if err == nil {
		t.Fatalf("expected rate limit error, got nil")
	}
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got: %v", err)
	}
	if atomic.LoadInt32(&createCalled) != 0 {
		t.Fatalf("CreateWebhook should NOT be called on 429 Rate Limit")
	}
}

func TestReconciler_AuthErrors_HaltsWithoutCreation(t *testing.T) {
	testCases := []struct {
		name        string
		statusCode  int
		expectedErr error
	}{
		{"Unauthorized", http.StatusUnauthorized, ErrUnauthorized},
		{"Forbidden", http.StatusForbidden, ErrForbidden},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var createCalled int32

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					w.WriteHeader(tc.statusCode)
					_, _ = w.Write([]byte(`{"message":"Auth error"}`))
					return
				}
				if r.Method == http.MethodPost {
					atomic.AddInt32(&createCalled, 1)
					w.WriteHeader(http.StatusCreated)
					return
				}
			}))
			defer server.Close()

			client := NewHTTPGitHubClient(server.URL, "invalid-token", server.Client())
			reconciler := NewReconciler(client, nil)

		desired := &Webhook{
			Active: true,
			Config: WebhookConfig{URL: "https://example.com/hook"},
		}

		_, err := reconciler.Reconcile(context.Background(), "owner", "repo", desired)
		if err == nil {
			t.Fatalf("expected auth error for %s, got nil", tc.name)
		}
		if !errors.Is(err, tc.expectedErr) {
			t.Fatalf("expected %v, got: %v", tc.expectedErr, err)
		}
		if atomic.LoadInt32(&createCalled) != 0 {
			t.Fatalf("CreateWebhook should NOT be called on auth error %d", tc.statusCode)
		}
		})
	}
}

func TestReconciler_Timeout_HaltsWithoutCreation(t *testing.T) {
	var createCalled int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			time.Sleep(100 * time.Millisecond)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`[]`))
			return
		}
		if r.Method == http.MethodPost {
			atomic.AddInt32(&createCalled, 1)
			w.WriteHeader(http.StatusCreated)
			return
		}
	}))
	defer server.Close()

	client := NewHTTPGitHubClient(server.URL, "test-token", server.Client())
	reconciler := NewReconciler(client, nil)

	desired := &Webhook{
		Active: true,
		Config: WebhookConfig{URL: "https://example.com/hook"},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err := reconciler.Reconcile(ctx, "owner", "repo", desired)
	if err == nil {
		t.Fatalf("expected timeout error, got nil")
	}
	if atomic.LoadInt32(&createCalled) != 0 {
		t.Fatalf("CreateWebhook should NOT be called on timeout error")
	}
}

func TestReconciler_ExistingWebhook_Idempotent(t *testing.T) {
	var createCalled int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`[{"id":456,"active":true,"config":{"url":"https://example.com/hook"}}]`))
			return
		}
		if r.Method == http.MethodPost {
			atomic.AddInt32(&createCalled, 1)
			w.WriteHeader(http.StatusCreated)
			return
		}
	}))
	defer server.Close()

	client := NewHTTPGitHubClient(server.URL, "test-token", server.Client())
	reconciler := NewReconciler(client, nil)

	desired := &Webhook{
		Active: true,
		Config: WebhookConfig{URL: "https://example.com/hook"},
	}

	hook, err := reconciler.Reconcile(context.Background(), "owner", "repo", desired)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hook == nil || hook.ID != 456 {
		t.Fatalf("expected existing hook ID 456, got: %+v", hook)
	}
	if atomic.LoadInt32(&createCalled) != 0 {
		t.Fatalf("CreateWebhook should NOT be called when webhook already exists")
	}
}


// TestReconciler_Pagination_FindsMatchOnLaterPage verifies that ListWebhooks
// follows the Link header across pages. When the matching webhook lives on
// page 2, Reconcile must find it and NOT create a duplicate.
func TestReconciler_Pagination_FindsMatchOnLaterPage(t *testing.T) {
	var createCalled int32
	var page1Seen, page2Seen int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "" {
			atomic.AddInt32(&page1Seen, 1)
			hooks := make([]map[string]interface{}, 30)
			for i := range hooks {
				hooks[i] = map[string]interface{}{
					"id":     int64(i + 1),
					"active": true,
					"config": map[string]string{"url": "https://example.com/different/" + string(rune('a'+i))},
				}
			}
			w.Header().Set("Link", "<"+server.URL+"/repos/owner/repo/hooks?page=2>; rel=\"next\"")
			_ = json.NewEncoder(w).Encode(hooks)
			return
		}
		if r.URL.Query().Get("page") == "2" {
			atomic.AddInt32(&page2Seen, 1)
			hooks := []map[string]interface{}{
				{"id": int64(999), "active": true, "config": map[string]string{"url": "https://example.com/hook"}},
			}
			_ = json.NewEncoder(w).Encode(hooks)
			return
		}
		if r.Method == http.MethodPost {
			atomic.AddInt32(&createCalled, 1)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":123,"active":true,"config":{"url":"https://example.com/hook"}}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	client := NewHTTPGitHubClient(server.URL, "test-token", server.Client())
	reconciler := NewReconciler(client, nil)
	desired := &Webhook{Active: true, Config: WebhookConfig{URL: "https://example.com/hook"}}

	hook, err := reconciler.Reconcile(context.Background(), "owner", "repo", desired)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hook == nil || hook.ID != 999 {
		t.Fatalf("expected existing hook ID 999, got: %+v", hook)
	}
	if atomic.LoadInt32(&page1Seen) != 1 {
		t.Fatalf("expected page 1 fetched, got %d", page1Seen)
	}
	if atomic.LoadInt32(&page2Seen) != 1 {
		t.Fatalf("expected page 2 fetched, got %d", page2Seen)
	}
	if atomic.LoadInt32(&createCalled) != 0 {
		t.Fatalf("CreateWebhook must NOT be called when match found on page 2, got %d", createCalled)
	}
}

// TestReconciler_Pagination_Page2Failure_Aborts verifies that if a later page
// fails, Reconcile aborts without creating a duplicate webhook.
func TestReconciler_Pagination_Page2Failure_Aborts(t *testing.T) {
	var createCalled int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "" {
			hooks := make([]map[string]interface{}, 30)
			for i := range hooks {
				hooks[i] = map[string]interface{}{
					"id":     int64(i + 1),
					"active": true,
					"config": map[string]string{"url": "https://example.com/different/" + string(rune('a'+i))},
				}
			}
			w.Header().Set("Link", "<"+server.URL+"/repos/owner/repo/hooks?page=2>; rel=\"next\"")
			_ = json.NewEncoder(w).Encode(hooks)
			return
		}
		if r.URL.Query().Get("page") == "2" {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"server error"}`))
			return
		}
		if r.Method == http.MethodPost {
			atomic.AddInt32(&createCalled, 1)
			w.WriteHeader(http.StatusCreated)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	client := NewHTTPGitHubClient(server.URL, "test-token", server.Client())
	reconciler := NewReconciler(client, nil)
	desired := &Webhook{Active: true, Config: WebhookConfig{URL: "https://example.com/hook"}}

	_, err := reconciler.Reconcile(context.Background(), "owner", "repo", desired)
	if err == nil {
		t.Fatalf("expected error when page 2 fails, got nil")
	}
	if atomic.LoadInt32(&createCalled) != 0 {
		t.Fatalf("CreateWebhook must NOT be called when a later page fails, got %d", createCalled)
	}
}

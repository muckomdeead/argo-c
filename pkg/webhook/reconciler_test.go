package webhook

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/go-github/v55/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// mockGitHubClient implements the subset of github.Client used by Reconciler.
type mockGitHubClient struct {
	mock.Mock
}

func (m *mockGitHubClient) Repositories() *github.RepositoriesService {
	// The real client returns a *RepositoriesService; we embed the mock in it.
	return &github.RepositoriesService{Client: m}
}

// ListHooks mock.
func (m *mockGitHubClient) ListHooks(ctx context.Context, owner, repo string, opts *github.ListOptions) ([]*github.Hook, *github.Response, error) {
	args := m.Called(ctx, owner, repo, opts)
	return args.Get(0).([]*github.Hook), args.Get(1).(*github.Response), args.Error(2)
}

// CreateHook mock.
func (m *mockGitHubClient) CreateHook(ctx context.Context, owner, repo string, hook *github.Hook) (*github.Hook, *github.Response, error) {
	args := m.Called(ctx, owner, repo, hook)
	return args.Get(0).(*github.Hook), args.Get(1).(*github.Response), args.Error(2)
}

// Helper to build a *github.Response with a given status code.
func responseWithStatus(code int) *github.Response {
	return &github.Response{Response: &http.Response{StatusCode: code}}
}

func TestEnsureWebhook_MissingWebhook_CreatesIt(t *testing.T) {
	mockClient := new(mockGitHubClient)

	// ListHooks returns empty slice (no webhook present)
	mockClient.On("ListHooks", mock.Anything, "owner", "repo", (*github.ListOptions)(nil)).
		Return([]*github.Hook{}, responseWithStatus(200), nil)

	// CreateHook succeeds
	createdHook := &github.Hook{ID: github.Int64(123)}
	mockClient.On("CreateHook", mock.Anything, "owner", "repo", mock.Anything).
		Return(createdHook, responseWithStatus(201), nil)

	r := &Reconciler{
		ghClient: &github.Client{Repositories: mockClient},
		owner:    "owner",
		repo:     "repo",
	}
	err := r.EnsureWebhook(context.Background(), "https://example.com/hook")
	assert.NoError(t, err)
	mockClient.AssertExpectations(t)
}

func TestEnsureWebhook_404_NotFound_NoCreation(t *testing.T) {
	mockClient := new(mockGitHubClient)

	// Simulate 404 from ListHooks – treated as missing, but we still want to create.
	// In our implementation ListHooks never returns 404 (GitHub returns 200 with empty list),
	// however we also test the error path for completeness.
	ghErr := &github.ErrorResponse{Response: &http.Response{StatusCode: http.StatusNotFound}}
	mockClient.On("ListHooks", mock.Anything, "owner", "repo", (*github.ListOptions)(nil)).
		Return(nil, nil, ghErr)

	r := &Reconciler{
		ghClient: &github.Client{Repositories: mockClient},
		owner:    "owner",
		repo:     "repo",
	}
	err := r.EnsureWebhook(context.Background(), "https://example.com/hook")
	// Expect the error to be bubbled up (no creation attempted)
	assert.Error(t, err)
	assert.True(t, IsNotFound(err))
	mockClient.AssertNotCalled(t, "CreateHook", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

func TestEnsureWebhook_TransientError_Aborts(t *testing.T) {
	mockClient := new(mockGitHubClient)

	// Simulate a 429 rate‑limit response.
	ghErr := &github.ErrorResponse{Response: &http.Response{StatusCode: http.StatusTooManyRequests}}
	mockClient.On("ListHooks", mock.Anything, "owner", "repo", (*github.ListOptions)(nil)).
		Return(nil, nil, ghErr)

	r := &Reconciler{
		ghClient: &github.Client{Repositories: mockClient},
		owner:    "owner",
		repo:     "repo",
	}
	err := r.EnsureWebhook(context.Background(), "https://example.com/hook")
	assert.Error(t, err)
	assert.True(t, IsTransient(err))
	mockClient.AssertNotCalled(t, "CreateHook", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

func TestEnsureWebhook_AuthError_FailsFast(t *testing.T) {
	mockClient := new(mockGitHubClient)

	// Simulate a 401 Unauthorized response.
	ghErr := &github.ErrorResponse{Response: &http.Response{StatusCode: http.StatusUnauthorized}}
	mockClient.On("ListHooks", mock.Anything, "owner", "repo", (*github.ListOptions)(nil)).
		Return(nil, nil, ghErr)

	r := &Reconciler{
		ghClient: &github.Client{Repositories: mockClient},
		owner:    "owner",
		repo:     "repo",
	}
	err := r.EnsureWebhook(context.Background(), "https://example.com/hook")
	assert.Error(t, err)
	assert.True(t, IsAuthError(err))
	mockClient.AssertNotCalled(t, "CreateHook", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

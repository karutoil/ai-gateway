package provider

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ai-gateway/internal/models"
	"ai-gateway/internal/oauth"
	_ "ai-gateway/internal/oauth/providers"
)

// testOAuthDef registers a controllable OAuth definition for refresh tests.
// Its DoRefresh hook counts grants and can simulate latency, so the
// singleflight behavior of EnsureFreshAccess is observable.
func testOAuthDef(t *testing.T, refreshes *int64, delay time.Duration, failFirst int64) oauth.Definition {
	t.Helper()
	oauth.Register(oauth.Definition{
		ID:       "test-oauth",
		Name:     "Test OAuth",
		AuthURL:  "https://example.com/auth",
		TokenURL: "https://example.com/token",
		ClientID: "cid",
	}, oauth.Hooks{
		DoRefresh: func(ctx context.Context, def *oauth.Definition, refreshToken string, httpClient *http.Client) (*oauth.Tokens, error) {
			n := atomic.AddInt64(refreshes, 1)
			if n <= failFirst {
				return nil, fmt.Errorf("simulated upstream refusal")
			}
			time.Sleep(delay)
			return &oauth.Tokens{
				Access:    fmt.Sprintf("fresh-access-%d", n),
				Refresh:   refreshToken, // no rotation in this scenario
				ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
			}, nil
		},
	})
	return oauth.Definition{ID: "test-oauth"}
}

func seedExpiredTokens(t *testing.T, s *Store) *models.Provider {
	t.Helper()
	p, err := s.CreateWithOrg("agy-race", models.ProviderAntigravity, "", "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	tok := &oauth.Tokens{
		Access:    "stale-access",
		Refresh:   "refresh-1",
		ExpiresAt: time.Now().Add(-time.Hour).UnixMilli(), // expired
	}
	if err := s.SetOAuthTokens(p.ID, "test-oauth", tok); err != nil {
		t.Fatalf("seed tokens: %v", err)
	}
	return p
}

// Concurrent EnsureFreshAccess calls on an expired token must produce exactly
// ONE refresh grant: parallel refreshes of the same refresh token make
// upstreams answer invalid_grant (the refresh-storm race this guards).
func TestEnsureFreshAccessSingleFlight(t *testing.T) {
	s := openTestStore(t)
	var refreshes int64
	testOAuthDef(t, &refreshes, 50*time.Millisecond, 0)
	p := seedExpiredTokens(t, s)

	const callers = 12
	var wg sync.WaitGroup
	results := make([]string, callers)
	errs := make([]error, callers)
	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			access, _, _, err := s.EnsureFreshAccess(context.Background(), p, nil)
			results[i], errs[i] = access, err
		}(i)
	}
	close(start)
	wg.Wait()

	if got := atomic.LoadInt64(&refreshes); got != 1 {
		t.Fatalf("expected exactly 1 refresh grant under concurrency, got %d", got)
	}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d failed: %v", i, err)
		}
		if results[i] != "fresh-access-1" {
			t.Fatalf("caller %d got %q, want the shared fresh token", i, results[i])
		}
	}
}

// A fresh (unexpired) stored token must not trigger any refresh.
func TestEnsureFreshAccessSkipsRefreshWhenFresh(t *testing.T) {
	s := openTestStore(t)
	var refreshes int64
	testOAuthDef(t, &refreshes, 0, 0)
	p, err := s.CreateWithOrg("agy-fresh", models.ProviderAntigravity, "", "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.SetOAuthTokens(p.ID, "test-oauth", &oauth.Tokens{
		Access: "still-valid", Refresh: "refresh-1",
		ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
	}); err != nil {
		t.Fatalf("set: %v", err)
	}
	access, _, _, err := s.EnsureFreshAccess(context.Background(), p, nil)
	if err != nil || access != "still-valid" {
		t.Fatalf("fresh token bypass failed: %q %v", access, err)
	}
	if atomic.LoadInt64(&refreshes) != 0 {
		t.Fatal("refresh must not fire for an unexpired token")
	}
}

// A failed refresh releases the lock; the next caller retries (a transient
// upstream error must not latch the provider as permanently unrefreshable).
func TestEnsureFreshAccessRetriesAfterFailure(t *testing.T) {
	s := openTestStore(t)
	var refreshes int64
	testOAuthDef(t, &refreshes, 0, 1) // first grant fails
	p := seedExpiredTokens(t, s)

	if _, _, _, err := s.EnsureFreshAccess(context.Background(), p, nil); err == nil {
		t.Fatal("first refresh should fail")
	}
	access, _, _, err := s.EnsureFreshAccess(context.Background(), p, nil)
	if err != nil {
		t.Fatalf("second refresh should succeed: %v", err)
	}
	if access != "fresh-access-2" {
		t.Fatalf("second refresh access = %q", access)
	}
}

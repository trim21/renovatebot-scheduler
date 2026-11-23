package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func tokenServer(t *testing.T, status int, calls *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		if got := r.PostFormValue("grant_type"); got != "refresh_token" {
			t.Errorf("unexpected grant_type %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status == http.StatusOK {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token":  "fresh-access",
				"refresh_token": "fresh-refresh",
				"expires_in":    3600,
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":             "invalid_grant",
			"error_description": "token was already used",
		})
	}))
}

func newTestAuth(t *testing.T, serverURL string) *Auth {
	t.Helper()
	return NewAuth(&Config{
		ForgejoURL:        serverURL,
		OAuthClientID:     "client",
		OAuthClientSecret: "secret",
	}, nil)
}

func TestTokenStoreRefreshesOnceForConcurrentCallers(t *testing.T) {
	var calls atomic.Int32
	srv := tokenServer(t, http.StatusOK, &calls)
	defer srv.Close()

	store := newTokenStore(newTestAuth(t, srv.URL), &oauthToken{
		AccessToken:  "stale",
		RefreshToken: "old-refresh",
		expiry:       time.Now().Add(-time.Minute),
	})

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			token, err := store.AccessToken(context.Background())
			if err != nil || token != "fresh-access" {
				t.Errorf("unexpected token %q (err %v)", token, err)
			}
		}()
	}
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Fatalf("expected exactly one refresh request, got %d", got)
	}

	token, err := store.AccessToken(context.Background())
	if err != nil || token != "fresh-access" {
		t.Fatalf("unexpected token %q (err %v)", token, err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("valid token should not be refreshed again, got %d requests", got)
	}
}

func TestTokenStoreRejectsSessionAfterFailedRefresh(t *testing.T) {
	var calls atomic.Int32
	srv := tokenServer(t, http.StatusBadRequest, &calls)
	defer srv.Close()

	store := newTokenStore(newTestAuth(t, srv.URL), &oauthToken{
		AccessToken:  "stale",
		RefreshToken: "old-refresh",
		expiry:       time.Now().Add(-time.Minute),
	})

	_, err := store.AccessToken(context.Background())
	if !errors.Is(err, errSessionExpired) {
		t.Fatalf("expected errSessionExpired, got %v", err)
	}
	if !strings.Contains(err.Error(), "invalid_grant") || !strings.Contains(err.Error(), "token was already used") {
		t.Fatalf("error should carry the OAuth error details, got %v", err)
	}

	_, err = store.AccessToken(context.Background())
	if !errors.Is(err, errSessionExpired) {
		t.Fatalf("rejected store should keep failing, got %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("rejected store should not retry, got %d requests", got)
	}
}

func TestTokenStoreWithoutRefreshToken(t *testing.T) {
	var calls atomic.Int32
	srv := tokenServer(t, http.StatusOK, &calls)
	defer srv.Close()

	store := newTokenStore(newTestAuth(t, srv.URL), &oauthToken{
		AccessToken: "stale",
		expiry:      time.Now().Add(-time.Minute),
	})

	if _, err := store.AccessToken(context.Background()); !errors.Is(err, errSessionExpired) {
		t.Fatalf("expected errSessionExpired, got %v", err)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("no refresh should be attempted without a refresh token, got %d", got)
	}
}

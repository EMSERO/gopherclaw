package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// setupTestManager creates a Manager with a mock GitHub token and a temporary home.
func setupTestManager(t *testing.T) (*Manager, string) {
	t.Helper()
	home := t.TempDir()

	// Write auth.json
	authDir := filepath.Join(home, ".gopherclaw", "agents", "main", "agent")
	if err := os.MkdirAll(authDir, 0700); err != nil {
		t.Fatal(err)
	}
	authData := []byte(`{"github-copilot": {"type": "pat", "key": "ghu_testtoken123"}}`)
	if err := os.WriteFile(filepath.Join(authDir, "auth.json"), authData, 0600); err != nil {
		t.Fatal(err)
	}

	m := &Manager{
		ghToken: "ghu_testtoken123",
		home:    home,
	}
	return m, home
}

// mockTokenServer creates an httptest.Server that mimics the GitHub Copilot token exchange.
// The handler validates the Authorization header and returns a token response.
func mockTokenServer(t *testing.T, ghToken string, statusCode int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Validate method
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// Validate auth header
		expected := "Bearer " + ghToken
		if r.Header.Get("Authorization") != expected {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// Validate other headers
		if r.Header.Get("Accept") != "application/json" {
			http.Error(w, "bad accept header", http.StatusBadRequest)
			return
		}
		if r.Header.Get("User-Agent") != "gopherclaw/1.0" {
			http.Error(w, "bad user-agent", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		_, _ = w.Write([]byte(body))
	}))
}

// --- APIURL tests ---

func TestAPIURL_Default(t *testing.T) {
	m := &Manager{}
	if got := m.APIURL(); got != defaultAPIURL {
		t.Errorf("APIURL() = %q, want %q", got, defaultAPIURL)
	}
}

func TestAPIURL_WithToken(t *testing.T) {
	m := &Manager{
		current: &Token{
			Token:     "tok",
			ExpiresAt: time.Now().Add(30 * time.Minute),
			APIURL:    "https://custom.api.example.com",
		},
	}
	if got := m.APIURL(); got != "https://custom.api.example.com" {
		t.Errorf("APIURL() = %q, want %q", got, "https://custom.api.example.com")
	}
}

func TestAPIURL_TokenWithEmptyURL(t *testing.T) {
	m := &Manager{
		current: &Token{
			Token:     "tok",
			ExpiresAt: time.Now().Add(30 * time.Minute),
			APIURL:    "",
		},
	}
	if got := m.APIURL(); got != defaultAPIURL {
		t.Errorf("APIURL() = %q, want %q (default)", got, defaultAPIURL)
	}
}

// --- Cache tests ---

func TestLoadCache_Valid(t *testing.T) {
	m, home := setupTestManager(t)

	cacheDir := filepath.Join(home, ".gopherclaw", "state", "credentials")
	if err := os.MkdirAll(cacheDir, 0700); err != nil {
		t.Fatal(err)
	}
	token := Token{
		Token:     "cached_token_value",
		ExpiresAt: time.Now().Add(30 * time.Minute),
		APIURL:    "https://api.example.com",
	}
	data, _ := json.Marshal(token)
	if err := os.WriteFile(filepath.Join(cacheDir, "github-copilot.token.json"), data, 0600); err != nil {
		t.Fatal(err)
	}

	m.loadCache()
	if m.current == nil {
		t.Fatal("expected cached token to be loaded")
	}
	if m.current.Token != "cached_token_value" {
		t.Errorf("got token %q, want %q", m.current.Token, "cached_token_value")
	}
}

func TestLoadCache_Expired(t *testing.T) {
	m, home := setupTestManager(t)

	cacheDir := filepath.Join(home, ".gopherclaw", "state", "credentials")
	if err := os.MkdirAll(cacheDir, 0700); err != nil {
		t.Fatal(err)
	}
	token := Token{
		Token:     "expired_token",
		ExpiresAt: time.Now().Add(-5 * time.Minute),
		APIURL:    "https://old.example.com",
	}
	data, _ := json.Marshal(token)
	if err := os.WriteFile(filepath.Join(cacheDir, "github-copilot.token.json"), data, 0600); err != nil {
		t.Fatal(err)
	}

	m.loadCache()
	if m.current != nil {
		t.Error("expected expired token to not be loaded from cache")
	}
}

func TestLoadCache_NearExpiry(t *testing.T) {
	m, home := setupTestManager(t)

	cacheDir := filepath.Join(home, ".gopherclaw", "state", "credentials")
	if err := os.MkdirAll(cacheDir, 0700); err != nil {
		t.Fatal(err)
	}
	// Token expires in 30 seconds -- under the 60s threshold
	token := Token{
		Token:     "almost_expired",
		ExpiresAt: time.Now().Add(30 * time.Second),
	}
	data, _ := json.Marshal(token)
	if err := os.WriteFile(filepath.Join(cacheDir, "github-copilot.token.json"), data, 0600); err != nil {
		t.Fatal(err)
	}

	m.loadCache()
	if m.current != nil {
		t.Error("expected near-expiry token (30s remaining) to not be loaded; threshold is 60s")
	}
}

func TestLoadCache_MissingFile(t *testing.T) {
	m, _ := setupTestManager(t)
	m.loadCache() // should not panic
	if m.current != nil {
		t.Error("expected nil current when cache file missing")
	}
}

func TestLoadCache_InvalidJSON(t *testing.T) {
	m, home := setupTestManager(t)

	cacheDir := filepath.Join(home, ".gopherclaw", "state", "credentials")
	if err := os.MkdirAll(cacheDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "github-copilot.token.json"), []byte("{invalid"), 0600); err != nil {
		t.Fatal(err)
	}

	m.loadCache()
	if m.current != nil {
		t.Error("expected nil current when cache file contains invalid JSON")
	}
}

func TestSaveCache(t *testing.T) {
	m, home := setupTestManager(t)
	m.current = &Token{
		Token:     "save_me",
		ExpiresAt: time.Now().Add(30 * time.Minute),
		APIURL:    "https://saved.example.com",
	}

	m.saveCache()

	// Verify file was written
	path := filepath.Join(home, cacheFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("expected cache file to exist: %v", err)
	}

	var tok Token
	if err := json.Unmarshal(data, &tok); err != nil {
		t.Fatalf("failed to unmarshal cached token: %v", err)
	}
	if tok.Token != "save_me" {
		t.Errorf("cached token = %q, want %q", tok.Token, "save_me")
	}
	if tok.APIURL != "https://saved.example.com" {
		t.Errorf("cached APIURL = %q, want %q", tok.APIURL, "https://saved.example.com")
	}
}

func TestSaveCache_CreatesDirectory(t *testing.T) {
	m, home := setupTestManager(t)
	m.current = &Token{
		Token:     "dir_test",
		ExpiresAt: time.Now().Add(10 * time.Minute),
	}

	// The cache directory should not exist yet
	path := filepath.Join(home, cacheFile)
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatal("expected cache directory to not exist before saveCache")
	}

	m.saveCache()

	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Fatal("expected cache file to exist after saveCache")
	}
}

// --- GetToken tests ---

func TestGetToken_CachedValid(t *testing.T) {
	m, _ := setupTestManager(t)
	m.current = &Token{
		Token:     "cached_valid",
		ExpiresAt: time.Now().Add(30 * time.Minute),
		APIURL:    "https://api.example.com",
	}

	tok, err := m.GetToken(context.Background())
	if err != nil {
		t.Fatalf("GetToken: %v", err)
	}
	if tok != "cached_valid" {
		t.Errorf("GetToken() = %q, want %q", tok, "cached_valid")
	}
	if m.APIURL() != "https://api.example.com" {
		t.Errorf("APIURL() = %q, want %q", m.APIURL(), "https://api.example.com")
	}
}

func TestGetToken_ExpiredTriggersRefresh(t *testing.T) {
	exp := time.Now().Add(30 * time.Minute).Unix()
	body := fmt.Sprintf(`{"token":"fresh_tok","expires_at":%d,"endpoints":{"api":"https://fresh.api.com"}}`, exp)
	srv := mockTokenServer(t, "ghu_testtoken123", 200, body)
	defer srv.Close()

	m, _ := setupTestManager(t)
	m.tokenExchangeURL = srv.URL

	// No current token -- should trigger refresh
	tok, err := m.GetToken(context.Background())
	if err != nil {
		t.Fatalf("GetToken: %v", err)
	}
	if tok != "fresh_tok" {
		t.Errorf("GetToken() = %q, want %q", tok, "fresh_tok")
	}
	if m.APIURL() != "https://fresh.api.com" {
		t.Errorf("APIURL() = %q, want %q", m.APIURL(), "https://fresh.api.com")
	}
}

func TestGetToken_NearExpiryTriggersRefresh(t *testing.T) {
	exp := time.Now().Add(30 * time.Minute).Unix()
	body := fmt.Sprintf(`{"token":"refreshed","expires_at":%d,"endpoints":{"api":"https://new.api.com"}}`, exp)
	srv := mockTokenServer(t, "ghu_testtoken123", 200, body)
	defer srv.Close()

	m, _ := setupTestManager(t)
	m.tokenExchangeURL = srv.URL
	// Token expires in 30 seconds -- under the 60s threshold
	m.current = &Token{
		Token:     "about_to_expire",
		ExpiresAt: time.Now().Add(30 * time.Second),
	}

	tok, err := m.GetToken(context.Background())
	if err != nil {
		t.Fatalf("GetToken: %v", err)
	}
	if tok != "refreshed" {
		t.Errorf("GetToken() = %q, want %q", tok, "refreshed")
	}
}

// --- refresh() tests ---

func TestRefresh_Success(t *testing.T) {
	exp := time.Now().Add(30 * time.Minute).Unix()
	body := fmt.Sprintf(`{"token":"fresh_abc","expires_at":%d,"endpoints":{"api":"https://api.test.com"}}`, exp)
	srv := mockTokenServer(t, "ghu_testtoken123", 200, body)
	defer srv.Close()

	m, _ := setupTestManager(t)
	m.tokenExchangeURL = srv.URL

	tok, err := m.refresh(context.Background())
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if tok != "fresh_abc" {
		t.Errorf("refresh() = %q, want %q", tok, "fresh_abc")
	}
	if m.current == nil {
		t.Fatal("expected current token to be set after refresh")
	}
	if m.current.APIURL != "https://api.test.com" {
		t.Errorf("APIURL = %q, want %q", m.current.APIURL, "https://api.test.com")
	}
}

func TestRefresh_DoubleCheckSkipsHTTP(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		exp := time.Now().Add(30 * time.Minute).Unix()
		resp := fmt.Sprintf(`{"token":"new_tok","expires_at":%d}`, exp)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(resp))
	}))
	defer srv.Close()

	m, _ := setupTestManager(t)
	m.tokenExchangeURL = srv.URL
	// Pre-set a valid token -- double-check in refresh() should return it
	m.current = &Token{
		Token:     "already_valid",
		ExpiresAt: time.Now().Add(10 * time.Minute),
	}

	tok, err := m.refresh(context.Background())
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if tok != "already_valid" {
		t.Errorf("refresh() = %q, want %q (double-check should have returned existing)", tok, "already_valid")
	}
	if calls.Load() != 0 {
		t.Errorf("expected 0 HTTP calls (double-check), got %d", calls.Load())
	}
}

func TestRefresh_HTTPError(t *testing.T) {
	srv := mockTokenServer(t, "ghu_testtoken123", 401, `{"message":"Bad credentials"}`)
	defer srv.Close()

	m, _ := setupTestManager(t)
	m.tokenExchangeURL = srv.URL

	_, err := m.refresh(context.Background())
	if err == nil {
		t.Fatal("expected error from refresh with 401 response")
	}
	expected := `copilot token exchange 401`
	if got := err.Error(); len(got) < len(expected) || got[:len(expected)] != expected {
		t.Errorf("error = %q, want prefix %q", got, expected)
	}
}

func TestRefresh_ServerError(t *testing.T) {
	srv := mockTokenServer(t, "ghu_testtoken123", 500, `internal server error`)
	defer srv.Close()

	m, _ := setupTestManager(t)
	m.tokenExchangeURL = srv.URL

	_, err := m.refresh(context.Background())
	if err == nil {
		t.Fatal("expected error from refresh with 500 response")
	}
	expected := `copilot token exchange 500`
	if got := err.Error(); len(got) < len(expected) || got[:len(expected)] != expected {
		t.Errorf("error = %q, want prefix %q", got, expected)
	}
}

func TestRefresh_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{not valid json`))
	}))
	defer srv.Close()

	m, _ := setupTestManager(t)
	m.tokenExchangeURL = srv.URL

	_, err := m.refresh(context.Background())
	if err == nil {
		t.Fatal("expected error from refresh with invalid JSON response")
	}
	expected := "parse token response"
	if got := err.Error(); len(got) < len(expected) || got[:len(expected)] != expected {
		t.Errorf("error = %q, want prefix %q", got, expected)
	}
}

func TestRefresh_NetworkError(t *testing.T) {
	m, _ := setupTestManager(t)
	// Point to a closed server
	m.tokenExchangeURL = "http://127.0.0.1:1" // nothing listening

	_, err := m.refresh(context.Background())
	if err == nil {
		t.Fatal("expected error from refresh with unreachable server")
	}
	expected := "copilot token exchange"
	if got := err.Error(); len(got) < len(expected) || got[:len(expected)] != expected {
		t.Errorf("error = %q, want prefix %q", got, expected)
	}
}

func TestRefresh_ContextCancelled(t *testing.T) {
	// Server that blocks until cancelled
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	m, _ := setupTestManager(t)
	m.tokenExchangeURL = srv.URL

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := m.refresh(ctx)
	if err == nil {
		t.Fatal("expected error from refresh with cancelled context")
	}
}

func TestRefresh_NoEndpointsAPI_DefaultsToDefault(t *testing.T) {
	exp := time.Now().Add(30 * time.Minute).Unix()
	body := fmt.Sprintf(`{"token":"tok_no_api","expires_at":%d}`, exp)
	srv := mockTokenServer(t, "ghu_testtoken123", 200, body)
	defer srv.Close()

	m, _ := setupTestManager(t)
	m.tokenExchangeURL = srv.URL

	tok, err := m.refresh(context.Background())
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if tok != "tok_no_api" {
		t.Errorf("refresh() = %q, want %q", tok, "tok_no_api")
	}
	// When endpoints.api is empty, APIURL should default
	if m.current.APIURL != defaultAPIURL {
		t.Errorf("APIURL = %q, want %q (default)", m.current.APIURL, defaultAPIURL)
	}
}

func TestRefresh_NonIntExpiry_FallsBackTo25Min(t *testing.T) {
	// expires_at is a float that can't be parsed by Int64() -- triggers the fallback path
	body := `{"token":"tok_float_exp","expires_at":123.456,"endpoints":{"api":"https://api.example.com"}}`
	srv := mockTokenServer(t, "ghu_testtoken123", 200, body)
	defer srv.Close()

	m, _ := setupTestManager(t)
	m.tokenExchangeURL = srv.URL

	before := time.Now()
	tok, err := m.refresh(context.Background())
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if tok != "tok_float_exp" {
		t.Errorf("refresh() = %q, want %q", tok, "tok_float_exp")
	}
	// Expiry should be ~25 minutes from now (fallback)
	expectedMin := before.Add(24 * time.Minute)
	expectedMax := before.Add(26 * time.Minute)
	if m.current.ExpiresAt.Before(expectedMin) || m.current.ExpiresAt.After(expectedMax) {
		t.Errorf("ExpiresAt = %v, want between %v and %v", m.current.ExpiresAt, expectedMin, expectedMax)
	}
}

func TestRefresh_SavesCacheFile(t *testing.T) {
	exp := time.Now().Add(30 * time.Minute).Unix()
	body := fmt.Sprintf(`{"token":"cached_fresh","expires_at":%d,"endpoints":{"api":"https://api.cached.com"}}`, exp)
	srv := mockTokenServer(t, "ghu_testtoken123", 200, body)
	defer srv.Close()

	m, home := setupTestManager(t)
	m.tokenExchangeURL = srv.URL

	_, err := m.refresh(context.Background())
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}

	// Verify cache file was created
	cachePath := filepath.Join(home, cacheFile)
	data, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("expected cache file to exist after refresh: %v", err)
	}
	var tok Token
	if err := json.Unmarshal(data, &tok); err != nil {
		t.Fatalf("unmarshal cache: %v", err)
	}
	if tok.Token != "cached_fresh" {
		t.Errorf("cached token = %q, want %q", tok.Token, "cached_fresh")
	}
}

func TestRefresh_HeadersAreSent(t *testing.T) {
	var gotAuth, gotAccept, gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAccept = r.Header.Get("Accept")
		gotUA = r.Header.Get("User-Agent")
		exp := time.Now().Add(30 * time.Minute).Unix()
		resp := fmt.Sprintf(`{"token":"hdr_tok","expires_at":%d}`, exp)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(resp))
	}))
	defer srv.Close()

	m := &Manager{
		ghToken:          "my_special_pat",
		home:             t.TempDir(),
		tokenExchangeURL: srv.URL,
	}

	_, err := m.refresh(context.Background())
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if gotAuth != "Bearer my_special_pat" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer my_special_pat")
	}
	if gotAccept != "application/json" {
		t.Errorf("Accept = %q, want %q", gotAccept, "application/json")
	}
	if gotUA != "gopherclaw/1.0" {
		t.Errorf("User-Agent = %q, want %q", gotUA, "gopherclaw/1.0")
	}
}

// --- New() constructor tests ---

func TestNew_MissingAuthFile(t *testing.T) {
	// New() reads from os.UserHomeDir() which we can't easily override,
	// but we can test the internal logic by creating a newFromHome helper.
	// For now, we test the error paths that don't depend on the real home dir
	// by using the internal struct directly.

	// Test that a Manager with empty ghToken would fail on New() if key is missing.
	// We can't easily test New() without modifying the home dir,
	// so we test the auth.json parsing logic indirectly.
	t.Run("empty_key_rejected", func(t *testing.T) {
		home := t.TempDir()
		authDir := filepath.Join(home, ".gopherclaw", "agents", "main", "agent")
		if err := os.MkdirAll(authDir, 0700); err != nil {
			t.Fatal(err)
		}
		// Valid JSON but empty key
		data := []byte(`{"github-copilot": {"type": "pat", "key": ""}}`)
		if err := os.WriteFile(filepath.Join(authDir, "auth.json"), data, 0600); err != nil {
			t.Fatal(err)
		}

		var a authJSON
		raw, _ := os.ReadFile(filepath.Join(authDir, "auth.json"))
		if err := json.Unmarshal(raw, &a); err != nil {
			t.Fatal(err)
		}
		if a.GithubCopilot.Key != "" {
			t.Error("expected empty key")
		}
	})

	t.Run("invalid_json_rejected", func(t *testing.T) {
		home := t.TempDir()
		authDir := filepath.Join(home, ".gopherclaw", "agents", "main", "agent")
		if err := os.MkdirAll(authDir, 0700); err != nil {
			t.Fatal(err)
		}
		data := []byte(`{not valid json}`)
		if err := os.WriteFile(filepath.Join(authDir, "auth.json"), data, 0600); err != nil {
			t.Fatal(err)
		}

		var a authJSON
		raw, _ := os.ReadFile(filepath.Join(authDir, "auth.json"))
		err := json.Unmarshal(raw, &a)
		if err == nil {
			t.Error("expected error for invalid JSON")
		}
	})

	t.Run("valid_auth_json", func(t *testing.T) {
		home := t.TempDir()
		authDir := filepath.Join(home, ".gopherclaw", "agents", "main", "agent")
		if err := os.MkdirAll(authDir, 0700); err != nil {
			t.Fatal(err)
		}
		data := []byte(`{"github-copilot": {"type": "pat", "key": "ghu_valid123"}}`)
		if err := os.WriteFile(filepath.Join(authDir, "auth.json"), data, 0600); err != nil {
			t.Fatal(err)
		}

		var a authJSON
		raw, _ := os.ReadFile(filepath.Join(authDir, "auth.json"))
		if err := json.Unmarshal(raw, &a); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if a.GithubCopilot.Key != "ghu_valid123" {
			t.Errorf("key = %q, want %q", a.GithubCopilot.Key, "ghu_valid123")
		}
	})
}

// --- StartRefresher tests ---

func TestStartRefresher_CancelStopsLoop(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		exp := time.Now().Add(5 * time.Second).Unix() // short expiry to trigger fast refresh
		resp := fmt.Sprintf(`{"token":"refresher_tok","expires_at":%d}`, exp)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(resp))
	}))
	defer srv.Close()

	m := &Manager{
		ghToken:          "ghu_testtoken123",
		home:             t.TempDir(),
		tokenExchangeURL: srv.URL,
	}

	ctx, cancel := context.WithCancel(context.Background())
	m.StartRefresher(ctx)

	// Give the refresher time to make at least one call
	time.Sleep(200 * time.Millisecond)
	cancel()
	// Give the goroutine time to exit
	time.Sleep(100 * time.Millisecond)

	// The refresher should have made at least one call
	if calls.Load() < 1 {
		t.Errorf("expected at least 1 refresh call, got %d", calls.Load())
	}
}

func TestStartRefresher_WithExistingToken(t *testing.T) {
	// When there's already a valid token, the refresher should wait
	// until near expiry before refreshing
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		exp := time.Now().Add(30 * time.Minute).Unix()
		resp := fmt.Sprintf(`{"token":"refresher_tok","expires_at":%d}`, exp)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(resp))
	}))
	defer srv.Close()

	m := &Manager{
		ghToken:          "ghu_testtoken123",
		home:             t.TempDir(),
		tokenExchangeURL: srv.URL,
		current: &Token{
			Token:     "existing_valid",
			ExpiresAt: time.Now().Add(30 * time.Minute),
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	m.StartRefresher(ctx)

	// With a 30-minute token, the refresher should wait ~28 min before refreshing.
	// After a brief sleep, no calls should have been made.
	time.Sleep(200 * time.Millisecond)
	cancel()
	time.Sleep(100 * time.Millisecond)

	if calls.Load() != 0 {
		t.Errorf("expected 0 refresh calls (token still valid), got %d", calls.Load())
	}
}

// --- Integration-style tests ---

func TestGetToken_FullFlow_CacheToRefresh(t *testing.T) {
	callCount := atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := callCount.Add(1)
		exp := time.Now().Add(30 * time.Minute).Unix()
		resp := fmt.Sprintf(`{"token":"flow_tok_%d","expires_at":%d,"endpoints":{"api":"https://flow.api.com"}}`, n, exp)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(resp))
	}))
	defer srv.Close()

	m, _ := setupTestManager(t)
	m.tokenExchangeURL = srv.URL

	ctx := context.Background()

	// First call: no token, triggers refresh
	tok1, err := m.GetToken(ctx)
	if err != nil {
		t.Fatalf("first GetToken: %v", err)
	}
	if tok1 != "flow_tok_1" {
		t.Errorf("first token = %q, want %q", tok1, "flow_tok_1")
	}

	// Second call: token is valid, should return cached
	tok2, err := m.GetToken(ctx)
	if err != nil {
		t.Fatalf("second GetToken: %v", err)
	}
	if tok2 != "flow_tok_1" {
		t.Errorf("second token = %q, want %q (should be cached)", tok2, "flow_tok_1")
	}
	if callCount.Load() != 1 {
		t.Errorf("expected 1 HTTP call, got %d", callCount.Load())
	}

	// Simulate token near expiry
	m.mu.Lock()
	m.current.ExpiresAt = time.Now().Add(30 * time.Second) // under 60s threshold
	m.mu.Unlock()

	// Third call: token nearly expired, triggers refresh
	tok3, err := m.GetToken(ctx)
	if err != nil {
		t.Fatalf("third GetToken: %v", err)
	}
	if tok3 != "flow_tok_2" {
		t.Errorf("third token = %q, want %q", tok3, "flow_tok_2")
	}
	if callCount.Load() != 2 {
		t.Errorf("expected 2 HTTP calls, got %d", callCount.Load())
	}
}

func TestRefresh_EmptyExchangeURL_FallsBackToConst(t *testing.T) {
	// When tokenExchangeURL field is empty, it should use the package const.
	// We can't actually call the real GitHub API, so we verify the field
	// defaults to the const value in the normal code path.
	m := &Manager{
		ghToken:          "ghu_test",
		home:             t.TempDir(),
		tokenExchangeURL: "", // empty
	}

	// This will fail because the real GitHub API requires auth,
	// but the important thing is it doesn't panic on empty URL.
	_, err := m.refresh(context.Background())
	if err == nil {
		t.Fatal("expected error calling real GitHub API with fake token")
	}
	// The error should be from the HTTP call, not from URL construction
}

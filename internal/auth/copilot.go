package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	tokenExchangeURL = "https://api.github.com/copilot_internal/v2/token"
	cacheFile        = ".gopherclaw/state/credentials/github-copilot.token.json"
	authFile         = ".gopherclaw/agents/main/agent/auth.json"
)

// Token is a short-lived Copilot API token.
type Token struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
	APIURL    string    `json:"api_url,omitempty"` // from endpoints.api
}

const defaultAPIURL = "https://api.enterprise.githubcopilot.com"

type authJSON struct {
	GithubCopilot struct {
		Type string `json:"type"`
		Key  string `json:"key"`
	} `json:"github-copilot"`
}

// Manager handles GitHub Copilot token exchange and caching.
type Manager struct {
	mu               sync.RWMutex
	current          *Token
	ghToken          string
	home             string
	tokenExchangeURL string // overridable for tests; defaults to tokenExchangeURL const
}

// APIURL returns the Copilot API base URL (from the last token exchange).
func (m *Manager) APIURL() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.current != nil && m.current.APIURL != "" {
		return m.current.APIURL
	}
	return defaultAPIURL
}

// New creates a Manager, loads the GitHub PAT from auth.json.
func New() (*Manager, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(filepath.Join(home, authFile))
	if err != nil {
		return nil, fmt.Errorf("read auth.json: %w", err)
	}
	var a authJSON
	if err := json.Unmarshal(data, &a); err != nil {
		return nil, fmt.Errorf("parse auth.json: %w", err)
	}
	if a.GithubCopilot.Key == "" {
		return nil, fmt.Errorf("no github-copilot key in auth.json")
	}

	m := &Manager{ghToken: a.GithubCopilot.Key, home: home, tokenExchangeURL: tokenExchangeURL}

	// Try loading cached token
	m.loadCache()

	return m, nil
}

// GetToken returns a valid Copilot access token, refreshing if needed.
func (m *Manager) GetToken(ctx context.Context) (string, error) {
	m.mu.RLock()
	if m.current != nil && time.Until(m.current.ExpiresAt) > 60*time.Second {
		t := m.current.Token
		m.mu.RUnlock()
		return t, nil
	}
	m.mu.RUnlock()

	return m.refresh(ctx)
}

// StartRefresher launches a background goroutine that proactively refreshes the token.
func (m *Manager) StartRefresher(ctx context.Context) {
	go func() {
		for {
			m.mu.RLock()
			var wait time.Duration
			if m.current != nil {
				wait = max(time.Until(m.current.ExpiresAt)-2*time.Minute, 0)
			}
			m.mu.RUnlock()

			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
				if _, err := m.refresh(ctx); err != nil {
					// retry in 30s
					time.Sleep(30 * time.Second)
				}
			}
		}
	}()
}

func (m *Manager) refresh(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Double-check after acquiring write lock
	if m.current != nil && time.Until(m.current.ExpiresAt) > 60*time.Second {
		return m.current.Token, nil
	}

	exchangeURL := m.tokenExchangeURL
	if exchangeURL == "" {
		exchangeURL = tokenExchangeURL
	}
	req, err := http.NewRequestWithContext(ctx, "GET", exchangeURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+m.ghToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "gopherclaw/1.0")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("copilot token exchange: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MB limit
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("copilot token exchange %d: %s", resp.StatusCode, body)
	}

	// GitHub returns: {"token":"...","expires_at":1234567890,"endpoints":{"api":"..."},...}
	var raw struct {
		Token     string      `json:"token"`
		ExpiresAt json.Number `json:"expires_at"`
		Endpoints struct {
			API string `json:"api"`
		} `json:"endpoints"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return "", fmt.Errorf("parse token response: %w", err)
	}

	var expiry time.Time
	if ts, err := raw.ExpiresAt.Int64(); err == nil {
		expiry = time.Unix(ts, 0)
	} else {
		expiry = time.Now().Add(25 * time.Minute)
	}

	apiURL := raw.Endpoints.API
	if apiURL == "" {
		apiURL = defaultAPIURL
	}

	m.current = &Token{Token: raw.Token, ExpiresAt: expiry, APIURL: apiURL}
	m.saveCache()

	return m.current.Token, nil
}

func (m *Manager) loadCache() {
	path := filepath.Join(m.home, cacheFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var t Token
	if err := json.Unmarshal(data, &t); err != nil {
		return
	}
	if time.Until(t.ExpiresAt) > 60*time.Second {
		m.current = &t
	}
}

func (m *Manager) saveCache() {
	path := filepath.Join(m.home, cacheFile)
	_ = os.MkdirAll(filepath.Dir(path), 0700)
	data, _ := json.MarshalIndent(m.current, "", "  ")
	_ = os.WriteFile(path, data, 0600)
}

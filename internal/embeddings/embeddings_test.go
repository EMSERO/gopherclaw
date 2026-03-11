package embeddings

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// openaiEmbeddingResponse mirrors the OpenAI embeddings API response shape.
type openaiEmbeddingResponse struct {
	Object string                   `json:"object"`
	Data   []openaiEmbeddingData    `json:"data"`
	Model  string                   `json:"model"`
	Usage  openaiEmbeddingUsage     `json:"usage"`
}

type openaiEmbeddingData struct {
	Object    string    `json:"object"`
	Embedding []float32 `json:"embedding"`
	Index     int       `json:"index"`
}

type openaiEmbeddingUsage struct {
	PromptTokens int `json:"prompt_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

func newTestServer(handler http.HandlerFunc) *httptest.Server {
	return httptest.NewServer(handler)
}

func successHandler(embeddingCount int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input interface{} `json:"input"`
			Model string      `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		// Determine how many embeddings to return.
		count := embeddingCount
		if count == 0 {
			// Try to figure out from the input.
			switch v := req.Input.(type) {
			case []interface{}:
				count = len(v)
			case string:
				count = 1
			default:
				count = 1
			}
		}

		data := make([]openaiEmbeddingData, count)
		for i := 0; i < count; i++ {
			data[i] = openaiEmbeddingData{
				Object:    "embedding",
				Embedding: []float32{0.1, 0.2, 0.3, 0.4},
				Index:     i,
			}
		}

		resp := openaiEmbeddingResponse{
			Object: "list",
			Data:   data,
			Model:  req.Model,
			Usage:  openaiEmbeddingUsage{PromptTokens: 5, TotalTokens: 5},
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}
}

// --- New() tests ---

func TestNew_ValidConfig(t *testing.T) {
	c := New(Config{
		Provider: "openai",
		Model:    "text-embedding-3-small",
		APIKey:   "test-key",
	})
	if c == nil {
		t.Fatal("expected non-nil client")
	}
	if c.model != "text-embedding-3-small" {
		t.Errorf("model = %q, want %q", c.model, "text-embedding-3-small")
	}
}

func TestNew_EmptyAPIKey(t *testing.T) {
	c := New(Config{
		Provider: "ollama",
		Model:    "nomic-embed-text",
	})
	if c == nil {
		t.Fatal("expected non-nil client")
	}
	// The client should still be created with "no-key" fallback.
}

func TestNew_CustomBaseURL(t *testing.T) {
	c := New(Config{
		BaseURL: "http://localhost:9999/v1",
		Model:   "test-model",
		APIKey:  "key",
	})
	if c == nil {
		t.Fatal("expected non-nil client")
	}
}

func TestNew_Dimensions(t *testing.T) {
	c := New(Config{
		Provider:   "openai",
		Model:      "text-embedding-3-small",
		APIKey:     "key",
		Dimensions: 256,
	})
	if c.dims != 256 {
		t.Errorf("dims = %d, want 256", c.dims)
	}
}

// --- Embed() tests ---

func TestEmbed_Success(t *testing.T) {
	srv := newTestServer(successHandler(1))
	defer srv.Close()

	c := New(Config{
		BaseURL: srv.URL,
		Model:   "test-model",
		APIKey:  "key",
	})

	vec, err := c.Embed(context.Background(), "hello world")
	if err != nil {
		t.Fatalf("Embed() error: %v", err)
	}
	if len(vec) != 4 {
		t.Errorf("len(vec) = %d, want 4", len(vec))
	}
	if vec[0] != 0.1 {
		t.Errorf("vec[0] = %f, want 0.1", vec[0])
	}
}

func TestEmbed_APIError(t *testing.T) {
	srv := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]interface{}{
				"message": "internal server error",
				"type":    "server_error",
			},
		})
	})
	defer srv.Close()

	c := New(Config{
		BaseURL: srv.URL,
		Model:   "test-model",
		APIKey:  "key",
	})

	_, err := c.Embed(context.Background(), "hello")
	if err == nil {
		t.Fatal("expected error from Embed()")
	}
}

func TestEmbed_Timeout(t *testing.T) {
	srv := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
	})
	defer srv.Close()

	c := New(Config{
		BaseURL: srv.URL,
		Model:   "test-model",
		APIKey:  "key",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := c.Embed(ctx, "hello")
	if err == nil {
		t.Fatal("expected timeout error from Embed()")
	}
}

func TestEmbed_EmptyResponse(t *testing.T) {
	srv := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		resp := openaiEmbeddingResponse{
			Object: "list",
			Data:   []openaiEmbeddingData{},
			Model:  "test-model",
			Usage:  openaiEmbeddingUsage{},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})
	defer srv.Close()

	c := New(Config{
		BaseURL: srv.URL,
		Model:   "test-model",
		APIKey:  "key",
	})

	_, err := c.Embed(context.Background(), "hello")
	if err == nil {
		t.Fatal("expected empty response error")
	}
	if err.Error() != "embeddings: empty response" {
		t.Errorf("unexpected error: %v", err)
	}
}

// --- EmbedBatch() tests ---

func TestEmbedBatch_Success(t *testing.T) {
	srv := newTestServer(successHandler(0))
	defer srv.Close()

	c := New(Config{
		BaseURL: srv.URL,
		Model:   "test-model",
		APIKey:  "key",
	})

	vecs, err := c.EmbedBatch(context.Background(), []string{"hello", "world", "foo"})
	if err != nil {
		t.Fatalf("EmbedBatch() error: %v", err)
	}
	if len(vecs) != 3 {
		t.Errorf("len(vecs) = %d, want 3", len(vecs))
	}
	for i, v := range vecs {
		if len(v) != 4 {
			t.Errorf("len(vecs[%d]) = %d, want 4", i, len(v))
		}
	}
}

func TestEmbedBatch_EmptyInput(t *testing.T) {
	c := New(Config{
		BaseURL: "http://unused",
		Model:   "test-model",
		APIKey:  "key",
	})

	vecs, err := c.EmbedBatch(context.Background(), []string{})
	if err != nil {
		t.Fatalf("EmbedBatch() error: %v", err)
	}
	if vecs != nil {
		t.Errorf("expected nil, got %v", vecs)
	}
}

func TestEmbedBatch_NilInput(t *testing.T) {
	c := New(Config{
		BaseURL: "http://unused",
		Model:   "test-model",
		APIKey:  "key",
	})

	vecs, err := c.EmbedBatch(context.Background(), nil)
	if err != nil {
		t.Fatalf("EmbedBatch() error: %v", err)
	}
	if vecs != nil {
		t.Errorf("expected nil, got %v", vecs)
	}
}

func TestEmbedBatch_APIError(t *testing.T) {
	srv := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]interface{}{
				"message": "rate limit exceeded",
				"type":    "rate_limit_error",
			},
		})
	})
	defer srv.Close()

	c := New(Config{
		BaseURL: srv.URL,
		Model:   "test-model",
		APIKey:  "key",
	})

	_, err := c.EmbedBatch(context.Background(), []string{"hello", "world"})
	if err == nil {
		t.Fatal("expected error from EmbedBatch()")
	}
}

func TestEmbedBatch_SingleItem(t *testing.T) {
	srv := newTestServer(successHandler(0))
	defer srv.Close()

	c := New(Config{
		BaseURL: srv.URL,
		Model:   "test-model",
		APIKey:  "key",
	})

	vecs, err := c.EmbedBatch(context.Background(), []string{"single"})
	if err != nil {
		t.Fatalf("EmbedBatch() error: %v", err)
	}
	if len(vecs) != 1 {
		t.Errorf("len(vecs) = %d, want 1", len(vecs))
	}
}

// --- defaultBaseURL() tests ---

func TestDefaultBaseURL_Ollama(t *testing.T) {
	got := defaultBaseURL("ollama")
	want := "http://localhost:11434/v1"
	if got != want {
		t.Errorf("defaultBaseURL(ollama) = %q, want %q", got, want)
	}
}

func TestDefaultBaseURL_OpenAI(t *testing.T) {
	got := defaultBaseURL("openai")
	want := "https://api.openai.com/v1"
	if got != want {
		t.Errorf("defaultBaseURL(openai) = %q, want %q", got, want)
	}
}

func TestDefaultBaseURL_OpenRouter(t *testing.T) {
	got := defaultBaseURL("openrouter")
	want := "https://openrouter.ai/api/v1"
	if got != want {
		t.Errorf("defaultBaseURL(openrouter) = %q, want %q", got, want)
	}
}

func TestDefaultBaseURL_Unknown(t *testing.T) {
	got := defaultBaseURL("some-unknown-provider")
	want := "https://api.openai.com/v1"
	if got != want {
		t.Errorf("defaultBaseURL(unknown) = %q, want %q", got, want)
	}
}

func TestDefaultBaseURL_Empty(t *testing.T) {
	got := defaultBaseURL("")
	want := "https://api.openai.com/v1"
	if got != want {
		t.Errorf("defaultBaseURL('') = %q, want %q", got, want)
	}
}

// --- Concurrency test ---

func TestEmbed_ConcurrentSafety(t *testing.T) {
	srv := newTestServer(successHandler(1))
	defer srv.Close()

	c := New(Config{
		BaseURL: srv.URL,
		Model:   "test-model",
		APIKey:  "key",
	})

	errs := make(chan error, 10)
	for i := 0; i < 10; i++ {
		go func() {
			_, err := c.Embed(context.Background(), "concurrent test")
			errs <- err
		}()
	}

	for i := 0; i < 10; i++ {
		if err := <-errs; err != nil {
			t.Errorf("concurrent Embed() error: %v", err)
		}
	}
}

// --- Auth header test ---

func TestEmbed_SendsAuthHeader(t *testing.T) {
	srv := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer my-secret-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		successHandler(1)(w, r)
	})
	defer srv.Close()

	c := New(Config{
		BaseURL: srv.URL,
		Model:   "test-model",
		APIKey:  "my-secret-key",
	})

	_, err := c.Embed(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Embed() with auth error: %v", err)
	}
}

func TestEmbed_ContextCanceled(t *testing.T) {
	srv := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Second)
	})
	defer srv.Close()

	c := New(Config{
		BaseURL: srv.URL,
		Model:   "test-model",
		APIKey:  "key",
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := c.Embed(ctx, "hello")
	if err == nil {
		t.Fatal("expected context canceled error")
	}
}

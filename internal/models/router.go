package models

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"strings"
	"sync"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"go.uber.org/zap"

	"github.com/EMSERO/gopherclaw/internal/taskqueue"
)

const (
	defaultMaxRetries  = 2     // up to 2 retries (3 total attempts) per model
	defaultRetryBaseMs = 500   // initial backoff 500ms
	defaultRetryMaxMs  = 10000 // max backoff 10s
)

// APIError is a structured error returned by providers that includes the HTTP
// status code and optional Retry-After duration from the server.
type APIError struct {
	StatusCode int
	RetryAfter time.Duration // parsed from Retry-After header; 0 = not specified
	Message    string
}

func (e *APIError) Error() string {
	return e.Message
}

// Router dispatches chat completion requests to the correct provider based
// on the provider prefix in model strings (e.g. "anthropic/claude-opus-4-6").
// It tries the primary model first, then each fallback in order.
// Within each model attempt, transient errors (429, 5xx, connection resets)
// are retried with exponential backoff before moving to the next model.
type Router struct {
	mu               sync.RWMutex
	providers        map[string]Provider
	primary          string
	fallbacks        []string
	Timeout          time.Duration                     // per-model timeout; 0 = use caller's ctx deadline
	StreamConnectTTL time.Duration                     // stream connect timeout for primary when fallbacks exist; 0 = 30s
	RateLimiters     map[string]*taskqueue.RateLimiter // provider name → rate limiter (optional)
	Cooldowns        *Cooldown                         // per-model cooldowns (optional); nil = no cooldowns
	MaxRetries       int                               // max retries per model for transient errors; 0 = use default (2)
	logger           *zap.SugaredLogger
}

// NewRouter creates a Router with the given provider registry and model config.
// Model strings must use the "provider/model-id" format.
// Models without a "/" separator are routed to the "github-copilot" provider.
func NewRouter(logger *zap.SugaredLogger, providers map[string]Provider, primary string, fallbacks []string) *Router {
	return &Router{providers: providers, primary: primary, fallbacks: fallbacks, logger: logger}
}

// SetModels updates the primary model and fallbacks at runtime (e.g. hot-reload).
func (r *Router) SetModels(primary string, fallbacks []string) {
	clone := make([]string, len(fallbacks))
	copy(clone, fallbacks)
	r.mu.Lock()
	r.primary = primary
	r.fallbacks = clone
	r.mu.Unlock()
}

// Chat sends a chat completion request, trying primary then fallbacks.
// The Model field in req should be the full "provider/model-id" string;
// the router strips the provider prefix before calling the provider.
// Transient errors (429, 5xx, connection failures) are retried with
// exponential backoff before falling through to the next model.
func (r *Router) Chat(ctx context.Context, req openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
	models := r.modelList(req.Model)
	maxRetry := r.maxRetries()
	var lastErr error
	allCooldown := true
	for i, fullModel := range models {
		// Skip models in cooldown.
		if r.Cooldowns != nil && !r.Cooldowns.IsAvailable(fullModel) {
			r.logger.Warnf("model in cooldown, skipping: model=%s remaining=%v", fullModel, r.Cooldowns.CooldownRemaining(fullModel))
			continue
		}
		allCooldown = false
		p, modelID, err := ProviderFor(r.providers, fullModel)
		if err != nil {
			r.logger.Warnf("model attempt failed: model=%s attempt=%d/%d err=%v", fullModel, i+1, len(models), err)
			lastErr = err
			continue
		}
		req.Model = modelID
		if rl := r.rateLimiter(fullModel); rl != nil {
			if err := rl.Wait(ctx); err != nil {
				r.logger.Warnf("model attempt rate-limited: model=%s attempt=%d/%d err=%v", fullModel, i+1, len(models), err)
				lastErr = err
				continue
			}
		}
		// Retry loop for transient errors on this model.
		var retryHint time.Duration
		for retry := 0; retry <= maxRetry; retry++ {
			if retry > 0 {
				r.logger.Infof("retrying model: model=%s retry=%d/%d", fullModel, retry, maxRetry)
				if !retryBackoff(ctx, retry-1, retryHint) {
					break // context expired
				}
			}
			modelCtx, cancel := r.modelContext(ctx, i, len(models))
			resp, err := p.Chat(modelCtx, req)
			cancel()
			if err == nil {
				if r.Cooldowns != nil {
					r.Cooldowns.RecordSuccess(fullModel)
				}
				return resp, nil
			}
			lastErr = err
			retryHint = retryAfterFromErr(err)
			if !isRetryable(err) || retry == maxRetry {
				r.logger.Warnf("model attempt failed: model=%s attempt=%d/%d retry=%d/%d err=%v", fullModel, i+1, len(models), retry, maxRetry, err)
				break
			}
			r.logger.Warnf("model transient error, will retry: model=%s attempt=%d/%d retry=%d/%d err=%v", fullModel, i+1, len(models), retry, maxRetry, err)
		}
		// Only cooldown on transient errors — permanent errors (401, 403) won't recover with time.
		if r.Cooldowns != nil && isRetryable(lastErr) {
			r.Cooldowns.RecordFailure(fullModel)
		}
	}
	if allCooldown && lastErr == nil {
		return openai.ChatCompletionResponse{}, fmt.Errorf("all models in cooldown, none available")
	}
	return openai.ChatCompletionResponse{}, fmt.Errorf("all models failed: %w", lastErr)
}

// ChatStream sends a streaming chat completion request.
// Returns a Stream for the first model that successfully starts; tries fallbacks on error.
// Each model gets a connect-timeout sub-context to prevent a hanging primary from
// consuming the entire caller deadline and starving fallbacks.
// Transient errors are retried with exponential backoff before moving to the next model.
// On success, the sub-context is NOT canceled — the stream's HTTP body needs it alive.
// It will expire naturally when the parent context is done.
func (r *Router) ChatStream(ctx context.Context, req openai.ChatCompletionRequest) (Stream, error) {
	models := r.modelList(req.Model)
	maxRetry := r.maxRetries()
	var lastErr error
	allCooldownStream := true
	for i, fullModel := range models {
		// Skip models in cooldown.
		if r.Cooldowns != nil && !r.Cooldowns.IsAvailable(fullModel) {
			r.logger.Warnf("model in cooldown, skipping stream: model=%s remaining=%v", fullModel, r.Cooldowns.CooldownRemaining(fullModel))
			continue
		}
		allCooldownStream = false
		p, modelID, err := ProviderFor(r.providers, fullModel)
		if err != nil {
			r.logger.Warnf("model stream attempt failed: model=%s attempt=%d/%d err=%v", fullModel, i+1, len(models), err)
			lastErr = err
			continue
		}
		req.Model = modelID
		if rl := r.rateLimiter(fullModel); rl != nil {
			if err := rl.Wait(ctx); err != nil {
				r.logger.Warnf("model stream attempt rate-limited: model=%s attempt=%d/%d err=%v", fullModel, i+1, len(models), err)
				lastErr = err
				continue
			}
		}
		// Retry loop for transient errors on this model.
		var streamRetryHint time.Duration
		for retry := 0; retry <= maxRetry; retry++ {
			if retry > 0 {
				r.logger.Infof("retrying model stream: model=%s retry=%d/%d", fullModel, retry, maxRetry)
				if !retryBackoff(ctx, retry-1, streamRetryHint) {
					break // context expired
				}
			}
			// Use a short connect-timeout so a hanging primary doesn't starve
			// fallbacks. Once connected, the stream reads use the parent ctx
			// which has the full caller deadline (e.g. 3600s).
			//
			// We achieve this by racing a connect timer against ChatStream.
			// If ChatStream returns before the timer, we cancel the timer;
			// the stream body is tied to the parent ctx, not the timer.
			streamCtx, streamCancel := context.WithCancel(ctx)
			connectTTL := r.StreamConnectTTL
			if connectTTL <= 0 {
				connectTTL = 30 * time.Second
			}
			// Only apply connect timeout to the primary model when fallbacks exist.
			applyConnectTimeout := (i == 0 && len(models) > 1)

			type streamResult struct {
				stream Stream
				err    error
			}
			ch := make(chan streamResult, 1)
			go func() {
				s, err := p.ChatStream(streamCtx, req)
				ch <- streamResult{s, err}
			}()

			var stream Stream
			var err error
			if applyConnectTimeout {
				timer := time.NewTimer(connectTTL)
				select {
				case res := <-ch:
					timer.Stop()
					stream, err = res.stream, res.err
				case <-timer.C:
					streamCancel()
					<-ch // wait for goroutine to finish
					err = fmt.Errorf("stream connect timeout (%v) for %s", connectTTL, fullModel)
				}
			} else {
				res := <-ch
				stream, err = res.stream, res.err
			}

			if err != nil {
				streamCancel()
				lastErr = err
				streamRetryHint = retryAfterFromErr(err)
				if !isRetryable(err) || retry == maxRetry {
					r.logger.Warnf("model stream attempt failed: model=%s attempt=%d/%d retry=%d/%d err=%v", fullModel, i+1, len(models), retry, maxRetry, err)
					break
				}
				r.logger.Warnf("model stream transient error, will retry: model=%s attempt=%d/%d retry=%d/%d err=%v", fullModel, i+1, len(models), retry, maxRetry, err)
				continue
			}
			if r.Cooldowns != nil {
				r.Cooldowns.RecordSuccess(fullModel)
			}
			// Wrap the stream so streamCancel is called when the caller
			// closes it, preventing the context from leaking.
			return &cancelOnCloseStream{Stream: stream, cancel: streamCancel}, nil
		}
		if r.Cooldowns != nil && isRetryable(lastErr) {
			r.Cooldowns.RecordFailure(fullModel)
		}
	}
	if allCooldownStream && lastErr == nil {
		return nil, fmt.Errorf("all models in cooldown, none available")
	}
	return nil, fmt.Errorf("all models failed: %w", lastErr)
}

// modelContext returns a context with a per-model timeout. When the primary
// model has fallbacks available, its timeout is capped at 60s to fail fast.
func (r *Router) modelContext(ctx context.Context, idx, total int) (context.Context, context.CancelFunc) {
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	// Cap primary at 60s when fallbacks exist
	if idx == 0 && total > 1 && timeout > 60*time.Second {
		timeout = 60 * time.Second
	}
	return context.WithTimeout(ctx, timeout)
}

// StreamAll collects a full streaming response into a string.
func StreamAll(stream Stream) (string, error) {
	var sb strings.Builder
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return sb.String(), err
		}
		if len(chunk.Choices) > 0 {
			sb.WriteString(chunk.Choices[0].Delta.Content)
		}
	}
	return sb.String(), nil
}

// ModelHealth returns the health status of the primary model and fallbacks.
// It checks provider registration and cooldown state without making API calls.
type ModelHealthStatus struct {
	Model     string `json:"model"`
	Provider  string `json:"provider"`
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"` // why unavailable
}

func (r *Router) ModelHealth() []ModelHealthStatus {
	models := r.modelList("")
	out := make([]ModelHealthStatus, 0, len(models))
	for _, fullModel := range models {
		providerID, _ := splitModel(fullModel)
		status := ModelHealthStatus{Model: fullModel, Provider: providerID, Available: true}

		// Check provider registered.
		if _, _, err := ProviderFor(r.providers, fullModel); err != nil {
			status.Available = false
			status.Reason = "provider not registered"
			out = append(out, status)
			continue
		}
		// Check cooldown.
		if r.Cooldowns != nil && !r.Cooldowns.IsAvailable(fullModel) {
			status.Available = false
			remaining := r.Cooldowns.CooldownRemaining(fullModel)
			status.Reason = fmt.Sprintf("cooldown (%v remaining)", remaining.Truncate(time.Second))
		}
		out = append(out, status)
	}
	return out
}

// rateLimiter returns the rate limiter for a model's provider, or nil.
func (r *Router) rateLimiter(fullModel string) *taskqueue.RateLimiter {
	if len(r.RateLimiters) == 0 {
		return nil
	}
	parts := strings.SplitN(fullModel, "/", 2)
	if len(parts) < 2 {
		return r.RateLimiters[fullModel]
	}
	return r.RateLimiters[parts[0]]
}

func (r *Router) modelList(override string) []string {
	r.mu.RLock()
	primary := r.primary
	fallbacks := r.fallbacks
	r.mu.RUnlock()
	if override != "" && override != primary {
		return []string{override}
	}
	out := []string{primary}
	out = append(out, fallbacks...)
	return out
}

// maxRetries returns the configured max retries per model, or the default.
func (r *Router) maxRetries() int {
	if r.MaxRetries > 0 {
		return r.MaxRetries
	}
	return defaultMaxRetries
}

// isRetryable returns true for transient errors worth retrying: rate limits (429),
// server errors (5xx), and network-level failures (connection reset, timeout, etc.).
// Context cancellation and deadline exceeded are NOT retryable — they indicate the
// caller gave up or the overall session timed out.
func isRetryable(err error) bool {
	if err == nil {
		return false
	}
	// Never retry context cancellation or deadline exceeded — those are intentional.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	// Structured APIError — check status code directly.
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		switch apiErr.StatusCode {
		case 408, 429, 500, 502, 503, 504, 529:
			return true
		}
		return false
	}
	// Fallback: string matching for errors from other providers (go-openai, etc.).
	lower := strings.ToLower(err.Error())
	if strings.Contains(lower, "status code: 429") ||
		strings.Contains(lower, "status code: 500") ||
		strings.Contains(lower, "status code: 502") ||
		strings.Contains(lower, "status code: 503") ||
		strings.Contains(lower, "status code: 504") ||
		strings.Contains(lower, "status code: 529") {
		return true
	}
	if strings.Contains(lower, "rate limit") ||
		strings.Contains(lower, "too many requests") ||
		strings.Contains(lower, "overloaded") {
		return true
	}
	// Network-level transient failures.
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	if strings.Contains(lower, "connection reset") ||
		strings.Contains(lower, "connection refused") ||
		strings.Contains(lower, "broken pipe") ||
		strings.Contains(lower, "eof") {
		return true
	}
	return false
}

// retryAfterFromErr extracts the Retry-After duration from an APIError, or 0.
func retryAfterFromErr(err error) time.Duration {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.RetryAfter
	}
	return 0
}

// retryBackoff sleeps for exponential backoff with jitter, or returns early
// if the context is canceled. Returns false if the context expired.
// If serverHint > 0 (from Retry-After), it is used as the minimum delay.
func retryBackoff(ctx context.Context, attempt int, serverHint time.Duration) bool {
	baseMs := defaultRetryBaseMs * (1 << attempt) // 500ms, 1000ms, 2000ms, ...
	if baseMs > defaultRetryMaxMs {
		baseMs = defaultRetryMaxMs
	}
	// Honor server's Retry-After if it's longer than our calculated backoff.
	if hint := int(serverHint.Milliseconds()); hint > baseMs {
		// Cap server hint at our max to avoid unbounded waits.
		if hint > defaultRetryMaxMs {
			hint = defaultRetryMaxMs
		}
		baseMs = hint
	}
	// Add ±25% jitter.
	jitter := baseMs / 4
	delayMs := baseMs - jitter + rand.Intn(2*jitter+1)
	timer := time.NewTimer(time.Duration(delayMs) * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

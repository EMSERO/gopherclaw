// Package agent — compaction.go implements LLM-powered context compaction (REQ-400, REQ-401).
//
// When context exceeds the pruning threshold and surgical pruning is insufficient,
// the compaction system:
//  1. Splits messages into token-budgeted chunks (adaptive ratio: base 0.4, min 0.15)
//  2. Strips tool result details before summarization (DEC-051)
//  3. Summarizes each chunk via the active model with identifier preservation
//  4. Merges summaries preserving active tasks, decisions, TODOs, commitments
//  5. Falls back progressively: full → partial → note → hard clear
//
// The context window guard (REQ-401) hard-blocks models with <16K context and
// warns about models with <32K context.
package agent

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	openai "github.com/sashabaranov/go-openai"
	"go.uber.org/zap"

	"github.com/EMSERO/gopherclaw/internal/session"
)

// Compaction constants per architecture spec.
const (
	SummarizationOverheadTokens = 4096
	BaseChunkRatio              = 0.4
	MinChunkRatio               = 0.15
	SafetyMargin                = 1.2

	// Context window guard thresholds (REQ-401)
	ContextWindowMinimum = 16_000 // hard block below this
	ContextWindowWarning = 32_000 // warning below this

	// Retry settings for summarization
	compactionMaxRetries   = 3
	compactionBaseDelay    = 500 * time.Millisecond
	compactionMaxDelay     = 5 * time.Second
	compactionJitterFactor = 0.2

	// Oversized message threshold (>50% of context window)
	oversizedRatio = 0.5
)

// compactionInstructions is the system prompt for chunk summarization.
const compactionChunkPrompt = `Summarize the following conversation segment concisely, preserving:
- Active tasks and their current state
- Key decisions made
- Important facts, numbers, and context needed to continue
- TODOs and commitments
- Batch/step progress markers

CRITICAL: Preserve all identifiers EXACTLY as they appear:
- UUIDs, hashes, commit SHAs, session IDs, token strings
- API keys, auth tokens (masked portions too)
- Hostnames, IP addresses, ports, URLs
- File paths, directory names, package names
- Variable names, function names, class names

Be concise but comprehensive. Output only the summary, no preamble.`

const compactionMergePrompt = `Merge these conversation summaries into a single coherent summary.
Preserve:
- Active tasks and batch progress
- The last user request and its current state
- Key decisions and their rationale
- TODOs, commitments, and deadlines
- All identifiers (UUIDs, hashes, IPs, URLs, filenames) exactly as written

Prioritize recent context over older context. Eliminate redundancy.
Output only the merged summary, no preamble.`

// CompactorClient is the model interface needed for compaction.
type CompactorClient interface {
	Chat(ctx context.Context, req openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error)
}

// CompactHistory runs LLM-powered compaction on messages that exceed the token budget.
// It returns a compacted message list or the original messages if compaction fails.
// keepN is the number of recent assistant turns to preserve intact.
func CompactHistory(ctx context.Context, logger *zap.SugaredLogger, client CompactorClient, model string, msgs []session.Message, maxTokens, keepN int) ([]session.Message, error) {
	if keepN <= 0 {
		keepN = 2
	}

	// Find the split point: protect the last keepN assistant messages and their associated tool sequences
	splitAt := findCompactionSplitPoint(msgs, keepN)
	if splitAt <= 0 {
		return msgs, nil // nothing to compact
	}

	oldMsgs := msgs[:splitAt]
	recentMsgs := msgs[splitAt:]

	// Calculate token budget for chunks (account for summarization overhead)
	budgetTokens := int(float64(maxTokens)/SafetyMargin) - SummarizationOverheadTokens
	if budgetTokens <= 0 {
		return msgs, fmt.Errorf("compaction: token budget too small (%d max tokens)", maxTokens)
	}

	// Adaptive chunk ratio based on average message size
	ratio := adaptiveChunkRatio(oldMsgs, maxTokens)
	chunkBudget := int(ratio * float64(budgetTokens))
	if chunkBudget < 500 {
		chunkBudget = 500
	}

	// Split into token-budgeted chunks
	chunks := chunkByTokenBudget(oldMsgs, chunkBudget, maxTokens)
	if len(chunks) == 0 {
		return msgs, nil
	}

	// Summarize each chunk
	var summaries []string
	for i, chunk := range chunks {
		summary, err := summarizeChunkWithRetry(ctx, client, model, chunk)
		if err != nil {
			logger.Warnf("compaction: chunk %d/%d summarization failed: %v", i+1, len(chunks), err)
			// Progressive fallback: try without oversized messages
			summary, err = summarizeChunkPartial(ctx, client, model, chunk, maxTokens)
			if err != nil {
				logger.Warnf("compaction: chunk %d/%d partial summarization also failed: %v", i+1, len(chunks), err)
				summaries = append(summaries, "[Earlier conversation context was compacted but summarization failed]")
				continue
			}
		}
		summaries = append(summaries, summary)
	}

	if len(summaries) == 0 {
		return msgs, fmt.Errorf("compaction: all chunks failed to summarize")
	}

	// Merge summaries if multiple chunks
	var finalSummary string
	if len(summaries) == 1 {
		finalSummary = summaries[0]
	} else {
		merged, err := mergeSummaries(ctx, client, model, summaries)
		if err != nil {
			logger.Warnf("compaction: merge failed, concatenating: %v", err)
			finalSummary = strings.Join(summaries, "\n\n---\n\n")
		} else {
			finalSummary = merged
		}
	}

	// Build result: summary message + recent messages
	summaryMsg := session.Message{
		Role:    "assistant",
		Content: fmt.Sprintf("[Compacted conversation summary]:\n%s", finalSummary),
		TS:      time.Now().UnixMilli(),
	}

	result := make([]session.Message, 0, 1+len(recentMsgs))
	result = append(result, summaryMsg)
	result = append(result, recentMsgs...)

	logger.Infof("compaction: compressed %d messages (%d tokens) into summary (%d chars) + %d recent messages",
		len(oldMsgs), session.EstimateTokens(oldMsgs), len(finalSummary), len(recentMsgs))

	return result, nil
}

// findCompactionSplitPoint finds the index to split at, protecting the last keepN assistant turns.
func findCompactionSplitPoint(msgs []session.Message, keepN int) int {
	var assistantIndices []int
	for i, m := range msgs {
		if m.Role == "assistant" {
			assistantIndices = append(assistantIndices, i)
		}
	}
	if len(assistantIndices) <= keepN {
		return 0
	}

	splitAt := assistantIndices[len(assistantIndices)-keepN]
	// Walk back past preceding user message
	if splitAt > 0 && msgs[splitAt-1].Role == "user" {
		splitAt--
	}
	// Walk back past tool sequences
	for splitAt > 0 && msgs[splitAt-1].Role == "tool" {
		splitAt--
	}
	if splitAt > 0 && msgs[splitAt-1].Role == "assistant" && len(msgs[splitAt-1].ToolCalls) > 0 {
		splitAt--
	}

	return splitAt
}

// adaptiveChunkRatio computes the chunk ratio based on average message size.
func adaptiveChunkRatio(msgs []session.Message, maxTokens int) float64 {
	if len(msgs) == 0 {
		return BaseChunkRatio
	}
	totalTokens := session.EstimateTokens(msgs)
	avgTokens := float64(totalTokens) / float64(len(msgs))

	// Large average messages → smaller chunks for better summarization
	avgRatio := avgTokens / float64(maxTokens)
	ratio := BaseChunkRatio - (avgRatio * 0.5)

	if ratio < MinChunkRatio {
		ratio = MinChunkRatio
	}
	if ratio > BaseChunkRatio {
		ratio = BaseChunkRatio
	}
	return ratio
}

// chunkByTokenBudget splits messages into groups not exceeding chunkBudget tokens each.
func chunkByTokenBudget(msgs []session.Message, chunkBudget, maxTokens int) [][]session.Message {
	var chunks [][]session.Message
	var current []session.Message
	currentTokens := 0

	for _, m := range msgs {
		msgTokens := len(m.Content)/4 + 4
		for _, tc := range m.ToolCalls {
			msgTokens += len(tc.Function.Arguments)/4 + len(tc.Function.Name)/4
		}

		if currentTokens+msgTokens > chunkBudget && len(current) > 0 {
			chunks = append(chunks, current)
			current = nil
			currentTokens = 0
		}
		current = append(current, m)
		currentTokens += msgTokens
	}
	if len(current) > 0 {
		chunks = append(chunks, current)
	}
	return chunks
}

// stripToolResultDetails replaces tool result content with placeholders (DEC-051).
func stripToolResultDetails(msgs []session.Message) []session.Message {
	stripped := make([]session.Message, len(msgs))
	for i, m := range msgs {
		stripped[i] = m
		if m.Role == "tool" {
			toolName := m.Name
			if toolName == "" {
				toolName = "unknown"
			}
			stripped[i].Content = fmt.Sprintf("[tool result: %s]", toolName)
		}
	}
	return stripped
}

// summarizeChunkWithRetry calls the model to summarize a chunk, with retry logic.
func summarizeChunkWithRetry(ctx context.Context, client CompactorClient, model string, chunk []session.Message) (string, error) {
	stripped := stripToolResultDetails(chunk)

	// Build the text to summarize
	var sb strings.Builder
	for _, m := range stripped {
		fmt.Fprintf(&sb, "[%s]: %s\n", m.Role, m.Content)
	}

	var lastErr error
	for attempt := range compactionMaxRetries {
		if attempt > 0 {
			delay := compactionBaseDelay * time.Duration(math.Pow(2, float64(attempt-1)))
			if delay > compactionMaxDelay {
				delay = compactionMaxDelay
			}
			// Add jitter
			jitter := time.Duration(float64(delay) * compactionJitterFactor)
			delay += jitter

			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(delay):
			}
		}

		resp, err := client.Chat(ctx, openai.ChatCompletionRequest{
			Model: model,
			Messages: []openai.ChatCompletionMessage{
				{Role: "system", Content: compactionChunkPrompt},
				{Role: "user", Content: sb.String()},
			},
		})
		if err != nil {
			lastErr = err
			continue
		}
		if len(resp.Choices) == 0 {
			lastErr = fmt.Errorf("empty response from model")
			continue
		}
		return resp.Choices[0].Message.Content, nil
	}
	return "", fmt.Errorf("summarization failed after %d attempts: %w", compactionMaxRetries, lastErr)
}

// summarizeChunkPartial tries summarizing without oversized messages (progressive fallback).
func summarizeChunkPartial(ctx context.Context, client CompactorClient, model string, chunk []session.Message, maxTokens int) (string, error) {
	threshold := int(float64(maxTokens) * oversizedRatio)
	var filtered []session.Message
	for _, m := range chunk {
		tokens := len(m.Content)/4 + 4
		if tokens > threshold {
			// Replace with a note
			filtered = append(filtered, session.Message{
				Role:    m.Role,
				Content: fmt.Sprintf("[oversized %s message: %d tokens — excluded from summary]", m.Role, tokens),
				TS:      m.TS,
			})
		} else {
			filtered = append(filtered, m)
		}
	}
	if len(filtered) == 0 {
		return "[context was compacted — all messages were oversized]", nil
	}
	return summarizeChunkWithRetry(ctx, client, model, filtered)
}

// mergeSummaries combines multiple chunk summaries into one coherent summary.
func mergeSummaries(ctx context.Context, client CompactorClient, model string, summaries []string) (string, error) {
	var sb strings.Builder
	for i, s := range summaries {
		fmt.Fprintf(&sb, "--- Segment %d ---\n%s\n\n", i+1, s)
	}

	resp, err := client.Chat(ctx, openai.ChatCompletionRequest{
		Model: model,
		Messages: []openai.ChatCompletionMessage{
			{Role: "system", Content: compactionMergePrompt},
			{Role: "user", Content: sb.String()},
		},
	})
	if err != nil {
		return "", err
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("empty response from model")
	}
	return resp.Choices[0].Message.Content, nil
}

// ──────────────────────────────────────────────────────────────────────
// Context Window Guard (REQ-401)
// ──────────────────────────────────────────────────────────────────────

// ContextWindowError is returned when the context window is too small.
type ContextWindowError struct {
	ModelTokens int
	Minimum     int
}

func (e *ContextWindowError) Error() string {
	return fmt.Sprintf("model context window too small: %d tokens (minimum %d required)", e.ModelTokens, e.Minimum)
}

// ValidateContextWindow checks if the resolved context window meets minimum requirements.
// Returns an error if below ContextWindowMinimum; logs a warning if below ContextWindowWarning.
func ValidateContextWindow(logger *zap.SugaredLogger, contextTokens int) error {
	if contextTokens > 0 && contextTokens < ContextWindowMinimum {
		return &ContextWindowError{ModelTokens: contextTokens, Minimum: ContextWindowMinimum}
	}
	if contextTokens > 0 && contextTokens < ContextWindowWarning {
		logger.Warnf("context window guard: model has only %d tokens (recommended minimum: %d)", contextTokens, ContextWindowWarning)
	}
	return nil
}

// ResolveContextWindow determines the effective context window size.
// Priority: perModelConfig > modelDefault > agentDefault > globalFallback (128K).
func ResolveContextWindow(perModelConfig, agentDefault int) int {
	if perModelConfig > 0 {
		return perModelConfig
	}
	if agentDefault > 0 {
		return agentDefault
	}
	return 128_000
}

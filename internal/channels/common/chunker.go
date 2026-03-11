// Package common – chunker.go provides code-fence-aware message splitting.
//
// Unlike the simple SplitMessage, SmartChunk respects Markdown structure:
//   - Never splits inside a fenced code block (``` or ~~~).
//   - Prefers to break at paragraph boundaries (double newline).
//   - Falls back to single newline, then sentence boundary, then hard cut.
//   - If a code fence is split across chunks, the outgoing chunk is closed
//     with the fence and the next chunk is reopened with the same fence+lang.
package common

import "strings"

// SmartChunk splits text into pieces of at most maxLen bytes, respecting
// Markdown code fences (``` / ~~~) and preferring structural break points.
//
// Break-point priority (highest first):
//  1. Paragraph boundary (\n\n)
//  2. Newline (\n)
//  3. Sentence-ending punctuation followed by a space (". ", "! ", "? ")
//  4. Space
//  5. Hard cut at maxLen
//
// Code-fence awareness:
//   - If a chunk boundary falls inside a fenced code block, the outgoing chunk
//     is terminated with a closing fence and the next chunk begins with the
//     original opening fence (including the info-string / language tag).
func SmartChunk(text string, maxLen int) []string {
	if maxLen <= 0 {
		maxLen = 4096
	}
	if len(text) <= maxLen {
		return []string{text}
	}

	var parts []string
	for len(text) > 0 {
		if len(text) <= maxLen {
			parts = append(parts, text)
			break
		}

		cut := findBreakPoint(text, maxLen)
		parts = append(parts, text[:cut])
		text = text[cut:]
	}

	return healFences(parts)
}

// findBreakPoint returns the best byte offset in text[0:maxLen] to cut.
func findBreakPoint(text string, maxLen int) int {
	window := text[:maxLen]

	// 1. Paragraph boundary (\n\n) — search backward from maxLen.
	if idx := strings.LastIndex(window, "\n\n"); idx > 0 {
		return idx + 2 // include the double newline in this chunk
	}

	// 2. Single newline — search backward up to 400 chars from cut.
	searchFrom := maxLen - 400
	if searchFrom < 0 {
		searchFrom = 0
	}
	if idx := strings.LastIndex(window[searchFrom:], "\n"); idx >= 0 {
		return searchFrom + idx + 1
	}

	// 3. Sentence boundary (". " / "! " / "? ") — backward up to 400 chars.
	for i := maxLen - 1; i > searchFrom; i-- {
		if i+1 < len(text) && text[i+1] == ' ' {
			if text[i] == '.' || text[i] == '!' || text[i] == '?' {
				return i + 1 // include the punctuation, not the space
			}
		}
	}

	// 4. Space — backward up to 400 chars.
	if idx := strings.LastIndex(window[searchFrom:], " "); idx >= 0 {
		return searchFrom + idx + 1
	}

	// 5. Hard cut.
	return maxLen
}

// healFences post-processes chunks to close and reopen Markdown fences that
// span chunk boundaries.
func healFences(parts []string) []string {
	if len(parts) <= 1 {
		return parts
	}

	var openFence string // "" = not inside a fence; e.g. "```go"
	out := make([]string, 0, len(parts))

	for i, part := range parts {
		piece := part

		// If we are continuing an open fence from the previous chunk, prepend.
		if openFence != "" {
			piece = openFence + "\n" + piece
		}

		// Walk through the piece and track fence state.
		openFence = trackFence(piece, "")

		// If we end this chunk inside a fence and there's a next chunk, close it.
		if openFence != "" && i < len(parts)-1 {
			closeMark := fenceMark(openFence)
			piece = piece + "\n" + closeMark
		}

		out = append(out, piece)
	}
	return out
}

// trackFence scans text line-by-line, toggling the open-fence state.
// Returns the fence header (e.g. "```go") if we end inside a code block, or ""
// if we end outside.
func trackFence(text, initial string) string {
	open := initial
	lines := strings.Split(text, "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if open == "" {
			// Not inside a fence — check for an opening fence.
			if hdr, ok := parseFenceOpen(trimmed); ok {
				open = hdr
			}
		} else {
			// Inside a fence — check for a matching close.
			if isFenceClose(trimmed, open) {
				open = ""
			}
		}
	}
	return open
}

// parseFenceOpen detects ``` or ~~~ at the start of a trimmed line.
// Returns the full fence header (e.g. "```go", "~~~") and true.
func parseFenceOpen(trimmed string) (string, bool) {
	if strings.HasPrefix(trimmed, "```") {
		return trimmed, true
	}
	if strings.HasPrefix(trimmed, "~~~") {
		return trimmed, true
	}
	return "", false
}

// isFenceClose returns true if trimmed is a closing fence matching the open header.
// The closing fence must use the same character (backtick or tilde) and be at
// least as long as the opening run, with no info-string.
func isFenceClose(trimmed, openHeader string) bool {
	mark := fenceMark(openHeader)
	if strings.HasPrefix(trimmed, mark) {
		rest := strings.TrimLeft(trimmed[len(mark):], string(mark[0]))
		// After removing all fence chars, there must be nothing (or only spaces).
		return strings.TrimSpace(rest) == ""
	}
	return false
}

// fenceMark extracts just the fence characters from a header (e.g. "```go" → "```").
func fenceMark(header string) string {
	ch := header[0]
	i := 0
	for i < len(header) && header[i] == ch {
		i++
	}
	return header[:i]
}

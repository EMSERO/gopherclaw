package eidetic

import "math"

// MMR re-ranks search results using Maximal Marginal Relevance to balance
// relevance against diversity.  lambda controls the trade-off:
//   - lambda=1.0 → pure relevance (no diversity penalty)
//   - lambda=0.0 → pure diversity
//   - lambda=0.5 → balanced (recommended default)
//
// Results must already be sorted by Relevance descending.  The function
// returns a new slice (up to maxResults) with diverse, relevant entries.
// If results have no embeddings or len ≤ 1, the original order is preserved.
func MMR(results []MemoryEntry, lambda float64, maxResults int) []MemoryEntry {
	if len(results) <= 1 || maxResults <= 0 {
		return results
	}
	if maxResults > len(results) {
		maxResults = len(results)
	}

	selected := make([]MemoryEntry, 0, maxResults)
	remaining := make([]int, len(results)) // indices into results
	for i := range remaining {
		remaining[i] = i
	}

	// Always pick the most relevant first.
	selected = append(selected, results[0])
	remaining = remaining[1:] // drop index 0

	for len(selected) < maxResults && len(remaining) > 0 {
		bestIdx := -1
		bestScore := -math.MaxFloat64

		for _, ri := range remaining {
			candidate := results[ri]
			relevance := candidate.Relevance

			// Max similarity to any already-selected entry.
			maxSim := 0.0
			for _, s := range selected {
				sim := contentSimilarity(candidate.Content, s.Content)
				if sim > maxSim {
					maxSim = sim
				}
			}

			// MMR score = λ · relevance − (1−λ) · maxSimilarity
			score := lambda*relevance - (1-lambda)*maxSim
			if score > bestScore {
				bestScore = score
				bestIdx = ri
			}
		}

		if bestIdx < 0 {
			break
		}
		selected = append(selected, results[bestIdx])
		remaining = removeVal(remaining, bestIdx)
	}

	return selected
}

// contentSimilarity computes a simple Jaccard similarity between two texts
// based on word overlap.  This is a lightweight proxy when we don't have
// embedding vectors for the results — good enough for deduplication.
func contentSimilarity(a, b string) float64 {
	wordsA := wordSet(a)
	wordsB := wordSet(b)
	if len(wordsA) == 0 || len(wordsB) == 0 {
		return 0
	}

	intersection := 0
	for w := range wordsA {
		if wordsB[w] {
			intersection++
		}
	}
	union := len(wordsA) + len(wordsB) - intersection
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

// wordSet splits text into a set of lowercase words.
func wordSet(text string) map[string]bool {
	set := make(map[string]bool)
	start := -1
	for i, c := range text {
		if isWordChar(c) {
			if start < 0 {
				start = i
			}
		} else if start >= 0 {
			w := toLower(text[start:i])
			if len(w) > 1 { // skip single-char noise
				set[w] = true
			}
			start = -1
		}
	}
	if start >= 0 {
		w := toLower(text[start:])
		if len(w) > 1 {
			set[w] = true
		}
	}
	return set
}

func isWordChar(c rune) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_'
}

func toLower(s string) string {
	b := make([]byte, len(s))
	for i := range s {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b[i] = c
	}
	return string(b)
}

func removeVal(s []int, val int) []int {
	for i, v := range s {
		if v == val {
			return append(s[:i], s[i+1:]...)
		}
	}
	return s
}

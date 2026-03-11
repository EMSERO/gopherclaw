package eidetic

import (
	"testing"
	"time"
)

func TestMMR_EmptyAndSingle(t *testing.T) {
	// Empty input returns empty.
	got := MMR(nil, 0.7, 5)
	if len(got) != 0 {
		t.Errorf("expected empty, got %d", len(got))
	}

	// Single entry returns as-is.
	single := []MemoryEntry{{Content: "hello", Relevance: 0.9}}
	got = MMR(single, 0.7, 5)
	if len(got) != 1 || got[0].Content != "hello" {
		t.Errorf("expected single entry preserved")
	}
}

func TestMMR_DiversifiesResults(t *testing.T) {
	now := time.Now()
	results := []MemoryEntry{
		{Content: "the quick brown fox jumps over the lazy dog", Relevance: 0.95, Timestamp: now},
		{Content: "the quick brown fox runs over the lazy dog", Relevance: 0.90, Timestamp: now},     // near-duplicate of first
		{Content: "user prefers dark mode in all applications", Relevance: 0.85, Timestamp: now},      // diverse
		{Content: "the quick brown fox leaps over the lazy dog", Relevance: 0.80, Timestamp: now},     // near-duplicate of first
		{Content: "deployment pipeline uses docker and kubernetes", Relevance: 0.75, Timestamp: now},   // diverse
	}

	got := MMR(results, 0.7, 3)
	if len(got) != 3 {
		t.Fatalf("expected 3 results, got %d", len(got))
	}

	// First result should always be the highest relevance.
	if got[0].Relevance != 0.95 {
		t.Errorf("first result should be highest relevance, got %.2f", got[0].Relevance)
	}

	// With MMR, the diverse entries should be preferred over near-duplicates.
	// The "dark mode" and "docker" entries should appear before the fox duplicates.
	contents := make(map[string]bool)
	for _, r := range got {
		contents[r.Content] = true
	}
	if !contents["user prefers dark mode in all applications"] {
		t.Error("expected diverse entry 'dark mode' to be selected by MMR")
	}
}

func TestMMR_RespectsMaxResults(t *testing.T) {
	results := make([]MemoryEntry, 10)
	for i := range results {
		results[i] = MemoryEntry{Content: "entry", Relevance: float64(10-i) / 10}
	}
	got := MMR(results, 0.7, 3)
	if len(got) != 3 {
		t.Errorf("expected 3 results, got %d", len(got))
	}
}

func TestContentSimilarity(t *testing.T) {
	// Identical strings should have similarity 1.0.
	sim := contentSimilarity("hello world foo", "hello world foo")
	if sim != 1.0 {
		t.Errorf("expected 1.0 for identical, got %.2f", sim)
	}

	// Completely different strings should have similarity 0.0.
	sim = contentSimilarity("hello world", "kubernetes docker")
	if sim != 0.0 {
		t.Errorf("expected 0.0 for disjoint, got %.2f", sim)
	}

	// Partial overlap should be between 0 and 1.
	sim = contentSimilarity("the quick brown fox", "the slow brown cat")
	if sim <= 0 || sim >= 1 {
		t.Errorf("expected partial similarity, got %.2f", sim)
	}
}

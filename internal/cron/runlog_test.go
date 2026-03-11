package cron

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAppendRunLog(t *testing.T) {
	dir := t.TempDir()
	entry := RunLogEntry{
		TS:         time.Now().UnixMilli(),
		JobID:      "test-job-1",
		Action:     "finished",
		Status:     "ok",
		Summary:    "completed successfully",
		RunAtMs:    time.Now().UnixMilli(),
		DurationMs: 1500,
	}

	if err := AppendRunLog(testLogger(), dir, entry); err != nil {
		t.Fatalf("AppendRunLog: %v", err)
	}

	// Check file exists
	path := runLogPath(dir, "test-job-1")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("run log file not created: %v", err)
	}

	// Append another
	entry2 := entry
	entry2.Status = "error"
	entry2.Error = "timeout"
	if err := AppendRunLog(testLogger(), dir, entry2); err != nil {
		t.Fatalf("AppendRunLog 2: %v", err)
	}
}

func TestReadRunLogPage_Basic(t *testing.T) {
	dir := t.TempDir()

	// Append 20 entries
	for i := range 20 {
		entry := RunLogEntry{
			TS:     int64(1000 + i),
			JobID:  "job-1",
			Action: "finished",
			Status: "ok",
		}
		if i%3 == 0 {
			entry.Status = "error"
			entry.Error = "some error"
		}
		if err := AppendRunLog(testLogger(), dir, entry); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	// Read all (default: desc, limit 50)
	page, err := ReadRunLogPage(dir, "job-1", RunLogPageOpts{})
	if err != nil {
		t.Fatalf("ReadRunLogPage: %v", err)
	}
	if page.Total != 20 {
		t.Errorf("Total = %d, want 20", page.Total)
	}
	if len(page.Entries) != 20 {
		t.Errorf("Entries = %d, want 20", len(page.Entries))
	}
	// Default sort: desc
	if page.Entries[0].TS < page.Entries[1].TS {
		t.Error("expected descending sort order")
	}
}

func TestReadRunLogPage_Pagination(t *testing.T) {
	dir := t.TempDir()
	for i := range 15 {
		if err := AppendRunLog(testLogger(), dir, RunLogEntry{TS: int64(i), JobID: "job-2", Action: "finished", Status: "ok"}); err != nil {
			t.Fatal(err)
		}
	}

	// Page 1: limit 5, offset 0
	p1, err := ReadRunLogPage(dir, "job-2", RunLogPageOpts{Limit: 5, Offset: 0})
	if err != nil {
		t.Fatal(err)
	}
	if len(p1.Entries) != 5 {
		t.Errorf("page 1: %d entries, want 5", len(p1.Entries))
	}
	if !p1.HasMore {
		t.Error("page 1: should have more")
	}

	// Page 3: limit 5, offset 10
	p3, err := ReadRunLogPage(dir, "job-2", RunLogPageOpts{Limit: 5, Offset: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(p3.Entries) != 5 {
		t.Errorf("page 3: %d entries, want 5", len(p3.Entries))
	}
	if p3.HasMore {
		t.Error("page 3: should not have more")
	}
}

func TestReadRunLogPage_StatusFilter(t *testing.T) {
	dir := t.TempDir()
	for i := range 10 {
		status := "ok"
		if i%2 == 0 {
			status = "error"
		}
		if err := AppendRunLog(testLogger(), dir, RunLogEntry{TS: int64(i), JobID: "job-3", Action: "finished", Status: status, Error: "err msg"}); err != nil {
			t.Fatal(err)
		}
	}

	page, err := ReadRunLogPage(dir, "job-3", RunLogPageOpts{Status: "error"})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 5 {
		t.Errorf("error filter: Total=%d, want 5", page.Total)
	}
	for _, e := range page.Entries {
		if e.Status != "error" {
			t.Errorf("unexpected status %q in error filter result", e.Status)
		}
	}
}

func TestReadRunLogPage_TextQuery(t *testing.T) {
	dir := t.TempDir()
	AppendRunLog(testLogger(), dir, RunLogEntry{TS: 1, JobID: "job-4", Action: "finished", Status: "ok", Summary: "deployed to production"})
	AppendRunLog(testLogger(), dir, RunLogEntry{TS: 2, JobID: "job-4", Action: "finished", Status: "error", Error: "timeout on staging"})
	AppendRunLog(testLogger(), dir, RunLogEntry{TS: 3, JobID: "job-4", Action: "finished", Status: "ok", Summary: "backup completed"})

	page, err := ReadRunLogPage(dir, "job-4", RunLogPageOpts{Query: "production"})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 {
		t.Errorf("query 'production': Total=%d, want 1", page.Total)
	}

	page, err = ReadRunLogPage(dir, "job-4", RunLogPageOpts{Query: "staging"})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 {
		t.Errorf("query 'staging': Total=%d, want 1", page.Total)
	}
}

func TestReadRunLogPage_AscSort(t *testing.T) {
	dir := t.TempDir()
	for i := range 5 {
		AppendRunLog(testLogger(), dir, RunLogEntry{TS: int64(i * 100), JobID: "job-5", Action: "finished", Status: "ok"})
	}

	page, err := ReadRunLogPage(dir, "job-5", RunLogPageOpts{SortDir: "asc"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(page.Entries); i++ {
		if page.Entries[i].TS < page.Entries[i-1].TS {
			t.Error("expected ascending sort order")
		}
	}
}

func TestReadRunLogPage_NonExistentJob(t *testing.T) {
	dir := t.TempDir()
	page, err := ReadRunLogPage(dir, "nonexistent", RunLogPageOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 0 || len(page.Entries) != 0 {
		t.Error("expected empty result for nonexistent job")
	}
}

func TestPruneIfNeeded(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.jsonl")

	// Write 100 lines
	f, _ := os.Create(path)
	for i := range 100 {
		f.WriteString(strings.Repeat("x", 100) + "\n")
		_ = i
	}
	f.Close()

	// Force prune (set maxBytes very low)
	if err := PruneIfNeeded(path, 1, 10); err != nil {
		t.Fatalf("PruneIfNeeded: %v", err)
	}

	// Check that file was pruned
	data, _ := os.ReadFile(path)
	lines := strings.Count(string(data), "\n")
	if lines > 11 { // 10 + possible trailing newline
		t.Errorf("after prune: %d lines, want <= 10", lines)
	}
}

func TestPruneIfNeeded_SmallFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "small.jsonl")

	f, _ := os.Create(path)
	f.WriteString(`{"ts":1}` + "\n")
	f.Close()

	// Should be a no-op
	if err := PruneIfNeeded(path, maxRunLogBytes, maxRunLogLines); err != nil {
		t.Fatal(err)
	}
}

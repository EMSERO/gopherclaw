// Package cron — runlog.go implements persistent cron run logging (REQ-430).
//
// After each cron job completes, a structured entry is appended to a per-job
// JSONL file at <agent>/cron/runs/<jobId>.jsonl.  Supports paginated reads
// with filtering, sorting, and auto-pruning when files exceed 2MB / 2000 lines.
package cron

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.uber.org/zap"
)

const (
	runLogDir      = "runs"
	maxRunLogBytes = 2 * 1024 * 1024 // 2MB
	maxRunLogLines = 2000
)

// RunLogEntry is a single run history record.
type RunLogEntry struct {
	TS             int64  `json:"ts"`
	JobID          string `json:"jobId"`
	Action         string `json:"action"` // "finished"
	Status         string `json:"status"` // "ok", "error", "skipped"
	Error          string `json:"error,omitempty"`
	Summary        string `json:"summary,omitempty"`
	Delivered      bool   `json:"delivered,omitempty"`
	DeliveryStatus string `json:"deliveryStatus,omitempty"`
	DeliveryError  string `json:"deliveryError,omitempty"`
	SessionID      string `json:"sessionId,omitempty"`
	SessionKey     string `json:"sessionKey,omitempty"`
	RunAtMs        int64  `json:"runAtMs"`
	DurationMs     int64  `json:"durationMs"`
	NextRunAtMs    int64  `json:"nextRunAtMs,omitempty"`
	Model          string `json:"model,omitempty"`
	Provider       string `json:"provider,omitempty"`
	InputTokens    int    `json:"inputTokens,omitempty"`
	OutputTokens   int    `json:"outputTokens,omitempty"`
}

// RunLogPageOpts controls paginated reads.
type RunLogPageOpts struct {
	Limit          int    // 1-200, default 50
	Offset         int    // default 0
	Status         string // "all", "ok", "error", "skipped"
	DeliveryStatus string // "" = all
	SortDir        string // "asc" or "desc" (default "desc")
	Query          string // text search in summary/error
}

// RunLogPage is a paginated result.
type RunLogPage struct {
	Entries []RunLogEntry `json:"entries"`
	Total   int           `json:"total"` // total matching entries
	HasMore bool          `json:"hasMore"`
}

// runLogPath returns the JSONL path for a job's run history.
func runLogPath(dir, jobID string) string {
	return filepath.Join(dir, runLogDir, jobID+".jsonl")
}

// AppendRunLog appends a single entry to the job's run log file.
func AppendRunLog(logger *zap.SugaredLogger, dir string, entry RunLogEntry) error {
	path := runLogPath(dir, entry.JobID)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("create run log dir: %w", err)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("open run log: %w", err)
	}
	defer func() { _ = f.Close() }()

	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("marshal entry: %w", err)
	}
	data = append(data, '\n')

	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write entry: %w", err)
	}

	// Auto-prune if needed (best-effort, non-blocking)
	go func() {
		if err := PruneIfNeeded(path, maxRunLogBytes, maxRunLogLines); err != nil {
			logger.Debugf("cron: run log prune failed for %s: %v", entry.JobID, err)
		}
	}()

	return nil
}

// ReadRunLogPage reads paginated run log entries from a job's JSONL file.
func ReadRunLogPage(dir, jobID string, opts RunLogPageOpts) (RunLogPage, error) {
	if opts.Limit <= 0 {
		opts.Limit = 50
	}
	if opts.Limit > 200 {
		opts.Limit = 200
	}
	if opts.SortDir == "" {
		opts.SortDir = "desc"
	}

	path := runLogPath(dir, jobID)
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return RunLogPage{}, nil
	}
	if err != nil {
		return RunLogPage{}, err
	}
	defer func() { _ = f.Close() }()

	// Read all entries
	var all []RunLogEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 256*1024), 256*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e RunLogEntry
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		all = append(all, e)
	}
	if err := sc.Err(); err != nil {
		return RunLogPage{}, err
	}

	// Filter
	filtered := filterEntries(all, opts)

	// Sort
	if opts.SortDir == "asc" {
		sort.Slice(filtered, func(i, j int) bool { return filtered[i].TS < filtered[j].TS })
	} else {
		sort.Slice(filtered, func(i, j int) bool { return filtered[i].TS > filtered[j].TS })
	}

	total := len(filtered)

	// Paginate
	start := opts.Offset
	if start < 0 {
		start = 0
	}
	if start > total {
		return RunLogPage{Total: total}, nil
	}
	end := start + opts.Limit
	if end > total {
		end = total
	}

	return RunLogPage{
		Entries: filtered[start:end],
		Total:   total,
		HasMore: end < total,
	}, nil
}

// filterEntries applies status, delivery, and text query filters.
func filterEntries(entries []RunLogEntry, opts RunLogPageOpts) []RunLogEntry {
	result := make([]RunLogEntry, 0, len(entries))
	for _, e := range entries {
		// Status filter
		if opts.Status != "" && opts.Status != "all" && e.Status != opts.Status {
			continue
		}
		// Delivery status filter
		if opts.DeliveryStatus != "" && e.DeliveryStatus != opts.DeliveryStatus {
			continue
		}
		// Text query
		if opts.Query != "" {
			q := strings.ToLower(opts.Query)
			if !strings.Contains(strings.ToLower(e.Summary), q) &&
				!strings.Contains(strings.ToLower(e.Error), q) &&
				!strings.Contains(strings.ToLower(e.SessionKey), q) {
				continue
			}
		}
		result = append(result, e)
	}
	return result
}

// PruneIfNeeded trims a JSONL file to keepLines entries if it exceeds maxBytes.
func PruneIfNeeded(path string, maxBytes int64, keepLines int) error {
	info, err := os.Stat(path)
	if err != nil {
		return nil // file doesn't exist or can't stat — nothing to prune
	}
	if info.Size() <= maxBytes {
		return nil
	}

	f, err := os.Open(path)
	if err != nil {
		return err
	}

	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 256*1024), 256*1024)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		_ = f.Close()
		return err
	}
	_ = f.Close()

	if len(lines) <= keepLines {
		return nil
	}

	// Keep the last keepLines entries
	lines = lines[len(lines)-keepLines:]

	// Rewrite atomically
	tmp := path + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	for _, line := range lines {
		if _, err := fmt.Fprintln(out, line); err != nil {
			_ = out.Close()
			_ = os.Remove(tmp)
			return err
		}
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}

	return os.Rename(tmp, path)
}

// runLogDir returns the runs directory for this manager.
func (m *Manager) RunLogDir() string {
	return filepath.Join(m.dir, runLogDir)
}

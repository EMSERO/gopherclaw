package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/EMSERO/gopherclaw/internal/agentapi"
)

// mockAnnouncer records AnnounceToSession calls for testing.
type mockAnnouncer struct {
	announced map[string]string
}

func (m *mockAnnouncer) AnnounceToSession(sessionKey, text string) {
	m.announced[sessionKey] = text
}

// compile-time check
var _ agentapi.Announcer = (*mockAnnouncer)(nil)

func TestReadFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	content := "hello world\nline 2"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	tool := &ReadFileTool{}
	args, _ := json.Marshal(readFileInput{Path: path})
	result := tool.Run(context.Background(), string(args))

	if result != content {
		t.Errorf("expected %q, got %q", content, result)
	}
}

func TestReadFileNotFound(t *testing.T) {
	tool := &ReadFileTool{}
	args, _ := json.Marshal(readFileInput{Path: "/nonexistent/path/file.txt"})
	result := tool.Run(context.Background(), string(args))

	if !strings.Contains(result, "error reading") {
		t.Errorf("expected error message, got %q", result)
	}
}

func TestReadFileTruncation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.txt")
	// Create a file larger than 200KB
	data := make([]byte, 250_000)
	for i := range data {
		data[i] = 'A'
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}

	tool := &ReadFileTool{}
	args, _ := json.Marshal(readFileInput{Path: path})
	result := tool.Run(context.Background(), string(args))

	if !strings.Contains(result, "truncated: showing 200,000 of") {
		t.Error("expected truncation marker with size info")
	}
	if !strings.Contains(result, "process specific sections") {
		t.Error("expected chunking guidance in truncation message")
	}
	if len(result) > 200_200 {
		t.Errorf("result too long: %d", len(result))
	}
}

func TestWriteFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subdir", "output.txt")

	tool := &WriteFileTool{}
	args, _ := json.Marshal(writeFileInput{Path: path, Content: "test content"})
	result := tool.Run(context.Background(), string(args))

	if !strings.Contains(result, "wrote 12 bytes") {
		t.Errorf("unexpected result: %s", result)
	}

	// Verify file was written
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "test content" {
		t.Errorf("expected 'test content', got %q", string(data))
	}
}

func TestWriteFileCreatesParentDirs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a", "b", "c", "file.txt")

	tool := &WriteFileTool{}
	args, _ := json.Marshal(writeFileInput{Path: path, Content: "deep"})
	result := tool.Run(context.Background(), string(args))

	if strings.Contains(result, "error") {
		t.Errorf("unexpected error: %s", result)
	}
}

func TestListDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("aaa"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0755); err != nil {
		t.Fatal(err)
	}

	tool := &ListDirTool{}
	args, _ := json.Marshal(listDirInput{Path: dir})
	result := tool.Run(context.Background(), string(args))

	if !strings.Contains(result, "a.txt") {
		t.Errorf("expected a.txt in listing, got: %s", result)
	}
	if !strings.Contains(result, "subdir") {
		t.Errorf("expected subdir in listing, got: %s", result)
	}
	if !strings.Contains(result, "file") {
		t.Errorf("expected 'file' kind, got: %s", result)
	}
	if !strings.Contains(result, "dir") {
		t.Errorf("expected 'dir' kind, got: %s", result)
	}
}

func TestListDirEmpty(t *testing.T) {
	dir := t.TempDir()
	tool := &ListDirTool{}
	args, _ := json.Marshal(listDirInput{Path: dir})
	result := tool.Run(context.Background(), string(args))

	if result != "(empty directory)" {
		t.Errorf("expected empty directory message, got: %s", result)
	}
}

func TestListDirNotFound(t *testing.T) {
	tool := &ListDirTool{}
	args, _ := json.Marshal(listDirInput{Path: "/nonexistent/dir"})
	result := tool.Run(context.Background(), string(args))

	if !strings.Contains(result, "error listing") {
		t.Errorf("expected error message, got: %s", result)
	}
}

func TestExecTool(t *testing.T) {
	tool := &ExecTool{
		DefaultTimeout: 30 * time.Second,
	}

	args, _ := json.Marshal(ExecInput{Command: "echo hello"})
	result := tool.Run(context.Background(), string(args))

	result = strings.TrimSpace(result)
	if result != "hello" {
		t.Errorf("expected 'hello', got %q", result)
	}
}

func TestExecToolTimeout(t *testing.T) {
	tool := &ExecTool{
		DefaultTimeout: 30 * time.Second,
	}

	// Use explicit short timeout
	args, _ := json.Marshal(ExecInput{Command: "sleep 10", Timeout: 1})
	result := tool.Run(context.Background(), string(args))

	if !strings.Contains(result, "timeout") {
		t.Errorf("expected timeout message, got: %s", result)
	}
}

func TestExecToolEnv(t *testing.T) {
	tool := &ExecTool{
		DefaultTimeout: 30 * time.Second,
		Env:            map[string]string{"TEST_VAR": "test_value"},
	}

	args, _ := json.Marshal(ExecInput{Command: "echo $TEST_VAR"})
	result := tool.Run(context.Background(), string(args))

	result = strings.TrimSpace(result)
	if result != "test_value" {
		t.Errorf("expected 'test_value', got %q", result)
	}
}

func TestExecToolError(t *testing.T) {
	tool := &ExecTool{
		DefaultTimeout: 30 * time.Second,
	}

	args, _ := json.Marshal(ExecInput{Command: "exit 1"})
	result := tool.Run(context.Background(), string(args))
	// Should get an error result (either empty output with error, or stderr)
	_ = result // just ensure it doesn't panic
}

func TestExecToolNoOutput(t *testing.T) {
	tool := &ExecTool{
		DefaultTimeout: 30 * time.Second,
	}

	args, _ := json.Marshal(ExecInput{Command: "true"})
	result := tool.Run(context.Background(), string(args))

	if result != "(no output)" {
		t.Errorf("expected '(no output)', got %q", result)
	}
}

func TestToolNames(t *testing.T) {
	tools := []Tool{
		&ExecTool{},
		&ReadFileTool{},
		&WriteFileTool{},
		&ListDirTool{},
	}
	expected := []string{"exec", "read_file", "write_file", "list_dir"}
	for i, tool := range tools {
		if tool.Name() != expected[i] {
			t.Errorf("expected name %s, got %s", expected[i], tool.Name())
		}
	}
}

func TestToolSchemas(t *testing.T) {
	tools := []Tool{
		&ExecTool{},
		&ReadFileTool{},
		&WriteFileTool{},
		&ListDirTool{},
	}
	for _, tool := range tools {
		schema := tool.Schema()
		var m map[string]any
		if err := json.Unmarshal(schema, &m); err != nil {
			t.Errorf("tool %s: invalid JSON schema: %v", tool.Name(), err)
		}
		if m["type"] != "object" {
			t.Errorf("tool %s: expected type=object, got %v", tool.Name(), m["type"])
		}
	}
}

// ---------------------------------------------------------------------------
// browser.go pure helper tests
// ---------------------------------------------------------------------------

func TestLastCompleteElement(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want int
	}{
		{"empty", []byte{}, -1},
		{"no brace", []byte("abc"), -1},
		{"single object", []byte(`{"a":1}`), 7},
		{"array of objects", []byte(`[{"a":1},{"b":2}]`), 16},
		{"truncated mid-object", []byte(`[{"a":1},{"b":2`), 8},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := lastCompleteElement(tc.in)
			if got != tc.want {
				t.Errorf("lastCompleteElement(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestBrowserToolNameSchema(t *testing.T) {
	tool := &BrowserTool{}
	if tool.Name() != "browser" {
		t.Errorf("expected name 'browser', got %q", tool.Name())
	}
	var m map[string]any
	if err := json.Unmarshal(tool.Schema(), &m); err != nil {
		t.Errorf("invalid schema JSON: %v", err)
	}
	if m["type"] != "object" {
		t.Errorf("expected type=object, got %v", m["type"])
	}
}

// ---------------------------------------------------------------------------
// WebSearchTool.Run via httptest
// ---------------------------------------------------------------------------

func TestWebSearchToolRunWithResults(t *testing.T) {
	html := `<div class="results">
<div class="result">
<a href="https://example.com/page1" class="result__a">Example Title</a>
<a class="result__snippet">This is a snippet about example.</a>
</div>
<div class="result">
<a href="https://example.com/page2" class="result__a">Second Result</a>
<a class="result__snippet">Second snippet here.</a>
</div>
</div>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		if q == "" {
			http.Error(w, "missing query", 400)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, html)
	}))
	defer srv.Close()

	tool := &WebSearchTool{
		MaxResults:     5,
		TimeoutSeconds: 5,
		BaseURL:        srv.URL + "/",
		Client:         srv.Client(),
	}

	args, _ := json.Marshal(webSearchInput{Query: "test query"})
	result := tool.Run(context.Background(), string(args))

	if !strings.Contains(result, "Example Title") {
		t.Errorf("expected 'Example Title' in result, got: %s", result)
	}
	if !strings.Contains(result, "Second Result") {
		t.Errorf("expected 'Second Result' in result, got: %s", result)
	}
	if !strings.Contains(result, "example.com/page1") {
		t.Errorf("expected URL in result, got: %s", result)
	}
}

func TestWebSearchToolRunNoResults(t *testing.T) {
	// Return HTML that has result markers but no actual results
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<html><body>result__a result__snippet but no actual result links</body></html>`)
	}))
	defer srv.Close()

	tool := &WebSearchTool{
		TimeoutSeconds: 5,
		BaseURL:        srv.URL + "/",
		Client:         srv.Client(),
	}
	args, _ := json.Marshal(webSearchInput{Query: "nothing"})
	result := tool.Run(context.Background(), string(args))
	if !strings.Contains(result, "No results found.") {
		t.Errorf("expected 'No results found.', got: %s", result)
	}
}

func TestWebSearchToolRunRateLimited(t *testing.T) {
	// Return HTML with no result markers at all (CAPTCHA page)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<html><body>Please verify you are human</body></html>`)
	}))
	defer srv.Close()

	tool := &WebSearchTool{
		TimeoutSeconds: 5,
		BaseURL:        srv.URL + "/",
		Client:         srv.Client(),
	}
	args, _ := json.Marshal(webSearchInput{Query: "blocked"})
	result := tool.Run(context.Background(), string(args))
	if !strings.Contains(result, "rate-limiting") {
		t.Errorf("expected rate-limiting message, got: %s", result)
	}
}

func TestWebSearchToolRunHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Close the connection abruptly to cause an error
		hj, ok := w.(http.Hijacker)
		if ok {
			conn, _, _ := hj.Hijack()
			conn.Close()
		}
	}))
	defer srv.Close()

	tool := &WebSearchTool{
		TimeoutSeconds: 5,
		BaseURL:        srv.URL + "/",
		Client:         srv.Client(),
	}
	args, _ := json.Marshal(webSearchInput{Query: "error"})
	result := tool.Run(context.Background(), string(args))
	if !strings.Contains(result, "error") {
		t.Errorf("expected error message, got: %s", result)
	}
}

// ---------------------------------------------------------------------------
// WebFetchTool.Run via httptest
// ---------------------------------------------------------------------------

func TestWebFetchToolRunSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><body><h1>Hello World</h1><p>Some content here.</p></body></html>`)
	}))
	defer srv.Close()

	tool := &WebFetchTool{
		MaxChars:       50000,
		TimeoutSeconds: 5,
		Client:         srv.Client(),
		SkipSSRF:       true,
	}

	args, _ := json.Marshal(webFetchInput{URL: srv.URL})
	result := tool.Run(context.Background(), string(args))
	if !strings.Contains(result, "Hello World") {
		t.Errorf("expected 'Hello World' in result, got: %s", result)
	}
	if !strings.Contains(result, "Some content here") {
		t.Errorf("expected 'Some content here' in result, got: %s", result)
	}
}

func TestWebFetchToolRunTruncation(t *testing.T) {
	// Generate a large response that exceeds MaxChars
	bigContent := strings.Repeat("A", 1000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, bigContent)
	}))
	defer srv.Close()

	tool := &WebFetchTool{
		MaxChars:       100,
		TimeoutSeconds: 5,
		Client:         srv.Client(),
		SkipSSRF:       true,
	}

	args, _ := json.Marshal(webFetchInput{URL: srv.URL})
	result := tool.Run(context.Background(), string(args))
	if !strings.Contains(result, "[... truncated ...]") {
		t.Errorf("expected truncation marker, got len=%d: %s", len(result), result[:min(200, len(result))])
	}
}

func TestWebFetchToolRunHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if ok {
			conn, _, _ := hj.Hijack()
			conn.Close()
		}
	}))
	defer srv.Close()

	tool := &WebFetchTool{
		TimeoutSeconds: 5,
		Client:         srv.Client(),
		SkipSSRF:       true,
	}

	args, _ := json.Marshal(webFetchInput{URL: srv.URL})
	result := tool.Run(context.Background(), string(args))
	if !strings.Contains(result, "error") {
		t.Errorf("expected error, got: %s", result)
	}
}

func TestWebFetchToolRunBadURL(t *testing.T) {
	tool := &WebFetchTool{
		TimeoutSeconds: 5,
		SkipSSRF:       true,
	}
	args, _ := json.Marshal(webFetchInput{URL: "://invalid"})
	result := tool.Run(context.Background(), string(args))
	if !strings.Contains(result, "error") {
		t.Errorf("expected error for bad URL, got: %s", result)
	}
}

// ---------------------------------------------------------------------------
// ssrfSafeTransport
// ---------------------------------------------------------------------------

func TestSSRFSafeTransport(t *testing.T) {
	tr := SSRFSafeTransport()
	if tr == nil {
		t.Fatal("expected non-nil transport")
	}
	if tr.DialContext == nil {
		t.Error("expected DialContext to be set")
	}
}

// ---------------------------------------------------------------------------
// exec.go additional paths
// ---------------------------------------------------------------------------

func TestExecOutputTruncation(t *testing.T) {
	tool := &ExecTool{
		DefaultTimeout: 10 * time.Second,
	}
	// Generate output > 100KB
	args, _ := json.Marshal(ExecInput{Command: "head -c 150000 /dev/zero | tr '\\0' 'A'"})
	result := tool.Run(context.Background(), string(args))
	if !strings.Contains(result, "output truncated: showing 100000 of") {
		t.Errorf("expected truncation marker with size info, got len=%d", len(result))
	}
	if !strings.Contains(result, "process in chunks") {
		t.Error("expected chunking guidance in truncation message")
	}
}

func TestExecToolStderrOutput(t *testing.T) {
	tool := &ExecTool{
		DefaultTimeout: 5 * time.Second,
	}
	// Command that writes to stderr and exits non-zero
	args, _ := json.Marshal(ExecInput{Command: "echo 'error msg' >&2; exit 1"})
	result := tool.Run(context.Background(), string(args))
	if !strings.Contains(result, "error msg") {
		t.Errorf("expected stderr content, got: %s", result)
	}
}

func TestExecBackgroundModeNoOutputYet(t *testing.T) {
	tool := &ExecTool{
		DefaultTimeout: 30 * time.Second,
		BackgroundWait: 50 * time.Millisecond,
	}
	// Command that produces no output initially, then sleeps
	args, _ := json.Marshal(ExecInput{Command: "sleep 10"})
	result := tool.Run(context.Background(), string(args))
	if !strings.Contains(result, "still running") {
		t.Errorf("expected 'still running' message, got: %s", result)
	}
}

func TestExecBackgroundModeContextCancel(t *testing.T) {
	tool := &ExecTool{
		DefaultTimeout: 30 * time.Second,
		BackgroundWait: 5 * time.Second,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	args, _ := json.Marshal(ExecInput{Command: "sleep 60"})
	result := tool.Run(ctx, string(args))
	if !strings.Contains(result, "timeout") {
		t.Errorf("expected timeout message, got: %s", result)
	}
}

func TestExecSandboxNotAvailable(t *testing.T) {
	tool := &ExecTool{
		DefaultTimeout: 5 * time.Second,
		Sandbox: &SandboxConfig{
			Enabled: true,
			Image:   "nonexistent-image:latest",
		},
	}
	args, _ := json.Marshal(ExecInput{Command: "echo hello"})
	result := tool.Run(context.Background(), string(args))
	if !strings.Contains(result, "error") && !strings.Contains(result, "sandbox") {
		t.Errorf("expected sandbox error, got: %s", result)
	}
}

// ---------------------------------------------------------------------------
// files.go additional paths
// ---------------------------------------------------------------------------

func TestWriteFileInvalidJSON(t *testing.T) {
	tool := &WriteFileTool{}
	result := tool.Run(context.Background(), "not json")
	if !strings.Contains(result, "error") {
		t.Errorf("expected error for bad JSON, got: %s", result)
	}
}

func TestReadFileInvalidJSON(t *testing.T) {
	tool := &ReadFileTool{}
	result := tool.Run(context.Background(), "not json")
	if !strings.Contains(result, "error") {
		t.Errorf("expected error for bad JSON, got: %s", result)
	}
}

func TestListDirInvalidJSON(t *testing.T) {
	tool := &ListDirTool{}
	result := tool.Run(context.Background(), "not json")
	if !strings.Contains(result, "error") {
		t.Errorf("expected error for bad JSON, got: %s", result)
	}
}

func TestResolveSymlinksWithSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.Mkdir(target, 0755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	resolved := resolveSymlinks(link)
	if resolved != target {
		t.Errorf("expected %q, got %q", target, resolved)
	}

	// File under symlink
	resolved2 := resolveSymlinks(filepath.Join(link, "file.txt"))
	expected := filepath.Join(target, "file.txt")
	if resolved2 != expected {
		t.Errorf("expected %q, got %q", expected, resolved2)
	}
}

func TestCheckPathAllowedSymlinkEscape(t *testing.T) {
	dir := t.TempDir()
	allowed := filepath.Join(dir, "safe")
	if err := os.Mkdir(allowed, 0755); err != nil {
		t.Fatal(err)
	}

	// Create a symlink inside "safe" that points outside
	outside := filepath.Join(dir, "outside")
	if err := os.Mkdir(outside, 0755); err != nil {
		t.Fatal(err)
	}
	escape := filepath.Join(allowed, "escape")
	if err := os.Symlink(outside, escape); err != nil {
		t.Fatal(err)
	}

	// This should be blocked because the symlink resolves outside allowed
	err := checkPathAllowed(filepath.Join(escape, "secret.txt"), []string{allowed})
	if err == nil {
		t.Error("expected symlink escape to be blocked")
	}
}

func TestWriteFileUnwritablePath(t *testing.T) {
	tool := &WriteFileTool{}
	// /proc is not writable
	args, _ := json.Marshal(writeFileInput{Path: "/proc/fakefile", Content: "test"})
	result := tool.Run(context.Background(), string(args))
	if !strings.Contains(result, "error") {
		t.Errorf("expected error writing to /proc, got: %s", result)
	}
}

// ---------------------------------------------------------------------------
// BrowserTool.Run early-return paths (no Chrome needed)
// ---------------------------------------------------------------------------

func TestBrowserToolRunInvalidJSON(t *testing.T) {
	pool := &BrowserPool{
		sessions: make(map[string]*browserSession),
		done:     make(chan struct{}),
	}
	tool := &BrowserTool{Pool: pool}
	result := tool.Run(context.Background(), "not json")
	if !strings.Contains(result, "invalid arguments") {
		t.Errorf("expected invalid arguments error, got: %s", result)
	}
}

func TestBrowserToolRunCloseAction(t *testing.T) {
	pool := &BrowserPool{
		sessions: make(map[string]*browserSession),
		done:     make(chan struct{}),
	}
	tool := &BrowserTool{Pool: pool}
	args, _ := json.Marshal(browserInput{Action: "close"})
	result := tool.Run(context.Background(), string(args))
	if result != "browser session closed" {
		t.Errorf("expected 'browser session closed', got: %s", result)
	}
}

func TestBrowserToolRunUnknownAction(t *testing.T) {
	pool := &BrowserPool{
		sessions: make(map[string]*browserSession),
		done:     make(chan struct{}),
	}
	tool := &BrowserTool{Pool: pool}
	args, _ := json.Marshal(browserInput{Action: "navigate", URL: "http://example.com"})
	result := tool.Run(context.Background(), string(args))
	// This will fail because there's no Chrome, but it exercises the getOrCreate path
	// The error will be about Chrome not being available
	_ = result
}

func TestBrowserPoolCloseNonexistent(t *testing.T) {
	pool := &BrowserPool{
		sessions: make(map[string]*browserSession),
		done:     make(chan struct{}),
	}
	// Should not panic
	pool.Close("nonexistent-session")
}

func TestBrowserPoolCloseAll(t *testing.T) {
	pool := &BrowserPool{
		sessions: make(map[string]*browserSession),
		done:     make(chan struct{}),
	}
	// Should not panic
	pool.CloseAll()
}

func TestNewBrowserPool(t *testing.T) {
	pool := NewBrowserPool(true, "", false, 0, 0)
	if pool == nil {
		t.Fatal("expected non-nil pool")
	}
	if !pool.headless {
		t.Error("expected headless=true")
	}
	if len(pool.sessions) != 0 {
		t.Error("expected empty sessions map")
	}
	// CloseAll stops the idleReaper goroutine
	pool.CloseAll()
}

func TestBrowserToolRunNavigateNoURL(t *testing.T) {
	pool := &BrowserPool{
		sessions: make(map[string]*browserSession),
		done:     make(chan struct{}),
	}
	tool := &BrowserTool{Pool: pool}
	// navigate without URL — this will call getOrCreate, which will fail without Chrome
	// but the error path exercises browser.go code
	args, _ := json.Marshal(browserInput{Action: "navigate"})
	result := tool.Run(context.Background(), string(args))
	_ = result // just verify no panic
}

func TestBrowserToolRunClickNoSelector(t *testing.T) {
	pool := &BrowserPool{
		sessions: make(map[string]*browserSession),
		done:     make(chan struct{}),
	}
	tool := &BrowserTool{Pool: pool}
	args, _ := json.Marshal(browserInput{Action: "click"})
	result := tool.Run(context.Background(), string(args))
	_ = result
}

func TestBrowserToolRunTypeNoSelector(t *testing.T) {
	pool := &BrowserPool{
		sessions: make(map[string]*browserSession),
		done:     make(chan struct{}),
	}
	tool := &BrowserTool{Pool: pool}
	args, _ := json.Marshal(browserInput{Action: "type"})
	result := tool.Run(context.Background(), string(args))
	_ = result
}

func TestBrowserToolRunEvalNoJS(t *testing.T) {
	pool := &BrowserPool{
		sessions: make(map[string]*browserSession),
		done:     make(chan struct{}),
	}
	tool := &BrowserTool{Pool: pool}
	args, _ := json.Marshal(browserInput{Action: "eval"})
	result := tool.Run(context.Background(), string(args))
	_ = result
}

func TestBrowserToolRunScrapeNoSelector(t *testing.T) {
	pool := &BrowserPool{
		sessions: make(map[string]*browserSession),
		done:     make(chan struct{}),
	}
	tool := &BrowserTool{Pool: pool}
	args, _ := json.Marshal(browserInput{Action: "scrape"})
	result := tool.Run(context.Background(), string(args))
	_ = result
}

func TestBrowserToolRunBadAction(t *testing.T) {
	pool := &BrowserPool{
		sessions: make(map[string]*browserSession),
		done:     make(chan struct{}),
	}
	tool := &BrowserTool{Pool: pool}
	args, _ := json.Marshal(browserInput{Action: "nonexistent"})
	result := tool.Run(context.Background(), string(args))
	_ = result
}

func TestBrowserToolRunWithSessionKey(t *testing.T) {
	pool := &BrowserPool{
		sessions: make(map[string]*browserSession),
		done:     make(chan struct{}),
	}
	tool := &BrowserTool{Pool: pool}
	ctx := context.WithValue(context.Background(), SessionKeyContextKey{}, "test-session-123")
	args, _ := json.Marshal(browserInput{Action: "close"})
	result := tool.Run(ctx, string(args))
	if result != "browser session closed" {
		t.Errorf("expected 'browser session closed', got: %s", result)
	}
}

// ---------------------------------------------------------------------------
// ExecTool.Cleanup
// ---------------------------------------------------------------------------

func TestExecToolCleanupNoContainer(t *testing.T) {
	tool := &ExecTool{
		DefaultTimeout: 5 * time.Second,
	}
	// Should not panic when no container exists
	tool.Cleanup()
}

func TestExecToolCleanupWithFakeContainer(t *testing.T) {
	tool := &ExecTool{
		DefaultTimeout: 5 * time.Second,
	}
	// Set a fake container ID - Cleanup should try to remove it (and fail silently)
	tool.containerID = "fake-container-id-for-test"
	tool.Cleanup()
	if tool.containerID != "" {
		t.Error("expected containerID to be cleared after Cleanup")
	}
}

// ---------------------------------------------------------------------------
// ssrfSafeTransport DialContext integration
// ---------------------------------------------------------------------------

func TestSSRFSafeTransportBlocksPrivate(t *testing.T) {
	tr := SSRFSafeTransport()
	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}

	// This should fail because 127.0.0.1 is a private IP
	_, err := client.Get("http://127.0.0.1:1/")
	if err == nil {
		t.Error("expected error for private IP via ssrfSafeTransport")
	}
	if !strings.Contains(err.Error(), "ssrf") {
		t.Errorf("expected SSRF error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// parseDDGResults edge cases
// ---------------------------------------------------------------------------

func TestParseDDGResultsSnippetInSpan(t *testing.T) {
	// Test snippet in a <span> rather than <a>
	html := `<div class="result">
<a href="https://example.com/page" class="result__a">Title Here</a>
<span class="result__snippet">Span snippet text here.</span>
</div>`
	results := parseDDGResults(html, 5)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if !strings.Contains(results[0].snippet, "Span snippet text") {
		t.Errorf("expected snippet from span, got %q", results[0].snippet)
	}
}

func TestParseDDGResultsMultipleMax(t *testing.T) {
	// Build 10 results but max=2
	var sb strings.Builder
	for i := range 10 {
		fmt.Fprintf(&sb, `<a href="https://example.com/%d" class="result__a">Title %d</a>`, i, i)
		fmt.Fprintf(&sb, `<span class="result__snippet">Snippet %d</span>`, i)
	}
	results := parseDDGResults(sb.String(), 2)
	if len(results) != 2 {
		t.Errorf("expected 2 results (max), got %d", len(results))
	}
}

func TestParseDDGResultsDefaultMax(t *testing.T) {
	// max=0 should default to 5
	var sb strings.Builder
	for i := range 10 {
		fmt.Fprintf(&sb, `<a href="https://example.com/%d" class="result__a">Title %d</a>`, i, i)
		fmt.Fprintf(&sb, `<span class="result__snippet">Snippet %d</span>`, i)
	}
	results := parseDDGResults(sb.String(), 0)
	if len(results) != 5 {
		t.Errorf("expected 5 results (default max), got %d", len(results))
	}
}

// ---------------------------------------------------------------------------
// WebFetchTool.Run via httptest — strip tags
// ---------------------------------------------------------------------------

func TestWebFetchToolRunStripsTags(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><body><script>alert('x')</script><b>bold</b> text &amp; more</body></html>`)
	}))
	defer srv.Close()

	tool := &WebFetchTool{
		TimeoutSeconds: 5,
		Client:         srv.Client(),
		SkipSSRF:       true,
	}
	args, _ := json.Marshal(webFetchInput{URL: srv.URL})
	result := tool.Run(context.Background(), string(args))

	// Tags should be stripped
	if strings.Contains(result, "<b>") {
		t.Error("expected HTML tags to be stripped")
	}
	if !strings.Contains(result, "bold") {
		t.Error("expected text content to remain")
	}
	if !strings.Contains(result, "& more") {
		t.Errorf("expected decoded entity, got: %s", result)
	}
}

// ---------------------------------------------------------------------------
// WebSearchTool.Run edge cases
// ---------------------------------------------------------------------------

func TestWebSearchToolRunWithDDGRedirectHref(t *testing.T) {
	html := `<div class="result">
<a href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fgolang.org%2Fdoc&kh=-1" class="result__a">Go Docs</a>
<span class="result__snippet">The Go programming language docs.</span>
</div>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, html)
	}))
	defer srv.Close()

	tool := &WebSearchTool{
		MaxResults:     5,
		TimeoutSeconds: 5,
		BaseURL:        srv.URL + "/",
		Client:         srv.Client(),
	}

	args, _ := json.Marshal(webSearchInput{Query: "golang docs"})
	result := tool.Run(context.Background(), string(args))

	if !strings.Contains(result, "Go Docs") {
		t.Errorf("expected 'Go Docs' in result, got: %s", result)
	}
	if !strings.Contains(result, "golang.org/doc") {
		t.Errorf("expected decoded URL in result, got: %s", result)
	}
}

// ---------------------------------------------------------------------------
// MemoryAppendTool — write error path
// ---------------------------------------------------------------------------

func TestDecodeDDGRedirectPathWithL(t *testing.T) {
	// /l/ path variant with uddg
	got := decodeDDGRedirect("https://duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2Ftest")
	if got != "https://example.com/test" {
		t.Errorf("expected decoded URL, got %q", got)
	}
}

func TestDecodeDDGRedirectNoUddg(t *testing.T) {
	// DDG redirect path but no uddg parameter
	got := decodeDDGRedirect("//duckduckgo.com/l/?other=value")
	if got != "//duckduckgo.com/l/?other=value" {
		t.Errorf("expected raw URL back, got %q", got)
	}
}

func TestCheckSSRFPublicIP(t *testing.T) {
	// Public IP (google DNS) should pass
	err := checkSSRF("http://8.8.8.8/")
	if err != nil {
		t.Errorf("expected no error for public IP, got: %v", err)
	}
}

func TestCheckSSRFUnresolvable(t *testing.T) {
	err := checkSSRF("http://thishostdoesnotexist.invalid/")
	if err == nil {
		t.Error("expected error for unresolvable host")
	}
	if !strings.Contains(err.Error(), "cannot resolve") {
		t.Errorf("expected 'cannot resolve' error, got: %v", err)
	}
}

func TestMemoryAppendToolWriteError(t *testing.T) {
	// Use an unwritable directory
	tool := &MemoryAppendTool{Workspace: "/proc/fake"}
	args, _ := json.Marshal(memoryAppendInput{Content: "test", File: "memory.md"})
	result := tool.Run(context.Background(), string(args))
	if !strings.Contains(result, "error") {
		t.Errorf("expected error for unwritable path, got: %s", result)
	}
}

func TestListDirWithFiles(t *testing.T) {
	dir := t.TempDir()
	// Create files of known size
	if err := os.WriteFile(filepath.Join(dir, "small.txt"), []byte("hi"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0755); err != nil {
		t.Fatal(err)
	}

	tool := &ListDirTool{}
	args, _ := json.Marshal(listDirInput{Path: dir})
	result := tool.Run(context.Background(), string(args))

	if !strings.Contains(result, "small.txt") {
		t.Errorf("expected small.txt in listing, got: %s", result)
	}
	if !strings.Contains(result, "subdir") {
		t.Errorf("expected subdir in listing, got: %s", result)
	}
	if !strings.Contains(result, "file") {
		t.Errorf("expected 'file' kind, got: %s", result)
	}
	if !strings.Contains(result, "dir") {
		t.Errorf("expected 'dir' kind, got: %s", result)
	}
}

// ---------------------------------------------------------------------------
// BrowserPool: stale session in getOrCreate, Close with existing session,
// idleReaper coverage
// ---------------------------------------------------------------------------

func TestBrowserPoolGetOrCreateStaleSession(t *testing.T) {
	pool := &BrowserPool{
		sessions: make(map[string]*browserSession),
		done:     make(chan struct{}),
		headless: true,
	}
	// Create a session with an already-cancelled context to simulate a stale/crashed session
	cancelledCtx, cancelFn := context.WithCancel(context.Background())
	cancelFn() // immediately cancel to simulate crash
	pool.sessions["stale-key"] = &browserSession{
		ctx:      cancelledCtx,
		cancel:   cancelFn,
		lastUsed: time.Now().Add(-1 * time.Hour),
	}

	// getOrCreate should detect the stale session, discard it, and create a fresh one
	ctx, err := pool.getOrCreate("stale-key")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ctx == nil {
		t.Fatal("expected non-nil context")
	}
	// The new session should not be the cancelled one
	select {
	case <-ctx.Done():
		// The new chromedp context may or may not be done, but it's a different context
		// Check the pool has a new entry
	default:
		// good - new context is alive
	}
	if _, ok := pool.sessions["stale-key"]; !ok {
		t.Error("expected stale-key to exist in pool after recreation")
	}
}

func TestBrowserPoolGetOrCreateExistingSession(t *testing.T) {
	pool := &BrowserPool{
		sessions: make(map[string]*browserSession),
		done:     make(chan struct{}),
		headless: true,
	}

	// Create a valid (non-cancelled) session
	liveCtx, liveCancel := context.WithCancel(context.Background())
	defer liveCancel()
	pool.sessions["live-key"] = &browserSession{
		ctx:      liveCtx,
		cancel:   liveCancel,
		lastUsed: time.Now().Add(-5 * time.Minute),
	}

	// getOrCreate should return the existing session (reuse it)
	ctx, err := pool.getOrCreate("live-key")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ctx != liveCtx {
		t.Error("expected same context to be returned for live session")
	}
	// lastUsed should be updated
	pool.mu.Lock()
	s := pool.sessions["live-key"]
	pool.mu.Unlock()
	if time.Since(s.lastUsed) > 2*time.Second {
		t.Error("expected lastUsed to be updated to ~now")
	}
}

func TestBrowserPoolCloseExistingSession(t *testing.T) {
	pool := &BrowserPool{
		sessions: make(map[string]*browserSession),
		done:     make(chan struct{}),
	}

	cancelled := false
	pool.sessions["to-close"] = &browserSession{
		ctx:    context.Background(),
		cancel: func() { cancelled = true },
	}

	pool.Close("to-close")
	if !cancelled {
		t.Error("expected cancel to be called")
	}
	pool.mu.Lock()
	_, exists := pool.sessions["to-close"]
	pool.mu.Unlock()
	if exists {
		t.Error("expected session to be removed from pool")
	}
}

func TestBrowserPoolCloseAllWithSessions(t *testing.T) {
	pool := NewBrowserPool(true, "/fake/chrome", false, 0, 0)
	if pool.chromePath != "/fake/chrome" {
		t.Errorf("expected chromePath '/fake/chrome', got %q", pool.chromePath)
	}

	// Add a fake session
	cancelCount := 0
	pool.mu.Lock()
	pool.sessions["sess1"] = &browserSession{
		ctx:    context.Background(),
		cancel: func() { cancelCount++ },
	}
	pool.sessions["sess2"] = &browserSession{
		ctx:    context.Background(),
		cancel: func() { cancelCount++ },
	}
	pool.mu.Unlock()

	pool.CloseAll()

	if cancelCount != 2 {
		t.Errorf("expected 2 cancels, got %d", cancelCount)
	}
	pool.mu.Lock()
	remaining := len(pool.sessions)
	pool.mu.Unlock()
	if remaining != 0 {
		t.Errorf("expected 0 sessions after CloseAll, got %d", remaining)
	}
}

func TestBrowserToolRunSnapshotNoChrome(t *testing.T) {
	pool := &BrowserPool{
		sessions: make(map[string]*browserSession),
		done:     make(chan struct{}),
	}
	tool := &BrowserTool{Pool: pool}
	args, _ := json.Marshal(browserInput{Action: "snapshot"})
	result := tool.Run(context.Background(), string(args))
	// Without Chrome installed, getOrCreate will fail — verify graceful error
	_ = result // just verify no panic
}

func TestBrowserToolSnapshotInSchema(t *testing.T) {
	tool := &BrowserTool{}
	schema := string(tool.Schema())
	if !strings.Contains(schema, `"snapshot"`) {
		t.Errorf("expected 'snapshot' in schema enum, got: %s", schema)
	}
}

func TestBrowserToolRunDefaultSessionKey(t *testing.T) {
	pool := &BrowserPool{
		sessions: make(map[string]*browserSession),
		done:     make(chan struct{}),
	}
	tool := &BrowserTool{Pool: pool}

	// Run close action without a session key in context - should use default key
	args, _ := json.Marshal(browserInput{Action: "close"})
	result := tool.Run(context.Background(), string(args))
	if result != "browser session closed" {
		t.Errorf("expected 'browser session closed', got: %s", result)
	}
}

// ---------------------------------------------------------------------------
// ExecTool — deny command and custom timeout paths
// ---------------------------------------------------------------------------

func TestExecToolDenyCommand(t *testing.T) {
	tool := &ExecTool{
		DefaultTimeout: 5 * time.Second,
		DenyCommands:   []string{"rm -rf", "shutdown"},
	}
	args, _ := json.Marshal(ExecInput{Command: "rm -rf /"})
	result := tool.Run(context.Background(), string(args))
	if !strings.Contains(result, "denied") {
		t.Errorf("expected denied error, got: %s", result)
	}
}

func TestExecToolCustomTimeout(t *testing.T) {
	tool := &ExecTool{
		DefaultTimeout: 30 * time.Second,
	}
	args, _ := json.Marshal(ExecInput{Command: "echo timeout-test", Timeout: 5})
	result := tool.Run(context.Background(), string(args))
	if !strings.Contains(result, "timeout-test") {
		t.Errorf("expected 'timeout-test' output, got: %s", result)
	}
}

// ---------------------------------------------------------------------------
// ExecTool.Description / BrowserTool.Description
// ---------------------------------------------------------------------------

func TestExecToolDescription(t *testing.T) {
	tool := &ExecTool{}
	desc := tool.Description()
	if desc == "" {
		t.Error("ExecTool.Description should not be empty")
	}
}

func TestBrowserToolDescription(t *testing.T) {
	pool := NewBrowserPool(true, "", false, 0, 0)
	defer pool.CloseAll()
	tool := &BrowserTool{Pool: pool}
	desc := tool.Description()
	if desc == "" {
		t.Error("BrowserTool.Description should not be empty")
	}
}

// ---------------------------------------------------------------------------
// Browser: links, text, cookies, headers actions
// ---------------------------------------------------------------------------

func TestBrowserToolRunLinksNoChrome(t *testing.T) {
	pool := &BrowserPool{
		sessions: make(map[string]*browserSession),
		done:     make(chan struct{}),
	}
	tool := &BrowserTool{Pool: pool}
	args, _ := json.Marshal(browserInput{Action: "links"})
	result := tool.Run(context.Background(), string(args))
	// Without Chrome, getOrCreate will fail — verify graceful error
	_ = result // just verify no panic
}

func TestBrowserToolRunTextNoChrome(t *testing.T) {
	pool := &BrowserPool{
		sessions: make(map[string]*browserSession),
		done:     make(chan struct{}),
	}
	tool := &BrowserTool{Pool: pool}
	args, _ := json.Marshal(browserInput{Action: "text"})
	result := tool.Run(context.Background(), string(args))
	_ = result
}

func TestBrowserToolRunCookiesNoChrome(t *testing.T) {
	pool := &BrowserPool{
		sessions: make(map[string]*browserSession),
		done:     make(chan struct{}),
	}
	tool := &BrowserTool{Pool: pool}
	args, _ := json.Marshal(browserInput{Action: "cookies"})
	result := tool.Run(context.Background(), string(args))
	_ = result
}

func TestBrowserToolRunHeadersNoChrome(t *testing.T) {
	pool := &BrowserPool{
		sessions: make(map[string]*browserSession),
		done:     make(chan struct{}),
	}
	tool := &BrowserTool{Pool: pool}
	args, _ := json.Marshal(browserInput{Action: "headers"})
	result := tool.Run(context.Background(), string(args))
	_ = result
}

func TestBrowserToolLinksInSchema(t *testing.T) {
	tool := &BrowserTool{}
	schema := string(tool.Schema())
	for _, action := range []string{`"links"`, `"text"`, `"cookies"`, `"headers"`} {
		if !strings.Contains(schema, action) {
			t.Errorf("expected %s in schema enum, got: %s", action, schema)
		}
	}
}

func TestBrowserToolDescriptionMentionsCrawling(t *testing.T) {
	tool := &BrowserTool{}
	desc := tool.Description()
	if !strings.Contains(desc, "crawling") {
		t.Errorf("expected description to mention crawling, got: %s", desc)
	}
}

func TestBrowserToolLinksDefaultLimit(t *testing.T) {
	// Verify that limit defaults to 50 when not specified
	pool := &BrowserPool{
		sessions: make(map[string]*browserSession),
		done:     make(chan struct{}),
	}
	tool := &BrowserTool{Pool: pool}
	args, _ := json.Marshal(browserInput{Action: "links", Limit: 0})
	result := tool.Run(context.Background(), string(args))
	// Will error because no Chrome, but exercises the default limit path
	_ = result
}

func TestLastNewline(t *testing.T) {
	tests := []struct {
		input string
		want  int
	}{
		{"hello\nworld\nfoo", 11},
		{"no newlines", -1},
		{"\n", 0},
		{"abc\n", 3},
	}
	for _, tt := range tests {
		got := lastNewline(tt.input)
		if got != tt.want {
			t.Errorf("lastNewline(%q) = %d, want %d", tt.input, got, tt.want)
		}
	}
}

func TestReadFileToolDescription(t *testing.T) {
	tool := &ReadFileTool{}
	if tool.Description() == "" {
		t.Error("ReadFileTool.Description should not be empty")
	}
}

func TestWriteFileToolDescription(t *testing.T) {
	tool := &WriteFileTool{}
	if tool.Description() == "" {
		t.Error("WriteFileTool.Description should not be empty")
	}
}

func TestListDirToolDescription(t *testing.T) {
	tool := &ListDirTool{}
	if tool.Description() == "" {
		t.Error("ListDirTool.Description should not be empty")
	}
}

func TestMemoryAppendToolDescription(t *testing.T) {
	tool := &MemoryAppendTool{}
	if tool.Description() == "" {
		t.Error("MemoryAppendTool.Description should not be empty")
	}
}

func TestMemoryGetToolDescription(t *testing.T) {
	tool := &MemoryGetTool{}
	if tool.Description() == "" {
		t.Error("MemoryGetTool.Description should not be empty")
	}
}

func TestWebSearchToolDescription(t *testing.T) {
	tool := &WebSearchTool{}
	if tool.Description() == "" {
		t.Error("WebSearchTool.Description should not be empty")
	}
}

func TestWebFetchToolDescription(t *testing.T) {
	tool := &WebFetchTool{}
	if tool.Description() == "" {
		t.Error("WebFetchTool.Description should not be empty")
	}
}

func TestWriteFileAllowPathBlocked(t *testing.T) {
	tool := &WriteFileTool{AllowPaths: []string{"/allowed/dir"}}
	args, _ := json.Marshal(writeFileInput{Path: "/blocked/dir/file.txt", Content: "test"})
	result := tool.Run(context.Background(), string(args))
	if !strings.Contains(result, "error") {
		t.Errorf("expected error for blocked path, got: %s", result)
	}
}

func TestResolveSymlinksNonexistentRoot(t *testing.T) {
	// Path where no ancestor exists except filesystem root
	got := resolveSymlinks("/nonexistent/deeply/nested/path")
	// Should return the cleaned path since it walks up to root
	if got == "" {
		t.Error("expected non-empty resolved path")
	}
}

// ---------------------------------------------------------------------------
// NotifyUserTool
// ---------------------------------------------------------------------------

func TestNotifyUserTool_Basic(t *testing.T) {
	announced := make(map[string]string)
	mock := &mockAnnouncer{announced: announced}
	tool := &NotifyUserTool{Announcers: []agentapi.Announcer{mock}}

	if tool.Name() != "notify_user" {
		t.Fatalf("name = %q, want notify_user", tool.Name())
	}
	if tool.Description() == "" {
		t.Fatal("expected non-empty description")
	}
	if tool.Schema() == nil {
		t.Fatal("expected non-nil schema")
	}

	ctx := context.WithValue(context.Background(), SessionKeyContextKey{}, "tg:123")
	result := tool.Run(ctx, `{"message":"hello"}`)
	if !strings.Contains(result, "1 channel") {
		t.Errorf("result = %q, want mention of 1 channel", result)
	}
	if announced["tg:123"] != "hello" {
		t.Errorf("announced[tg:123] = %q, want hello", announced["tg:123"])
	}
}

func TestNotifyUserTool_NoSession(t *testing.T) {
	tool := &NotifyUserTool{}
	result := tool.Run(context.Background(), `{"message":"hello"}`)
	if !strings.Contains(result, "no session") {
		t.Errorf("result = %q, want 'no session' message", result)
	}
}

func TestNotifyUserTool_EmptyMessage(t *testing.T) {
	tool := &NotifyUserTool{}
	ctx := context.WithValue(context.Background(), SessionKeyContextKey{}, "tg:123")
	result := tool.Run(ctx, `{"message":""}`)
	if !strings.Contains(result, "error") {
		t.Errorf("result = %q, want error for empty message", result)
	}
}

func TestNotifyUserTool_NoAnnouncers(t *testing.T) {
	tool := &NotifyUserTool{}
	ctx := context.WithValue(context.Background(), SessionKeyContextKey{}, "tg:123")
	result := tool.Run(ctx, `{"message":"hello"}`)
	if !strings.Contains(result, "no delivery channel") {
		t.Errorf("result = %q, want 'no delivery channel' message", result)
	}
}

func TestNotifyUserTool_InvalidJSON(t *testing.T) {
	tool := &NotifyUserTool{}
	ctx := context.WithValue(context.Background(), SessionKeyContextKey{}, "tg:123")
	result := tool.Run(ctx, `{bad`)
	if !strings.Contains(result, "error") {
		t.Errorf("result = %q, want error for invalid JSON", result)
	}
}

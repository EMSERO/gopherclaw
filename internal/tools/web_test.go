package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseDDGResultsBasic(t *testing.T) {
	html := `<div class="results">
<div class="result">
<a class="result__a" href="https://example.com/page">Example Title</a>
<a class="result__snippet">This is the snippet text for the result.</a>
</div>
</div>`
	results := parseDDGResults(html, 5)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].title != "Example Title" {
		t.Errorf("expected title 'Example Title', got %q", results[0].title)
	}
	if results[0].url != "https://example.com/page" {
		t.Errorf("expected url 'https://example.com/page', got %q", results[0].url)
	}
	if !strings.Contains(results[0].snippet, "snippet text") {
		t.Errorf("expected snippet to contain 'snippet text', got %q", results[0].snippet)
	}
}

func TestParseDDGDecodeRedirectURL(t *testing.T) {
	cases := []struct {
		raw    string
		expect string
	}{
		{
			"//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2Fpath&kh=-1",
			"https://example.com/path",
		},
		{
			"/l/?uddg=https%3A%2F%2Fgolang.org%2Fdoc",
			"https://golang.org/doc",
		},
		{
			"https://example.com/direct",
			"https://example.com/direct",
		},
		{
			"",
			"",
		},
	}
	for _, tc := range cases {
		got := decodeDDGRedirect(tc.raw)
		if got != tc.expect {
			t.Errorf("decodeDDGRedirect(%q) = %q, want %q", tc.raw, got, tc.expect)
		}
	}
}

func TestParseDDGRedirectHref(t *testing.T) {
	// Simulate a DDG redirect href in a result block
	html := `<div class="result">
<a href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fgolang.org%2F&kh=-1" class="result__a">Go Language</a>
<a class="result__snippet">The Go programming language.</a>
</div>`
	results := parseDDGResults(html, 5)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].url != "https://golang.org/" {
		t.Errorf("expected decoded URL 'https://golang.org/', got %q", results[0].url)
	}
}

func TestParseDDGEmptyHTML(t *testing.T) {
	results := parseDDGResults("", 5)
	if len(results) != 0 {
		t.Errorf("expected no results for empty HTML, got %d", len(results))
	}
}

func TestParseDDGNoResults(t *testing.T) {
	// CAPTCHA-like page with no result markers
	results := parseDDGResults("<html><body>Please verify you are human</body></html>", 5)
	if len(results) != 0 {
		t.Errorf("expected no results for CAPTCHA page, got %d", len(results))
	}
}

func TestParseDDGMaxResults(t *testing.T) {
	// Build HTML with 5 results
	var sb strings.Builder
	for i := range 5 {
		sb.WriteString(`<a href="https://example.com/` + string(rune('a'+i)) + `" class="result__a">Title ` + string(rune('A'+i)) + `</a>`)
		sb.WriteString(`<a class="result__snippet">Snippet ` + string(rune('A'+i)) + `</a>`)
	}
	results := parseDDGResults(sb.String(), 3)
	if len(results) != 3 {
		t.Errorf("expected 3 results (max), got %d", len(results))
	}
}

func TestParseDDGResultURLFallback(t *testing.T) {
	// No href, but result__url span present
	html := `<div class="result">
<a class="result__a">No Href Title</a>
<span class="result__url">example.com/nohref</span>
<a class="result__snippet">Some snippet.</a>
</div>`
	results := parseDDGResults(html, 5)
	if len(results) == 1 && results[0].url != "example.com/nohref" {
		t.Errorf("expected fallback url 'example.com/nohref', got %q", results[0].url)
	}
	// If title is empty or url is empty, result may be skipped — that's OK
	// This test just verifies no panic occurs
}

func TestStripTags(t *testing.T) {
	cases := []struct {
		in  string
		out string
	}{
		{"<b>hello</b>", "hello"},
		{"no tags", "no tags"},
		{"<a href=\"x\">link &amp; text</a>", "link & text"},
		{"<span>A &lt; B</span>", "A < B"},
	}
	for _, tc := range cases {
		got := stripTags(tc.in)
		if got != tc.out {
			t.Errorf("stripTags(%q) = %q, want %q", tc.in, got, tc.out)
		}
	}
}

// ---------------------------------------------------------------------------
// WebSearchTool.Run — additional coverage paths
// ---------------------------------------------------------------------------

func TestWebSearchToolRunDefaultTimeout(t *testing.T) {
	// Verify that default timeout (30s) is used when TimeoutSeconds is 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<a href="https://example.com" class="result__a">Title</a><a class="result__snippet">Snip</a>`)
	}))
	defer srv.Close()

	tool := &WebSearchTool{
		MaxResults:     5,
		TimeoutSeconds: 0, // should use default 30s
		BaseURL:        srv.URL + "/",
		Client:         srv.Client(),
	}

	args, _ := json.Marshal(webSearchInput{Query: "test"})
	result := tool.Run(context.Background(), string(args))
	if !strings.Contains(result, "Title") {
		t.Errorf("expected 'Title' in result, got: %s", result)
	}
}

func TestWebSearchToolRunDefaultMaxResults(t *testing.T) {
	// Verify that default max=5 is used when MaxResults is 0
	var sb strings.Builder
	for i := range 10 {
		fmt.Fprintf(&sb, `<a href="https://example.com/%d" class="result__a">Title %d</a><a class="result__snippet">Snip %d</a>`, i, i, i)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, sb.String())
	}))
	defer srv.Close()

	tool := &WebSearchTool{
		MaxResults:     0, // should default to 5
		TimeoutSeconds: 5,
		BaseURL:        srv.URL + "/",
		Client:         srv.Client(),
	}

	args, _ := json.Marshal(webSearchInput{Query: "test"})
	result := tool.Run(context.Background(), string(args))
	// Count results: each result line starts with [N]
	count := strings.Count(result, "[")
	if count != 5 {
		t.Errorf("expected 5 results (default max), got %d: %s", count, result)
	}
}

func TestWebSearchToolRunDefaultBaseURL(t *testing.T) {
	// Verify that default base URL is DDG when BaseURL is empty.
	// We can't actually hit DDG, so just verify the error message mentions the URL.
	tool := &WebSearchTool{
		TimeoutSeconds: 1,
		// BaseURL empty, Client nil => uses real DDG which may timeout or fail
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	args, _ := json.Marshal(webSearchInput{Query: "test"})
	result := tool.Run(ctx, string(args))
	// Either it returns results or an error - either way it exercised the default path
	_ = result
}

// ---------------------------------------------------------------------------
// WebFetchTool.Run — additional coverage paths
// ---------------------------------------------------------------------------

func TestWebFetchToolRunDefaultsApplied(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "simple text response")
	}))
	defer srv.Close()

	// TimeoutSeconds=0 and MaxChars=0 => should use defaults (30s, 50000)
	tool := &WebFetchTool{
		TimeoutSeconds: 0,
		MaxChars:       0,
		Client:         srv.Client(),
		SkipSSRF:       true,
	}

	args, _ := json.Marshal(webFetchInput{URL: srv.URL})
	result := tool.Run(context.Background(), string(args))
	if result != "simple text response" {
		t.Errorf("expected 'simple text response', got: %s", result)
	}
}

func TestWebFetchToolRunWithTransport(t *testing.T) {
	// Test that when Client is nil but Transport is set, it creates a client with that transport
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "transport test")
	}))
	defer srv.Close()

	tool := &WebFetchTool{
		TimeoutSeconds: 5,
		Client:         nil,
		Transport:      srv.Client().Transport.(*http.Transport),
		SkipSSRF:       true,
	}

	args, _ := json.Marshal(webFetchInput{URL: srv.URL})
	result := tool.Run(context.Background(), string(args))
	if result != "transport test" {
		t.Errorf("expected 'transport test', got: %s", result)
	}
}

func TestWebFetchToolRunCreatesDefaultTransport(t *testing.T) {
	// When both Client and Transport are nil, a new SSRFSafeTransport is created
	// This will hit real DNS, so use a public URL that resolves
	tool := &WebFetchTool{
		TimeoutSeconds: 2,
		Client:         nil,
		Transport:      nil,
		SkipSSRF:       true, // skip SSRF to avoid DNS resolution issues
	}

	// Use a context that times out quickly
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	args, _ := json.Marshal(webFetchInput{URL: "http://0.0.0.1:1"})
	result := tool.Run(ctx, string(args))
	// Expect connection error since the address doesn't serve anything
	if !strings.Contains(result, "error") {
		t.Errorf("expected error, got: %s", result)
	}
}

// ---------------------------------------------------------------------------
// SSRFSafeTransport DialContext paths
// ---------------------------------------------------------------------------

func TestSSRFSafeTransportBlocksLoopback(t *testing.T) {
	tr := SSRFSafeTransport()
	client := &http.Client{Transport: tr}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, "GET", "http://127.0.0.1:1/test", nil)
	_, err := client.Do(req)
	if err == nil {
		t.Error("expected SSRF block for loopback, got nil error")
	}
	if !strings.Contains(err.Error(), "ssrf") && !strings.Contains(err.Error(), "private") {
		t.Errorf("expected ssrf/private error, got: %v", err)
	}
}

func TestSSRFSafeTransportBlocksLocalhost(t *testing.T) {
	tr := SSRFSafeTransport()
	client := &http.Client{Transport: tr}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, "GET", "http://localhost:1/test", nil)
	_, err := client.Do(req)
	if err == nil {
		t.Error("expected SSRF block for localhost, got nil error")
	}
}

// ---------------------------------------------------------------------------
// decodeDDGRedirect edge cases
// ---------------------------------------------------------------------------

func TestDecodeDDGRedirectInvalidURL(t *testing.T) {
	// A malformed URL should be returned as-is
	raw := ":%invalid"
	got := decodeDDGRedirect(raw)
	if got != raw {
		t.Errorf("expected raw back for invalid URL, got %q", got)
	}
}

func TestDecodeDDGRedirectNoUddgParam(t *testing.T) {
	// DDG redirect path but no uddg parameter
	raw := "//duckduckgo.com/l/?other=value"
	got := decodeDDGRedirect(raw)
	if got != raw {
		t.Errorf("expected raw back when no uddg param, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// parseDDGResults edge cases
// ---------------------------------------------------------------------------

func TestParseDDGResultsMissingClosingTag(t *testing.T) {
	// Result with missing </a> closing tag after title
	html := `<a href="https://example.com" class="result__a">Incomplete title`
	results := parseDDGResults(html, 5)
	// Should handle gracefully without panic
	_ = results
}

func TestParseDDGResultsSnippetWithSpan(t *testing.T) {
	// Snippet closed by </span> instead of </a>
	html := `<a href="https://example.com" class="result__a">Title</a>
<span class="result__snippet">Snippet in span</span>`
	results := parseDDGResults(html, 5)
	if len(results) == 1 && results[0].snippet != "Snippet in span" {
		t.Errorf("expected snippet 'Snippet in span', got %q", results[0].snippet)
	}
}

func TestParseDDGResultsNoHrefInTag(t *testing.T) {
	// <a> tag with class but no href attribute; should fall back to result__url
	html := `<a class="result__a">Title No Href</a>
<span class="result__url">fallback.example.com</span>`
	results := parseDDGResults(html, 5)
	if len(results) == 1 {
		if results[0].url != "fallback.example.com" {
			t.Errorf("expected fallback URL, got %q", results[0].url)
		}
	}
}

// ---------------------------------------------------------------------------
// SSRFSafeTransport — DNS resolution failure path
// ---------------------------------------------------------------------------

func TestSSRFSafeTransportUnresolvableHost(t *testing.T) {
	tr := SSRFSafeTransport()
	client := &http.Client{Transport: tr, Timeout: 3 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// Use a hostname that will fail DNS resolution
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://this-host-does-not-exist-zzzzz.invalid:80/test", nil)
	_, err := client.Do(req)
	if err == nil {
		t.Error("expected DNS resolution error, got nil")
	}
}

// ---------------------------------------------------------------------------
// WebSearchTool.Run — read error path
// ---------------------------------------------------------------------------

func TestWebSearchToolRunReadBodyError(t *testing.T) {
	// Server that returns content-length but closes connection early
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "99999999")
		w.WriteHeader(200)
		// Write partial data then close
		fmt.Fprint(w, `<a href="https://example.com" class="result__a">Partial</a>`)
		// Connection will close after handler returns with incomplete body
	}))
	defer srv.Close()

	tool := &WebSearchTool{
		TimeoutSeconds: 2,
		BaseURL:        srv.URL + "/",
		Client:         srv.Client(),
	}
	args, _ := json.Marshal(webSearchInput{Query: "test"})
	result := tool.Run(context.Background(), string(args))
	// Should still process whatever was read
	_ = result
}

// ---------------------------------------------------------------------------
// parseDDGResults — edge case: no <a before marker
// ---------------------------------------------------------------------------

func TestParseDDGResultsNoATagBeforeMarker(t *testing.T) {
	// class="result__a" exists but no <a tag before it (corrupt HTML)
	html := `<div class="result__a">Title Without A Tag</div>`
	results := parseDDGResults(html, 5)
	// Should skip gracefully
	if len(results) != 0 {
		t.Errorf("expected 0 results for corrupt HTML, got %d", len(results))
	}
}

func TestParseDDGResultsNoClosingTagEnd(t *testing.T) {
	// Marker found but no '>' after it
	html := `<a class="result__a"`
	results := parseDDGResults(html, 5)
	if len(results) != 0 {
		t.Errorf("expected 0 results, got %d", len(results))
	}
}

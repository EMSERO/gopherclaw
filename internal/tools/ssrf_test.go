package tools

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"
)

func TestIsPrivateOrReservedIP(t *testing.T) {
	cases := []struct {
		ip       string
		expected bool
	}{
		{"127.0.0.1", true},
		{"::1", true},
		{"10.0.0.1", true},
		{"192.168.1.1", true},
		{"172.16.0.1", true},
		{"169.254.1.1", true},     // link-local
		{"0.0.0.0", true},         // unspecified
		{"224.0.0.1", true},       // multicast
		{"8.8.8.8", false},        // public
		{"1.1.1.1", false},        // public
		{"93.184.216.34", false},  // example.com
		{"198.18.0.1", false},     // RFC 2544 benchmark (exempt)
		{"198.19.255.254", false}, // RFC 2544 upper bound
	}
	for _, tc := range cases {
		ip := net.ParseIP(tc.ip)
		if ip == nil {
			t.Fatalf("failed to parse IP %s", tc.ip)
		}
		got := isPrivateOrReservedIP(ip)
		if got != tc.expected {
			t.Errorf("isPrivateOrReservedIP(%s) = %v, want %v", tc.ip, got, tc.expected)
		}
	}
}

func TestCheckSSRFPrivateIPs(t *testing.T) {
	// Loopback should be blocked
	err := checkSSRF("http://127.0.0.1/secret")
	if err == nil {
		t.Error("expected SSRF block for 127.0.0.1")
	}
	if !strings.Contains(err.Error(), "private/reserved") {
		t.Errorf("expected private/reserved error, got: %v", err)
	}

	// localhost should be blocked
	err = checkSSRF("http://localhost/secret")
	if err == nil {
		t.Error("expected SSRF block for localhost")
	}
}

func TestCheckSSRFInvalidURL(t *testing.T) {
	err := checkSSRF("://invalid")
	if err == nil {
		t.Error("expected error for invalid URL")
	}
}

func TestCheckSSRFNoHost(t *testing.T) {
	err := checkSSRF("file:///etc/passwd")
	if err == nil {
		t.Error("expected error for URL with no host")
	}
}

func TestWebFetchToolSSRFBlock(t *testing.T) {
	tool := &WebFetchTool{TimeoutSeconds: 5}
	args, _ := json.Marshal(webFetchInput{URL: "http://127.0.0.1:8080/admin"})
	result := tool.Run(context.Background(), string(args))
	if !strings.Contains(result, "private/reserved") {
		t.Errorf("expected SSRF block, got: %s", result)
	}
}

func TestWebFetchToolInvalidJSON(t *testing.T) {
	tool := &WebFetchTool{}
	result := tool.Run(context.Background(), "bad json")
	if !strings.Contains(result, "error") {
		t.Errorf("expected error, got: %s", result)
	}
}

func TestWebSearchToolInvalidJSON(t *testing.T) {
	tool := &WebSearchTool{}
	result := tool.Run(context.Background(), "bad json")
	if !strings.Contains(result, "error") {
		t.Errorf("expected error, got: %s", result)
	}
}

func TestWebSearchToolName(t *testing.T) {
	tool := &WebSearchTool{}
	if tool.Name() != "web_search" {
		t.Errorf("expected web_search, got %s", tool.Name())
	}
	var m map[string]any
	if err := json.Unmarshal(tool.Schema(), &m); err != nil {
		t.Errorf("invalid schema: %v", err)
	}
}

func TestWebFetchToolName(t *testing.T) {
	tool := &WebFetchTool{}
	if tool.Name() != "web_fetch" {
		t.Errorf("expected web_fetch, got %s", tool.Name())
	}
	var m map[string]any
	if err := json.Unmarshal(tool.Schema(), &m); err != nil {
		t.Errorf("invalid schema: %v", err)
	}
}

func TestExecBackgroundMode(t *testing.T) {
	tool := &ExecTool{
		DefaultTimeout: 30 * time.Second,
		BackgroundWait: 100 * time.Millisecond,
	}

	// Command that finishes quickly — should get normal output
	args, _ := json.Marshal(ExecInput{Command: "echo fast"})
	result := tool.Run(context.Background(), string(args))
	result = strings.TrimSpace(result)
	if result != "fast" {
		t.Errorf("expected 'fast', got %q", result)
	}
}

func TestExecBackgroundModeSlow(t *testing.T) {
	tool := &ExecTool{
		DefaultTimeout: 30 * time.Second,
		BackgroundWait: 100 * time.Millisecond,
	}

	// Command that takes a while — should return partial output
	args, _ := json.Marshal(ExecInput{Command: "echo started; sleep 5"})
	result := tool.Run(context.Background(), string(args))
	if !strings.Contains(result, "still running") && !strings.Contains(result, "started") {
		t.Errorf("expected background indicator or partial output, got %q", result)
	}
}

func TestResolveSymlinks(t *testing.T) {
	// Non-existent path returns cleaned version
	result := resolveSymlinks("/nonexistent/path/to/file")
	if result != "/nonexistent/path/to/file" {
		t.Errorf("expected clean path, got %q", result)
	}

	// Real path resolves correctly
	dir := t.TempDir()
	result = resolveSymlinks(dir)
	if result == "" {
		t.Error("expected non-empty resolved path")
	}
}

func TestExecInvalidJSON(t *testing.T) {
	tool := &ExecTool{DefaultTimeout: 5 * time.Second}
	result := tool.Run(context.Background(), "not json")
	if !strings.Contains(result, "invalid arguments") {
		t.Errorf("expected invalid arguments error, got %q", result)
	}
}

func TestHtmlDecode(t *testing.T) {
	cases := []struct {
		in  string
		out string
	}{
		{"&amp;", "&"},
		{"&lt;", "<"},
		{"&gt;", ">"},
		{"&quot;", `"`},
		{"&#39;", "'"},
		{"&nbsp;", " "},
		{"no entities", "no entities"},
	}
	for _, tc := range cases {
		got := htmlDecode(tc.in)
		if got != tc.out {
			t.Errorf("htmlDecode(%q) = %q, want %q", tc.in, got, tc.out)
		}
	}
}

func TestSessionKeyContextKey(t *testing.T) {
	// Verify the context key type works
	ctx := context.WithValue(context.Background(), SessionKeyContextKey{}, "test-session")
	val := ctx.Value(SessionKeyContextKey{})
	if val != "test-session" {
		t.Errorf("expected 'test-session', got %v", val)
	}
}

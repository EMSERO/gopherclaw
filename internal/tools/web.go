package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// WebSearchTool searches the web using DuckDuckGo.
type WebSearchTool struct {
	MaxResults     int
	TimeoutSeconds int
	BaseURL        string       // override for testing; defaults to DDG HTML search endpoint
	Client         *http.Client // override for testing; defaults to http.DefaultClient
}

type webSearchInput struct {
	Query string `json:"query"`
}

func (t *WebSearchTool) Name() string { return "web_search" }

func (t *WebSearchTool) Description() string {
	return "Search the web using DuckDuckGo and return a list of results with titles, URLs, and snippets."
}

func (t *WebSearchTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"query": {"type": "string", "description": "Search query"}
		},
		"required": ["query"]
	}`)
}

func (t *WebSearchTool) Run(ctx context.Context, argsJSON string) string {
	var in webSearchInput
	if err := json.Unmarshal([]byte(argsJSON), &in); err != nil {
		return fmt.Sprintf("error: %v", err)
	}

	timeout := time.Duration(t.TimeoutSeconds) * time.Second
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// DuckDuckGo Lite HTML search
	base := t.BaseURL
	if base == "" {
		base = "https://html.duckduckgo.com/html/"
	}
	searchURL := base + "?q=" + url.QueryEscape(in.Query)
	req, err := http.NewRequestWithContext(ctx, "GET", searchURL, nil)
	if err != nil {
		return fmt.Sprintf("error: %v", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64; rv:120.0) Gecko/20100101 Firefox/120.0")
	req.Header.Set("Accept", "text/html")

	client := t.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Sprintf("error fetching search results: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 200_000))
	if err != nil {
		return fmt.Sprintf("error reading response: %v", err)
	}

	bodyStr := string(body)
	results := parseDDGResults(bodyStr, t.MaxResults)
	if len(results) == 0 {
		// Distinguish rate-limit/CAPTCHA from genuinely empty results
		if !strings.Contains(bodyStr, "result__a") && !strings.Contains(bodyStr, "result__snippet") {
			return "No results found (DDG may be rate-limiting or showing a CAPTCHA page)."
		}
		return "No results found."
	}

	var sb []byte
	for i, r := range results {
		sb = append(sb, fmt.Sprintf("[%d] %s\n%s\n%s\n\n", i+1, r.title, r.url, r.snippet)...)
	}
	return string(sb)
}

type ddgResult struct {
	title   string
	url     string
	snippet string
}

// parseDDGResults extracts search results from DuckDuckGo HTML.
// Uses absolute positions into html so the advance step never causes lookback issues.
func parseDDGResults(html string, max int) []ddgResult {
	if max == 0 {
		max = 5
	}

	const marker = `class="result__a"`
	var results []ddgResult
	pos := 0

	for len(results) < max && pos < len(html) {
		// Find the class marker for a result title link
		markerRel := strings.Index(html[pos:], marker)
		if markerRel < 0 {
			break
		}
		markerAbs := pos + markerRel

		// Find the opening <a tag by scanning backward from the marker
		tagStart := strings.LastIndex(html[:markerAbs], "<a")
		if tagStart < 0 {
			pos = markerAbs + len(marker)
			continue
		}

		// Find the closing > of the opening tag (scan forward from marker)
		tagEndRel := strings.Index(html[markerAbs:], ">")
		if tagEndRel < 0 {
			pos = markerAbs + len(marker)
			continue
		}
		tagEnd := markerAbs + tagEndRel // absolute index of '>'

		// Full opening tag text — search it for href= (works regardless of attribute order)
		openTag := html[tagStart : tagEnd+1]
		var href string
		if _, after, ok := strings.Cut(openTag, `href="`); ok {
			rest := after
			if before, _, ok := strings.Cut(rest, `"`); ok {
				href = decodeDDGRedirect(before)
			}
		}

		// Extract title text: between '>' and the first </a> after tagEnd
		titleStart := tagEnd + 1
		closeARel := strings.Index(html[titleStart:], "</a>")
		var title string
		if closeARel >= 0 {
			title = strings.TrimSpace(stripTags(html[titleStart : titleStart+closeARel]))
		}

		// Search for snippet after the closing </a> of the title link
		snippetFrom := titleStart
		if closeARel >= 0 {
			snippetFrom = titleStart + closeARel + 4
		}
		var snippet string
		if snipRel := strings.Index(html[snippetFrom:], `class="result__snippet"`); snipRel >= 0 {
			snipAbs := snippetFrom + snipRel
			if snipTagEndRel := strings.Index(html[snipAbs:], ">"); snipTagEndRel >= 0 {
				contentStart := snipAbs + snipTagEndRel + 1
				snipCloseA := strings.Index(html[contentStart:], "</a>")
				snipCloseSpan := strings.Index(html[contentStart:], "</span>")
				snipClose := -1
				switch {
				case snipCloseA >= 0 && (snipCloseSpan < 0 || snipCloseA < snipCloseSpan):
					snipClose = snipCloseA
				case snipCloseSpan >= 0:
					snipClose = snipCloseSpan
				}
				if snipClose >= 0 {
					snippet = strings.TrimSpace(stripTags(html[contentStart : contentStart+snipClose]))
				}
			}
		}

		// If href is still empty, try result__url span as fallback
		if href == "" {
			if urlRel := strings.Index(html[markerAbs:], `class="result__url"`); urlRel >= 0 {
				urlAbs := markerAbs + urlRel
				if urlTagEndRel := strings.Index(html[urlAbs:], ">"); urlTagEndRel >= 0 {
					urlContent := urlAbs + urlTagEndRel + 1
					if urlCloseRel := strings.Index(html[urlContent:], "</"); urlCloseRel >= 0 {
						href = strings.TrimSpace(stripTags(html[urlContent : urlContent+urlCloseRel]))
					}
				}
			}
		}

		if title != "" && href != "" {
			results = append(results, ddgResult{title: title, url: href, snippet: snippet})
		}

		// Advance past the current marker to search for the next result
		pos = markerAbs + len(marker)
	}
	return results
}

// decodeDDGRedirect extracts the real URL from a DuckDuckGo redirect href.
// DDG wraps URLs as: //duckduckgo.com/l/?uddg=<encoded>&... or /l/?uddg=...
// If not a redirect, returns the raw href unchanged.
func decodeDDGRedirect(raw string) string {
	if raw == "" {
		return ""
	}
	// Normalise: //duckduckgo.com/l/... → https://duckduckgo.com/l/...
	normalized := raw
	if strings.HasPrefix(raw, "//") {
		normalized = "https:" + raw
	}
	parsed, err := url.Parse(normalized)
	if err != nil {
		return raw
	}
	// Check if this is a DDG redirect path
	if strings.Contains(parsed.Path, "/l/") || strings.Contains(parsed.Path, "/l?") {
		if uddg := parsed.Query().Get("uddg"); uddg != "" {
			decoded, err := url.QueryUnescape(uddg)
			if err == nil && decoded != "" {
				return decoded
			}
		}
	}
	return raw
}

func stripTags(s string) string {
	var out []byte
	inTag := false
	for i := 0; i < len(s); i++ {
		if s[i] == '<' {
			inTag = true
			continue
		}
		if inTag {
			if s[i] == '>' {
				inTag = false
			}
			continue
		}
		out = append(out, s[i])
	}
	// Decode basic HTML entities
	result := string(out)
	result = htmlDecode(result)
	return result
}

var htmlReplacer = strings.NewReplacer(
	"&amp;", "&",
	"&lt;", "<",
	"&gt;", ">",
	"&quot;", `"`,
	"&#39;", "'",
	"&nbsp;", " ",
)

func htmlDecode(s string) string {
	return htmlReplacer.Replace(s)
}

// rfc2544Range is 198.18.0.0/15 (RFC 2544 benchmark testing range).
// Allowed for proxy/fake-IP networking modes.
var rfc2544Range = &net.IPNet{
	IP:   net.IP{198, 18, 0, 0},
	Mask: net.CIDRMask(15, 32),
}

// isPrivateOrReservedIP returns true if ip is a loopback, private, link-local,
// or multicast address — i.e. any address that should not be reachable from a
// server-side HTTP fetch (SSRF guard).
// RFC 2544 benchmark range (198.18.0.0/15) is explicitly allowed.
func isPrivateOrReservedIP(ip net.IP) bool {
	// Exempt RFC 2544 benchmark range (198.18.0.0/15)
	if rfc2544Range.Contains(ip) {
		return false
	}
	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() ||
		ip.IsUnspecified()
}

// checkSSRF resolves the host in rawURL and blocks requests to private,
// loopback, link-local, and multicast addresses (IPv4 and IPv6).
// Only http and https schemes are allowed.
func checkSSRF(rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	switch parsed.Scheme {
	case "http", "https":
		// allowed
	case "":
		return fmt.Errorf("URL has no scheme")
	default:
		return fmt.Errorf("blocked scheme %q: only http and https are allowed", parsed.Scheme)
	}
	host := parsed.Hostname()
	if host == "" {
		return fmt.Errorf("URL has no host")
	}
	// Resolve the host to IP addresses.
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("cannot resolve host %q: %w", host, err)
	}
	for _, ip := range ips {
		if isPrivateOrReservedIP(ip) {
			return fmt.Errorf("blocked: host %q resolves to private/reserved address %s", host, ip)
		}
	}
	return nil
}

// SSRFSafeTransport returns an http.Transport that validates resolved IPs
// against SSRF rules before connecting. This closes the TOCTOU gap between
// the checkSSRF pre-check and the actual TCP connection (DNS rebinding).
func SSRFSafeTransport() *http.Transport {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	return &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, fmt.Errorf("ssrf: invalid address %q: %w", addr, err)
			}
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, fmt.Errorf("ssrf: cannot resolve %q: %w", host, err)
			}
			for _, ipAddr := range ips {
				if isPrivateOrReservedIP(ipAddr.IP) {
					return nil, fmt.Errorf("ssrf: blocked — %q resolves to private address %s", host, ipAddr.IP)
				}
			}
			// Dial the validated IP directly to prevent re-resolution
			return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
		},
	}
}

// WebFetchTool fetches a URL and returns its text content.
type WebFetchTool struct {
	MaxChars       int
	TimeoutSeconds int
	Transport      *http.Transport // shared SSRF-safe transport; nil = create per-call (wasteful)
	Client         *http.Client    // override for testing; nil = use Transport
	SkipSSRF       bool            // skip SSRF check (test only)
}

type webFetchInput struct {
	URL string `json:"url"`
}

func (t *WebFetchTool) Name() string { return "web_fetch" }

func (t *WebFetchTool) Description() string {
	return "Fetch the content of a web page by URL and return its text. Supports HTML-to-text conversion with configurable size limits."
}

func (t *WebFetchTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"url": {"type": "string", "description": "URL to fetch"}
		},
		"required": ["url"]
	}`)
}

func (t *WebFetchTool) Run(ctx context.Context, argsJSON string) string {
	var in webFetchInput
	if err := json.Unmarshal([]byte(argsJSON), &in); err != nil {
		return fmt.Sprintf("error: %v", err)
	}

	timeout := time.Duration(t.TimeoutSeconds) * time.Second
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	maxChars := t.MaxChars
	if maxChars == 0 {
		maxChars = 50000
	}

	// SSRF guard: block private, loopback, link-local, and multicast addresses.
	if !t.SkipSSRF {
		if err := checkSSRF(in.URL); err != nil {
			return fmt.Sprintf("error: %v", err)
		}
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", in.URL, nil)
	if err != nil {
		return fmt.Sprintf("error: %v", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64; rv:120.0) Gecko/20100101 Firefox/120.0")

	client := t.Client
	if client == nil {
		tr := t.Transport
		if tr == nil {
			tr = SSRFSafeTransport()
		}
		client = &http.Client{Transport: tr}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Sprintf("error fetching %s: %v", in.URL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxChars*3)))
	if err != nil {
		return fmt.Sprintf("error reading response: %v", err)
	}

	text := stripTags(string(body))
	if len(text) > maxChars {
		text = text[:maxChars] + "\n[... truncated ...]"
	}
	return text
}

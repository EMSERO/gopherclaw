package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"

	"github.com/EMSERO/gopherclaw/internal/agentapi"
)

// browserLog is a file logger for browser pool diagnostics.
var browserLog *log.Logger
var browserLogOnce sync.Once

func blogf(format string, args ...any) {
	browserLogOnce.Do(func() {
		home, _ := os.UserHomeDir()
		dir := filepath.Join(home, ".gopherclaw", "logs")
		_ = os.MkdirAll(dir, 0755)
		f, err := os.OpenFile(filepath.Join(dir, "browser-debug.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return
		}
		browserLog = log.New(f, "browser: ", log.LstdFlags)
	})
	if browserLog != nil {
		browserLog.Printf(format, args...)
	}
}

// SessionKeyContextKey is an alias for agentapi.SessionKeyContextKey.
// Kept for backward compatibility with external references.
type SessionKeyContextKey = agentapi.SessionKeyContextKey

// BrowserPool manages per-session browser instances.
type BrowserPool struct {
	mu          sync.Mutex
	sessions    map[string]*browserSession
	headless    bool
	chromePath  string
	noSandbox   bool // if true, launch Chrome with --no-sandbox (required in some containers)
	maxSessions int  // hard cap on concurrent browser sessions (0 = 10)
	done        chan struct{}
	wg          sync.WaitGroup
}

type browserSession struct {
	ctx      context.Context
	cancel   context.CancelFunc
	lastUsed time.Time
}

// NewBrowserPool creates a pool with the given settings.
// If noSandbox is true, Chrome is launched with --no-sandbox (needed inside
// Docker containers that run as root). Prefer false when possible.
func NewBrowserPool(headless bool, chromePath string, noSandbox bool) *BrowserPool {
	p := &BrowserPool{
		sessions:   make(map[string]*browserSession),
		headless:   headless,
		chromePath: chromePath,
		noSandbox:  noSandbox,
		done:       make(chan struct{}),
	}
	p.wg.Add(1)
	go p.idleReaper()
	return p
}

func (p *BrowserPool) idleReaper() {
	defer p.wg.Done()
	ticker := time.NewTicker(2 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-p.done:
			return
		case <-ticker.C:
			p.mu.Lock()
			for key, s := range p.sessions {
				if time.Since(s.lastUsed) > 10*time.Minute {
					s.cancel()
					delete(p.sessions, key)
				}
			}
			p.mu.Unlock()
		}
	}
}

// getOrCreate returns (or creates) a chromedp context for the session key.
func (p *BrowserPool) getOrCreate(sessionKey string) (context.Context, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if s, ok := p.sessions[sessionKey]; ok {
		select {
		case <-s.ctx.Done():
			// Stale session (e.g. Chrome crashed) — discard and create fresh.
			blogf("getOrCreate(%q): STALE session detected (ctx.Err=%v), creating fresh", sessionKey, s.ctx.Err())
			s.cancel()
			delete(p.sessions, sessionKey)
		default:
			s.lastUsed = time.Now()
			blogf("getOrCreate(%q): reusing existing session", sessionKey)
			return s.ctx, nil
		}
	} else {
		blogf("getOrCreate(%q): no existing session, creating new", sessionKey)
	}

	// Enforce session cap to prevent resource exhaustion.
	maxS := p.maxSessions
	if maxS <= 0 {
		maxS = 10
	}
	if len(p.sessions) >= maxS {
		return nil, fmt.Errorf("browser pool full (%d sessions); close an existing session first", maxS)
	}

	opts := chromedp.DefaultExecAllocatorOptions[:]
	if p.headless {
		opts = append(opts, chromedp.Headless)
	}
	if p.chromePath != "" {
		opts = append(opts, chromedp.ExecPath(p.chromePath))
	}
	if p.noSandbox {
		opts = append(opts, chromedp.NoSandbox)
	}
	opts = append(opts,
		chromedp.DisableGPU,
		chromedp.Flag("disable-dev-shm-usage", true),
	)

	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), opts...)
	browserCtx, browserCancel := chromedp.NewContext(allocCtx)

	// Combined cancel with logging so we can trace unexpected session teardowns.
	combinedCancel := func() {
		blogf("combinedCancel called for session %q", sessionKey)
		browserCancel()
		allocCancel()
	}

	// Watchdog: log if browserCtx is canceled unexpectedly (Chrome crash, etc.).
	go func() {
		<-browserCtx.Done()
		blogf("browserCtx.Done for %q: err=%v", sessionKey, browserCtx.Err())
	}()

	s := &browserSession{ctx: browserCtx, cancel: combinedCancel, lastUsed: time.Now()}
	p.sessions[sessionKey] = s
	return browserCtx, nil
}

// Close removes and cancels the browser session for a key.
func (p *BrowserPool) Close(sessionKey string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s, ok := p.sessions[sessionKey]; ok {
		s.cancel()
		delete(p.sessions, sessionKey)
	}
}

// CloseAll closes all browser sessions and stops the idle reaper.
func (p *BrowserPool) CloseAll() {
	close(p.done)
	p.wg.Wait() // wait for reaper goroutine to exit
	p.mu.Lock()
	defer p.mu.Unlock()
	for key, s := range p.sessions {
		s.cancel()
		delete(p.sessions, key)
	}
}

// BrowserTool provides browser automation actions.
type BrowserTool struct {
	Pool *BrowserPool
}

type browserInput struct {
	Action    string `json:"action"`
	URL       string `json:"url,omitempty"`
	Selector  string `json:"selector,omitempty"`
	Text      string `json:"text,omitempty"`
	JS        string `json:"js,omitempty"`
	Limit     int    `json:"limit,omitempty"`     // max elements for scrape (default 50)
	TextLimit int    `json:"textLimit,omitempty"` // max chars per text/html for scrape (default 200)
}

// captureScreenshot captures a PNG screenshot from the given browser context.
func (t *BrowserTool) captureScreenshot(bCtx context.Context) ([]byte, error) {
	var buf []byte
	if err := chromedp.Run(bCtx, chromedp.CaptureScreenshot(&buf)); err != nil {
		return nil, err
	}
	const maxScreenshotBytes = 512 * 1024
	if len(buf) > maxScreenshotBytes {
		return nil, fmt.Errorf("screenshot too large (%d bytes, max %d); try 'scrape' instead", len(buf), maxScreenshotBytes)
	}
	return buf, nil
}

// CaptureScreenshot captures a PNG screenshot for the given session key.
// Returns the raw PNG bytes and the current page URL.
// Used by the MCP server to return proper image content blocks.
func (t *BrowserTool) CaptureScreenshot(ctx context.Context, sessionKey string) ([]byte, string, error) {
	bCtx, err := t.Pool.getOrCreate(sessionKey)
	if err != nil {
		return nil, "", fmt.Errorf("create browser: %w", err)
	}
	var url string
	_ = chromedp.Run(bCtx, chromedp.Location(&url))
	buf, err := t.captureScreenshot(bCtx)
	if err != nil {
		return nil, url, err
	}
	return buf, url, nil
}

func (t *BrowserTool) Name() string { return "browser" }

func (t *BrowserTool) Description() string {
	return "Control a headless Chrome browser for web scraping and crawling: navigate to URLs, click elements, type text, take screenshots, scrape page content, get interactive element snapshots, evaluate JavaScript, extract links, get full page text, read cookies, and inspect response headers."
}

func (t *BrowserTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"action": {"type": "string", "enum": ["navigate","screenshot","click","type","eval","scrape","snapshot","links","text","cookies","headers","close"], "description": "Browser action to perform"},
			"url": {"type": "string", "description": "URL to navigate to (for 'navigate')"},
			"selector": {"type": "string", "description": "CSS selector (for 'click', 'type', 'scrape')"},
			"text": {"type": "string", "description": "Text to type (for 'type')"},
			"js": {"type": "string", "description": "JavaScript to evaluate (for 'eval')"},
			"limit": {"type": "integer", "description": "Max elements to return (for 'scrape', default 50)"},
			"textLimit": {"type": "integer", "description": "Max chars per text/html field (for 'scrape', default 200)"}
		},
		"required": ["action"]
	}`)
}

func (t *BrowserTool) Run(ctx context.Context, argsJSON string) string {
	var in browserInput
	if err := json.Unmarshal([]byte(argsJSON), &in); err != nil {
		return fmt.Sprintf("error: invalid arguments: %v", err)
	}

	sessionKey, _ := ctx.Value(SessionKeyContextKey{}).(string)
	if sessionKey == "" {
		sessionKey = "browser:default"
	}
	blogf("Run action=%q sessionKey=%q", in.Action, sessionKey)

	if in.Action == "close" {
		t.Pool.Close(sessionKey)
		return "browser session closed"
	}

	bCtx, err := t.Pool.getOrCreate(sessionKey)
	if err != nil {
		return fmt.Sprintf("error: create browser: %v", err)
	}

	// Timeout safety: if an action hangs, close the session after 60s.
	// We intentionally avoid context.WithTimeout(bCtx, ...) because canceling
	// a child of the chromedp browser context kills the Chrome process,
	// breaking subsequent actions (screenshot after navigate, etc.).
	actionTimeout := time.AfterFunc(60*time.Second, func() {
		blogf("action timeout (60s): closing session %q", sessionKey)
		t.Pool.Close(sessionKey)
	})
	defer actionTimeout.Stop()

	switch in.Action {
	case "navigate":
		if in.URL == "" {
			return "error: url is required for navigate"
		}
		// Prevent SSRF — block private/reserved IPs (cloud metadata, internal services).
		if err := checkSSRF(in.URL); err != nil {
			return fmt.Sprintf("error: navigate blocked: %v", err)
		}
		var bodyText string
		err = chromedp.Run(bCtx,
			chromedp.Navigate(in.URL),
			chromedp.WaitReady("body", chromedp.ByQuery),
			// Wait for JS hydration — poll until document.readyState === 'complete' and body has real content
			chromedp.Evaluate(`new Promise(resolve => {
				if (document.readyState === 'complete') { resolve(); return; }
				window.addEventListener('load', resolve);
			})`, nil),
			chromedp.Sleep(300*time.Millisecond), // brief settle for React/Next.js hydration
			chromedp.Evaluate(`document.body.innerText || ''`, &bodyText),
		)
		if err != nil {
			blogf("navigate FAILED: %v", err)
			return fmt.Sprintf("error: navigate: %v", err)
		}
		// Verify navigation actually took effect in the browser.
		var postNavURL string
		if locErr := chromedp.Run(bCtx, chromedp.Location(&postNavURL)); locErr == nil {
			blogf("navigate OK: requested=%q actual=%q textLen=%d", in.URL, postNavURL, len(bodyText))
		} else {
			blogf("navigate: location check failed: %v", locErr)
		}
		if len(bodyText) > 8000 {
			bodyText = bodyText[:8000] + "\n[... truncated ...]"
		}
		return bodyText

	case "screenshot":
		// Log current URL to help debug "blank page" issues.
		var screenshotURL string
		_ = chromedp.Run(bCtx, chromedp.Location(&screenshotURL))
		blogf("screenshot: currentURL=%q bCtx.Err=%v", screenshotURL, bCtx.Err())

		buf, captureErr := t.captureScreenshot(bCtx)
		if captureErr != nil {
			return fmt.Sprintf("error: screenshot: %v", captureErr)
		}

		// Save to temp file so the agent doesn't waste context on base64.
		// MCP callers use CaptureScreenshot() directly for image content.
		tmpFile, tmpErr := os.CreateTemp("", "gc-screenshot-*.png")
		if tmpErr != nil {
			return fmt.Sprintf("error: screenshot temp file: %v", tmpErr)
		}
		_, _ = tmpFile.Write(buf)
		_ = tmpFile.Close()
		blogf("screenshot saved: %s (%d bytes)", tmpFile.Name(), len(buf))

		if screenshotURL == "about:blank" || screenshotURL == "" {
			return fmt.Sprintf("[WARNING: browser is on %q — did you call navigate first?]\nScreenshot saved to %s (%dKB). Use 'text' or 'scrape' to inspect page content.", screenshotURL, tmpFile.Name(), len(buf)/1024)
		}
		return fmt.Sprintf("Screenshot captured (page: %s, file: %s, %dKB). Use 'text' or 'scrape' to inspect page content programmatically.", screenshotURL, tmpFile.Name(), len(buf)/1024)

	case "click":
		if in.Selector == "" {
			return "error: selector is required for click"
		}
		err = chromedp.Run(bCtx,
			chromedp.Click(in.Selector, chromedp.ByQuery),
		)
		if err != nil {
			return fmt.Sprintf("error: click: %v", err)
		}
		return "clicked"

	case "type":
		if in.Selector == "" {
			return "error: selector is required for type"
		}
		err = chromedp.Run(bCtx,
			chromedp.Click(in.Selector, chromedp.ByQuery),
			chromedp.SendKeys(in.Selector, in.Text, chromedp.ByQuery),
		)
		if err != nil {
			return fmt.Sprintf("error: type: %v", err)
		}
		return "typed"

	case "eval":
		if in.JS == "" {
			return "error: js is required for eval"
		}
		var result any
		err = chromedp.Run(bCtx,
			chromedp.Evaluate(in.JS, &result),
		)
		if err != nil {
			return fmt.Sprintf("error: eval: %v", err)
		}
		out, _ := json.Marshal(result)
		return string(out)

	case "scrape":
		if in.Selector == "" {
			return "error: selector is required for scrape"
		}
		limit := in.Limit
		if limit <= 0 {
			limit = 50
		}
		textLimit := in.TextLimit
		if textLimit <= 0 {
			textLimit = 200
		}
		js := fmt.Sprintf(scrapeJS, fmt.Sprintf("%q", in.Selector), limit, textLimit)
		var result any
		err = chromedp.Run(bCtx, chromedp.Evaluate(js, &result))
		if err != nil {
			return fmt.Sprintf("error: scrape: %v", err)
		}
		out, _ := json.Marshal(result)
		if len(out) > 16384 {
			if idx := lastCompleteElement(out[:16384]); idx > 0 {
				out = append(out[:idx], ']')
			} else {
				out = out[:16384]
			}
			return string(out) + "\n[... truncated to 16KB ...]"
		}
		return string(out)

	case "snapshot":
		var title, location string
		var elements []map[string]interface{}
		err = chromedp.Run(bCtx,
			chromedp.Title(&title),
			chromedp.Location(&location),
			chromedp.Evaluate(snapshotJS, &elements),
		)
		if err != nil {
			return fmt.Sprintf("error: snapshot: %v", err)
		}
		out, _ := json.Marshal(map[string]interface{}{
			"title":    title,
			"url":      location,
			"elements": elements,
		})
		return string(out)

	case "links":
		limit := in.Limit
		if limit <= 0 {
			limit = 50
		}
		js := fmt.Sprintf(linksJS, limit)
		var result any
		err = chromedp.Run(bCtx, chromedp.Evaluate(js, &result))
		if err != nil {
			return fmt.Sprintf("error: links: %v", err)
		}
		out, _ := json.Marshal(result)
		return string(out)

	case "text":
		var location string
		var bodyText string
		err = chromedp.Run(bCtx,
			chromedp.Location(&location),
			chromedp.Evaluate(`document.body.innerText || ''`, &bodyText),
		)
		if err != nil {
			return fmt.Sprintf("error: text: %v", err)
		}
		const maxTextBytes = 50 * 1024
		if len(bodyText) > maxTextBytes {
			// Truncate at last newline before the limit for a clean cut.
			cut := bodyText[:maxTextBytes]
			if idx := lastNewline(cut); idx > 0 {
				cut = cut[:idx]
			}
			bodyText = cut + "\n[... truncated at 50KB ...]"
		}
		out, _ := json.Marshal(map[string]string{
			"url":  location,
			"text": bodyText,
		})
		return string(out)

	case "cookies":
		var cookies []*network.Cookie
		err = chromedp.Run(bCtx, chromedp.ActionFunc(func(ctx context.Context) error {
			var e error
			cookies, e = network.GetCookies().Do(ctx)
			return e
		}))
		if err != nil {
			return fmt.Sprintf("error: cookies: %v", err)
		}
		type cookieInfo struct {
			Name     string  `json:"name"`
			Value    string  `json:"value"`
			Domain   string  `json:"domain"`
			Path     string  `json:"path"`
			Expires  float64 `json:"expires"`
			HTTPOnly bool    `json:"httpOnly"`
			Secure   bool    `json:"secure"`
		}
		infos := make([]cookieInfo, len(cookies))
		for i, c := range cookies {
			infos[i] = cookieInfo{
				Name:     c.Name,
				Value:    c.Value,
				Domain:   c.Domain,
				Path:     c.Path,
				Expires:  c.Expires,
				HTTPOnly: c.HTTPOnly,
				Secure:   c.Secure,
			}
		}
		out, _ := json.Marshal(infos)
		return string(out)

	case "headers":
		var result any
		err = chromedp.Run(bCtx, chromedp.Evaluate(headersJS, &result))
		if err != nil {
			return fmt.Sprintf("error: headers: %v", err)
		}
		out, _ := json.Marshal(result)
		return string(out)

	default:
		return fmt.Sprintf("error: unknown action %q (use navigate, screenshot, click, type, eval, scrape, snapshot, links, text, cookies, headers, close)", in.Action)
	}
}

// scrapeJS is the JavaScript template for the scrape action.
// Parameters: %q selector, %d limit, %d textLimit.
const scrapeJS = `(function() {
	var selector = %s;
	var limit = %d;
	var textLimit = %d;

	var els = Array.from(document.querySelectorAll(selector)).slice(0, limit);
	var elSet = new Set(els);
	var indexMap = new Map();
	els.forEach(function(el, i) { indexMap.set(el, i); });

	function getDepthAndParent(el) {
		var depth = 0;
		var parentIdx = -1;
		var node = el.parentElement;
		while (node) {
			if (elSet.has(node)) {
				depth++;
				if (parentIdx === -1) parentIdx = indexMap.get(node);
			}
			node = node.parentElement;
		}
		return { depth: depth, parentIndex: parentIdx };
	}

	function truncate(s, max) {
		if (!s) return "";
		s = s.trim();
		return s.length > max ? s.substring(0, max) + "..." : s;
	}

	function getAttrs(el) {
		var want = ["role","aria-label","data-testid","class","id","href","src","alt","title","name","type"];
		var result = {};
		want.forEach(function(name) {
			var v = el.getAttribute(name);
			if (v != null && v !== "") {
				if (name === "class" && v.length > 100) v = v.substring(0, 100) + "...";
				result[name] = v;
			}
		});
		return result;
	}

	function getIndentation(el) {
		try {
			var cs = window.getComputedStyle(el);
			return Math.round((parseFloat(cs.marginLeft) || 0) + (parseFloat(cs.paddingLeft) || 0));
		} catch(e) { return 0; }
	}

	return els.map(function(el, i) {
		var dp = getDepthAndParent(el);
		var rect = el.getBoundingClientRect();
		return {
			index: i,
			text: truncate(el.innerText, textLimit),
			html: truncate(el.innerHTML, textLimit),
			attrs: getAttrs(el),
			depth: dp.depth,
			parentIndex: dp.parentIndex,
			rect: { x: Math.round(rect.x), y: Math.round(rect.y), width: Math.round(rect.width), height: Math.round(rect.height) },
			marginLeft: getIndentation(el)
		};
	});
})();`

// snapshotJS returns interactive elements on the page with CSS selectors.
const snapshotJS = `(function() {
	const selectors = 'a, button, input, select, textarea, [role="button"], [role="link"], [role="tab"], [onclick], [tabindex]';
	const nodes = document.querySelectorAll(selectors);
	const results = [];
	const seen = new Set();

	for (const el of nodes) {
		if (results.length >= 50) break;

		const rect = el.getBoundingClientRect();
		if (rect.width === 0 && rect.height === 0) continue;

		const style = window.getComputedStyle(el);
		if (style.display === 'none' || style.visibility === 'hidden' || style.opacity === '0') continue;

		let selector = '';
		if (el.id) {
			selector = '#' + CSS.escape(el.id);
		} else if (el.getAttribute('data-testid')) {
			selector = '[data-testid="' + el.getAttribute('data-testid') + '"]';
		} else if (el.getAttribute('data-cy')) {
			selector = '[data-cy="' + el.getAttribute('data-cy') + '"]';
		} else if (el.name && el.tagName !== 'A') {
			selector = el.tagName.toLowerCase() + '[name="' + el.name + '"]';
		} else if (el.type && el.tagName === 'INPUT') {
			const inputs = document.querySelectorAll('input[type="' + el.type + '"]');
			if (inputs.length === 1) {
				selector = 'input[type="' + el.type + '"]';
			} else {
				const idx = Array.from(inputs).indexOf(el);
				selector = 'input[type="' + el.type + '"]:nth-of-type(' + (idx + 1) + ')';
			}
		} else if (el.className && typeof el.className === 'string' && el.className.trim()) {
			const cls = el.className.trim().split(/\s+/)[0];
			const matches = document.querySelectorAll(el.tagName.toLowerCase() + '.' + CSS.escape(cls));
			if (matches.length === 1) {
				selector = el.tagName.toLowerCase() + '.' + CSS.escape(cls);
			}
		}

		if (!selector) {
			const tag = el.tagName.toLowerCase();
			const parent = el.parentElement;
			if (parent) {
				const siblings = parent.querySelectorAll(':scope > ' + tag);
				if (siblings.length === 1) {
					const parentSel = parent.id ? '#' + CSS.escape(parent.id) : parent.tagName.toLowerCase();
					selector = parentSel + ' > ' + tag;
				} else {
					const idx = Array.from(siblings).indexOf(el) + 1;
					const parentSel = parent.id ? '#' + CSS.escape(parent.id) : parent.tagName.toLowerCase();
					selector = parentSel + ' > ' + tag + ':nth-child(' + idx + ')';
				}
			} else {
				selector = tag;
			}
		}

		if (seen.has(selector)) continue;
		seen.add(selector);

		const text = (el.textContent || '').trim().substring(0, 100);
		const tag = el.tagName.toLowerCase();
		let elType = tag;
		if (tag === 'input') elType = 'input';
		else if (tag === 'a') elType = 'link';
		else if (tag === 'button' || el.getAttribute('role') === 'button') elType = 'button';
		else if (tag === 'select') elType = 'select';
		else if (tag === 'textarea') elType = 'textarea';

		const item = {
			tag: tag,
			selector: selector,
			type: elType,
			visible: true
		};
		if (text) item.text = text;
		if (el.placeholder) item.placeholder = el.placeholder;
		if (el.href) item.href = el.href;

		results.push(item);
	}
	return results;
})()`

// linksJS extracts all <a> tags with href, text, and a smart CSS selector.
// Parameter: %d limit.
const linksJS = `(function() {
	var limit = %d;
	var anchors = document.querySelectorAll('a[href]');
	var results = [];
	for (var i = 0; i < anchors.length && results.length < limit; i++) {
		var el = anchors[i];
		var href = el.getAttribute('href');
		if (!href || href === '') continue;

		var text = (el.innerText || '').trim().substring(0, 200);

		var selector = '';
		if (el.id) {
			selector = '#' + CSS.escape(el.id);
		} else if (el.getAttribute('data-testid')) {
			selector = 'a[data-testid="' + el.getAttribute('data-testid') + '"]';
		} else if (el.className && typeof el.className === 'string' && el.className.trim()) {
			var cls = el.className.trim().split(/\s+/)[0];
			var matches = document.querySelectorAll('a.' + CSS.escape(cls));
			if (matches.length === 1) {
				selector = 'a.' + CSS.escape(cls);
			}
		}
		if (!selector) {
			var parent = el.parentElement;
			if (parent) {
				var siblings = parent.querySelectorAll(':scope > a');
				if (siblings.length === 1) {
					var pSel = parent.id ? '#' + CSS.escape(parent.id) : parent.tagName.toLowerCase();
					selector = pSel + ' > a';
				} else {
					var idx = Array.from(siblings).indexOf(el) + 1;
					var pSel = parent.id ? '#' + CSS.escape(parent.id) : parent.tagName.toLowerCase();
					selector = pSel + ' > a:nth-child(' + idx + ')';
				}
			} else {
				selector = 'a';
			}
		}

		results.push({ href: el.href, text: text, selector: selector });
	}
	return results;
})()`

// headersJS uses the Performance API to retrieve response headers from the
// last navigation. This approach works without needing to enable CDP network
// domain tracking before the navigation occurs.
const headersJS = `(function() {
	var entries = performance.getEntriesByType('navigation');
	if (!entries || entries.length === 0) {
		return { status: 0, headers: {}, url: location.href, error: "no navigation entry" };
	}
	var nav = entries[0];
	var headers = {};
	if (nav.serverTiming) {
		nav.serverTiming.forEach(function(st) {
			headers['server-timing-' + st.name] = st.description || String(st.duration);
		});
	}
	// responseStatus is available in modern browsers via PerformanceNavigationTiming
	var status = nav.responseStatus || 0;

	// Transfer size info
	headers['transfer-size'] = String(nav.transferSize || 0);
	headers['encoded-body-size'] = String(nav.encodedBodySize || 0);
	headers['decoded-body-size'] = String(nav.decodedBodySize || 0);

	// Also grab content-type from document
	var ct = document.contentType || '';
	if (ct) headers['content-type'] = ct;

	return { status: status, headers: headers, url: location.href };
})()`

// lastNewline returns the index of the last newline in s, or -1.
func lastNewline(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '\n' {
			return i
		}
	}
	return -1
}

// lastCompleteElement finds the byte offset after the last complete JSON
// object closing brace in data, for truncating a JSON array cleanly.
func lastCompleteElement(data []byte) int {
	for i := len(data) - 1; i >= 0; i-- {
		if data[i] == '}' {
			return i + 1
		}
	}
	return -1
}

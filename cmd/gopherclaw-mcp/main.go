package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/EMSERO/gopherclaw/internal/config"
	"github.com/EMSERO/gopherclaw/internal/eidetic"
	"github.com/EMSERO/gopherclaw/internal/embeddings"
	"github.com/EMSERO/gopherclaw/internal/tools"
)

// version, commit, and date are injected via ldflags at build time.
// goreleaser sets these automatically; for manual builds use:
//
//	go build -ldflags "-X main.version=0.4.0 -X main.commit=$(git rev-parse --short HEAD) -X main.date=$(date -u +%Y-%m-%dT%H:%M:%SZ)" ./cmd/gopherclaw-mcp
var (
	version = "0.4.0-dev"
	commit  = "unknown"
	date    = "unknown"
)

// logger is the package-level file logger (nil if logging fails to init).
var logger *log.Logger

func initLogging(cfg *config.Root) {
	// Log to the same directory as the main GopherClaw logs.
	logDir := ""
	if cfg.Logging.File != "" {
		logDir = filepath.Dir(cfg.Logging.File)
	}
	if logDir == "" {
		home, _ := os.UserHomeDir()
		logDir = filepath.Join(home, ".gopherclaw", "logs")
	}
	_ = os.MkdirAll(logDir, 0755)
	logPath := filepath.Join(logDir, "gopherclaw-mcp.log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mcp: failed to open log file %s: %v\n", logPath, err)
		return
	}
	logger = log.New(f, "", log.LstdFlags)
	logger.Printf("gopherclaw-mcp started (pid=%d)", os.Getpid())
}

func logf(format string, args ...any) {
	if logger != nil {
		logger.Printf(format, args...)
	}
}

func main() {
	configPath := flag.String("config", "", "path to gopherclaw config.json")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	initLogging(cfg)

	s := server.NewMCPServer("gopherclaw", version,
		server.WithToolCapabilities(false),
	)

	logf("gopherclaw-mcp %s (commit=%s, date=%s)", version, commit, date)
	logf("config loaded from %s", cfg.Path)

	// --- Browser tools ---
	if cfg.Tools.Browser.Enabled {
		logf("registering browser tools (headless=%v)", cfg.Tools.Browser.IsHeadless())
		pool := tools.NewBrowserPool(
			cfg.Tools.Browser.IsHeadless(),
			cfg.Tools.Browser.ChromePath,
			cfg.Tools.Browser.NoSandbox,
			cfg.Tools.Browser.Width,
			cfg.Tools.Browser.Height,
		)
		registerBrowserTools(s, pool)
	}

	// --- Eidetic memory tools ---
	if cfg.Eidetic.Enabled {
		logf("registering eidetic tools (baseURL=%s)", cfg.Eidetic.BaseURL)
		client := eidetic.New(eidetic.Config{
			BaseURL:         cfg.Eidetic.BaseURL,
			APIKey:          cfg.Eidetic.APIKey,
			AgentID:         cfg.Eidetic.AgentID,
			RecentLimit:     cfg.Eidetic.RecentLimit,
			SearchLimit:     cfg.Eidetic.SearchLimit,
			SearchThreshold: cfg.Eidetic.SearchThreshold,
			TimeoutSeconds:  cfg.Eidetic.TimeoutSeconds,
		})
		var embedClient *embeddings.Client
		if cfg.Eidetic.Embeddings.Enabled {
			embedClient = embeddings.New(embeddings.Config{
				Provider:   cfg.Eidetic.Embeddings.Provider,
				Model:      cfg.Eidetic.Embeddings.Model,
				BaseURL:    cfg.Eidetic.Embeddings.BaseURL,
				APIKey:     cfg.Eidetic.Embeddings.APIKey,
				Dimensions: cfg.Eidetic.Embeddings.Dimensions,
			})
		}
		registerEideticTools(s, client, embedClient, cfg)
	}

	// --- Notify tool (via gateway HTTP API) ---
	if cfg.Gateway.Port > 0 {
		logf("registering notify tool (gateway=http://127.0.0.1:%d)", cfg.Gateway.Port)
		gatewayURL := fmt.Sprintf("http://127.0.0.1:%d", cfg.Gateway.Port)
		registerNotifyTool(s, gatewayURL, cfg.Gateway.Auth.Token, cfg.Gateway.ControlUI.BasePath)
	}

	// --- Web tools ---
	logf("registering web tools")
	registerWebTools(s)

	// --- File tools ---
	workspace := cfg.Agents.Defaults.Workspace
	logf("registering file tools (workspace=%s)", workspace)
	registerFileTools(s, workspace)

	// --- Memory tools ---
	if cfg.Agents.Defaults.Memory.Enabled {
		logf("registering memory tools (workspace=%s)", workspace)
		registerMemoryTools(s, workspace)
	}

	if err := server.ServeStdio(s); err != nil {
		fmt.Fprintf(os.Stderr, "mcp server error: %v\n", err)
		os.Exit(1)
	}
}

// ---------------------------------------------------------------------------
// Browser tools
// ---------------------------------------------------------------------------

func registerBrowserTools(s *server.MCPServer, pool *tools.BrowserPool) {
	// We expose the browser as individual MCP tools rather than one tool with
	// an "action" parameter, since MCP tools work best as discrete operations.

	sessionKey := "mcp-default" // MCP doesn't have sessions; use a single shared one

	// browser_navigate
	s.AddTool(
		mcp.NewTool("browser_navigate",
			mcp.WithDescription("Navigate the browser to a URL and return the page text"),
			mcp.WithString("url", mcp.Required(), mcp.Description("The URL to navigate to")),
			mcp.WithNumber("width", mcp.Description("Viewport width in pixels (optional, default 1280)")),
			mcp.WithNumber("height", mcp.Description("Viewport height in pixels (optional, default 900)")),
		),
		browserHandler(pool, sessionKey, "navigate"),
	)

	// browser_screenshot
	s.AddTool(
		mcp.NewTool("browser_screenshot",
			mcp.WithDescription("Take a screenshot of the current browser page. Returns a base64-encoded PNG."),
			mcp.WithNumber("width", mcp.Description("Viewport width in pixels (optional, default 1280)")),
			mcp.WithNumber("height", mcp.Description("Viewport height in pixels (optional, default 900)")),
		),
		browserHandler(pool, sessionKey, "screenshot"),
	)

	// browser_click
	s.AddTool(
		mcp.NewTool("browser_click",
			mcp.WithDescription("Click an element on the page by CSS selector"),
			mcp.WithString("selector", mcp.Required(), mcp.Description("CSS selector of the element to click")),
		),
		browserHandler(pool, sessionKey, "click"),
	)

	// browser_type
	s.AddTool(
		mcp.NewTool("browser_type",
			mcp.WithDescription("Type text into an input element"),
			mcp.WithString("selector", mcp.Required(), mcp.Description("CSS selector of the input element")),
			mcp.WithString("text", mcp.Required(), mcp.Description("Text to type")),
		),
		browserHandler(pool, sessionKey, "type"),
	)

	// browser_eval
	s.AddTool(
		mcp.NewTool("browser_eval",
			mcp.WithDescription("Execute JavaScript in the browser page and return the result"),
			mcp.WithString("js", mcp.Required(), mcp.Description("JavaScript code to evaluate")),
		),
		browserHandler(pool, sessionKey, "eval"),
	)

	// browser_scrape
	s.AddTool(
		mcp.NewTool("browser_scrape",
			mcp.WithDescription("Scrape elements matching a CSS selector, returning tag, text, and attributes"),
			mcp.WithString("selector", mcp.Required(), mcp.Description("CSS selector to scrape")),
			mcp.WithNumber("limit", mcp.Description("Maximum number of elements to return (optional)")),
			mcp.WithNumber("textLimit", mcp.Description("Maximum text length per element (optional)")),
		),
		browserHandler(pool, sessionKey, "scrape"),
	)

	// browser_snapshot
	s.AddTool(
		mcp.NewTool("browser_snapshot",
			mcp.WithDescription("Get an accessibility snapshot of the current page (title, URL, interactive elements)"),
		),
		browserHandler(pool, sessionKey, "snapshot"),
	)

	// browser_links
	s.AddTool(
		mcp.NewTool("browser_links",
			mcp.WithDescription("Get all links on the current page"),
			mcp.WithNumber("limit", mcp.Description("Maximum number of links to return (optional)")),
		),
		browserHandler(pool, sessionKey, "links"),
	)

	// browser_text
	s.AddTool(
		mcp.NewTool("browser_text",
			mcp.WithDescription("Get the full text content of the current page"),
		),
		browserHandler(pool, sessionKey, "text"),
	)

	// browser_cookies
	s.AddTool(
		mcp.NewTool("browser_cookies",
			mcp.WithDescription("Get all cookies for the current page"),
		),
		browserHandler(pool, sessionKey, "cookies"),
	)
}

// browserHandler returns an MCP handler that delegates to the BrowserTool's Run method.
func browserHandler(pool *tools.BrowserPool, sessionKey, action string) server.ToolHandlerFunc {
	bt := &tools.BrowserTool{Pool: pool}
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		logf("tool call: browser_%s", action)

		// Build args for Run().
		args := make(map[string]any)
		args["action"] = action
		if params, ok := request.Params.Arguments.(map[string]any); ok {
			for k, v := range params {
				args[k] = v
			}
		}
		argsJSON, _ := json.Marshal(args)

		// Inject session key into context for browser pool.
		ctx = context.WithValue(ctx, tools.SessionKeyContextKey{}, sessionKey)

		// Screenshot: apply viewport override if requested, then capture image.
		if action == "screenshot" {
			w, _ := args["width"].(float64)
			h, _ := args["height"].(float64)
			if w > 0 || h > 0 {
				bt.SetViewport(ctx, sessionKey, int(w), int(h))
			}
			buf, pageURL, err := bt.CaptureScreenshot(ctx, sessionKey)
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("error: screenshot: %v", err)), nil
			}
			b64 := base64.StdEncoding.EncodeToString(buf)
			content := []mcp.Content{mcp.NewImageContent(b64, "image/png")}
			if pageURL != "" {
				content = append(content, mcp.NewTextContent(fmt.Sprintf("Page: %s", pageURL)))
			}
			return &mcp.CallToolResult{Content: content}, nil
		}

		result := bt.Run(ctx, string(argsJSON))

		if strings.HasPrefix(result, "error:") {
			return mcp.NewToolResultError(result), nil
		}
		return mcp.NewToolResultText(result), nil
	}
}

// ---------------------------------------------------------------------------
// Eidetic memory tools
// ---------------------------------------------------------------------------

func registerEideticTools(s *server.MCPServer, client eidetic.Client, embedClient *embeddings.Client, cfg *config.Root) {
	searchTool := &tools.EideticSearchTool{
		Client:     client,
		Embeddings: embedClient,
		Limit:      cfg.Eidetic.SearchLimit,
		Threshold:  cfg.Eidetic.SearchThreshold,
	}
	appendTool := &tools.EideticAppendTool{
		Client:     client,
		Embeddings: embedClient,
		AgentID:    cfg.Eidetic.AgentID,
	}

	s.AddTool(
		mcp.NewTool("eidetic_search",
			mcp.WithDescription(searchTool.Description()),
			mcp.WithString("query", mcp.Required(), mcp.Description("Natural language query to search memories")),
			mcp.WithNumber("limit", mcp.Description("Maximum number of results (optional)")),
			mcp.WithNumber("threshold", mcp.Description("Minimum cosine similarity 0–1 (optional)")),
		),
		wrapGopherClawTool(searchTool),
	)

	s.AddTool(
		mcp.NewTool("eidetic_append",
			mcp.WithDescription(appendTool.Description()),
			mcp.WithString("content", mcp.Required(), mcp.Description("Memory content to store (max 4000 chars)")),
		),
		wrapGopherClawTool(appendTool),
	)
}

// ---------------------------------------------------------------------------
// Notify tool (via gateway HTTP)
// ---------------------------------------------------------------------------

func registerNotifyTool(s *server.MCPServer, gatewayURL, authToken, basePath string) {
	s.AddTool(
		mcp.NewTool("notify_user",
			mcp.WithDescription("Send a notification message to the user on their active channel (Telegram/Discord/Slack) via the GopherClaw gateway"),
			mcp.WithString("message", mcp.Required(), mcp.Description("The message to send to the user")),
			mcp.WithString("session", mcp.Description("Session key to deliver to (optional, defaults to most recent)")),
		),
		func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			msg, _ := request.RequireString("message")
			if msg == "" {
				return mcp.NewToolResultError("message is required"), nil
			}
			session := ""
			if s, ok := request.Params.Arguments.(map[string]any)["session"].(string); ok {
				session = s
			}

			payload, err := json.Marshal(map[string]string{
				"message": msg,
				"session": session,
			})
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("failed to marshal payload: %v", err)), nil
			}

			notifyURL := gatewayURL + basePath + "/api/notify"
			req, err := http.NewRequestWithContext(ctx, "POST", notifyURL, strings.NewReader(string(payload)))
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("failed to create request: %v", err)), nil
			}
			req.Header.Set("Content-Type", "application/json")
			if authToken != "" {
				req.Header.Set("Authorization", "Bearer "+authToken)
			}

			client := &http.Client{Timeout: 30 * time.Second}
			resp, err := client.Do(req)
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("gateway request failed: %v", err)), nil
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				return mcp.NewToolResultError(fmt.Sprintf("gateway returned %d", resp.StatusCode)), nil
			}
			return mcp.NewToolResultText("notification delivered"), nil
		},
	)
}

// ---------------------------------------------------------------------------
// Helper: wrap a GopherClaw Tool as an MCP handler
// ---------------------------------------------------------------------------

type gcTool interface {
	Run(ctx context.Context, argsJSON string) string
}

func wrapGopherClawTool(t gcTool) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		argsJSON, _ := json.Marshal(request.Params.Arguments)
		logf("tool call: %T args=%s", t, string(argsJSON))
		result := t.Run(ctx, string(argsJSON))
		if strings.HasPrefix(result, "error:") {
			return mcp.NewToolResultError(result), nil
		}
		return mcp.NewToolResultText(result), nil
	}
}

// ---------------------------------------------------------------------------
// Web tools
// ---------------------------------------------------------------------------

func registerWebTools(s *server.MCPServer) {
	s.AddTool(
		mcp.NewTool("web_search",
			mcp.WithDescription("Search the web using a query string"),
			mcp.WithString("query", mcp.Required(), mcp.Description("Search query")),
			mcp.WithNumber("limit", mcp.Description("Maximum number of results (default 5)")),
		),
		wrapGopherClawTool(&tools.WebSearchTool{}),
	)

	s.AddTool(
		mcp.NewTool("web_fetch",
			mcp.WithDescription("Fetch a web page and return its text content"),
			mcp.WithString("url", mcp.Required(), mcp.Description("URL to fetch")),
			mcp.WithNumber("maxLength", mcp.Description("Maximum response length in characters (optional)")),
		),
		wrapGopherClawTool(&tools.WebFetchTool{}),
	)
}

// ---------------------------------------------------------------------------
// File tools
// ---------------------------------------------------------------------------

func registerFileTools(s *server.MCPServer, workspace string) {
	var allowPaths []string
	if workspace != "" {
		allowPaths = []string{workspace}
	}

	s.AddTool(
		mcp.NewTool("read_file",
			mcp.WithDescription("Read the contents of a file"),
			mcp.WithString("path", mcp.Required(), mcp.Description("File path (relative to workspace or absolute)")),
			mcp.WithNumber("offset", mcp.Description("Line offset to start reading from (optional)")),
			mcp.WithNumber("limit", mcp.Description("Maximum number of lines to read (optional)")),
		),
		wrapGopherClawTool(&tools.ReadFileTool{AllowPaths: allowPaths}),
	)

	s.AddTool(
		mcp.NewTool("write_file",
			mcp.WithDescription("Write content to a file (creates or overwrites)"),
			mcp.WithString("path", mcp.Required(), mcp.Description("File path (relative to workspace or absolute)")),
			mcp.WithString("content", mcp.Required(), mcp.Description("Content to write")),
		),
		wrapGopherClawTool(&tools.WriteFileTool{AllowPaths: allowPaths}),
	)

	s.AddTool(
		mcp.NewTool("list_dir",
			mcp.WithDescription("List files and directories in a path"),
			mcp.WithString("path", mcp.Required(), mcp.Description("Directory path (relative to workspace or absolute)")),
		),
		wrapGopherClawTool(&tools.ListDirTool{AllowPaths: allowPaths}),
	)
}

// ---------------------------------------------------------------------------
// Memory tools
// ---------------------------------------------------------------------------

func registerMemoryTools(s *server.MCPServer, workspace string) {
	s.AddTool(
		mcp.NewTool("memory_append",
			mcp.WithDescription("Append a memory entry to the workspace MEMORY.md file"),
			mcp.WithString("content", mcp.Required(), mcp.Description("Content to append to memory")),
		),
		wrapGopherClawTool(&tools.MemoryAppendTool{Workspace: workspace}),
	)

	s.AddTool(
		mcp.NewTool("memory_get",
			mcp.WithDescription("Read the workspace MEMORY.md file"),
		),
		wrapGopherClawTool(&tools.MemoryGetTool{Workspace: workspace}),
	)
}

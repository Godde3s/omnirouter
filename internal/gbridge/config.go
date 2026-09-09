// Configuration for the Gemini bridge (package gbridge).
// .env-first with sane defaults, ported from the Qwen/GLM-Free-API architecture.

package gbridge

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// ============================================================================
// CONFIGURATION
// ============================================================================

// Upstream endpoints (Google Gemini web app).
var (
	URL_INIT          = "https://gemini.google.com/app"
	URL_GENERATE      = "https://gemini.google.com/_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate"
	URL_BATCH_EXECUTE = "https://gemini.google.com/_/BardChatUi/data/batchexecute"
	URL_ROTATE        = "https://accounts.google.com/RotateCookies"
	URL_UPLOAD        = "https://content-push.googleapis.com/upload"
	URL_GOOGLE        = "https://www.google.com"
)

// ---------- Config struct ----------

type Config struct {
	Server struct {
		Port int
		Host string
	}
	Auth struct {
		Enabled bool
		Token   string
	}
	Timeouts struct {
		Default int
	}
	// GeminiCookies holds the multi-account pool. GEMINI_COOKIES accepts:
	//   - JSON list: [{"psid":"...","psidts":"..."}]
	//   - cookie header: "__Secure-1PSID=...; __Secure-1PSIDTS=..."
	//   - single PSID value
	// GEMINI_COOKIES_FILE points to a JSON file with the same list shape.
	GeminiCookies    string
	GeminiCookiesFle string
	// AccountQueueTimeout bounds (seconds) how long a request waits in the
	// queue when EVERY account is rate-limited/dead before a 503 is
	// returned (ACCOUNT_QUEUE_TIMEOUT, default 120; 0 waits indefinitely).
	AccountQueueTimeout int
	// AccountCooldownBase is the first 429 cooldown in seconds; subsequent
	// consecutive 429s double it (cap 30m) (ACCOUNT_COOLDOWN_BASE, default 120).
	AccountCooldownBase int
	AgentMode           bool
	// AgentModeVariant selects the agent-mode compatibility shim:
	//   "modern" (default) — XML-sectioned prompt shim (see agent.go)
	//   "legacy"           — the original [ROLE: ...] rewrite shim
	AgentModeVariant string
	Logging          struct {
		Level  string
		Format string
	}
	KnownModels []string
	// THINKING default for requests that do not state a preference
	// (default on; per-request reasoning:false wins).
	Thinking bool
	// InitTokenTTL seconds for caching SNlM0e/bl/f.sid per account
	// (INIT_TOKEN_TTL, default 1800 = 30 minutes).
	InitTokenTTL int
}

// loadDotEnv reads a `.env` file from the working directory (if present) and
// injects its KEY=VALUE pairs into the process environment. Existing env vars
// always win, so real environment overrides still work.
func loadDotEnv() {
	f, err := os.Open(".env")
	if err != nil {
		return // no .env — perfectly fine
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ") // tolerate `export KEY=V`
		eq := strings.Index(line, "=")
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		if i := strings.Index(val, " #"); i >= 0 { // trailing comment
			val = strings.TrimSpace(val[:i])
		}
		if len(val) >= 2 && (val[0] == '"' || val[0] == '\'') && val[len(val)-1] == val[0] {
			val = val[1 : len(val)-1]
		}
		if key == "" {
			continue
		}
		if os.Getenv(key) == "" { // real env wins over .env
			_ = os.Setenv(key, val)
		}
	}
}

func loadConfig() *Config {
	loadDotEnv()

	c := &Config{}
	c.Server.Port = 8080
	c.Server.Host = "0.0.0.0"
	c.Auth.Enabled = true
	c.Auth.Token = "gemini"
	c.Timeouts.Default = 300000
	c.GeminiCookies = ""
	c.GeminiCookiesFle = ""
	c.AccountQueueTimeout = 120
	c.AccountCooldownBase = 120
	c.AgentMode = false
	c.AgentModeVariant = "modern"
	c.Logging.Level = "debug"
	c.Logging.Format = "text"
	c.KnownModels = []string{"gemini-3.6-flash", "gemini-3.5-flash-lite", "gemini-3.1-pro"}
	c.Thinking = true
	c.InitTokenTTL = 1800

	if p := os.Getenv("PORT"); p != "" {
		if n, err := strconv.Atoi(p); err == nil {
			c.Server.Port = n
		}
	}
	if h := os.Getenv("HOST"); h != "" {
		c.Server.Host = h
	}
	if t := os.Getenv("AUTH_TOKEN"); t != "" {
		c.Auth.Token = t
	}
	if t := os.Getenv("TIMEOUT"); t != "" {
		if n, err := strconv.Atoi(t); err == nil {
			c.Timeouts.Default = n
		}
	}
	if t := os.Getenv("GEMINI_COOKIES"); t != "" {
		c.GeminiCookies = t
	}
	if t := os.Getenv("GEMINI_COOKIES_FILE"); t != "" {
		c.GeminiCookiesFle = t
	}
	if t := os.Getenv("ACCOUNT_QUEUE_TIMEOUT"); t != "" {
		if n, err := strconv.Atoi(t); err == nil && n >= 0 {
			c.AccountQueueTimeout = n
		}
	}
	if t := os.Getenv("ACCOUNT_COOLDOWN_BASE"); t != "" {
		if n, err := strconv.Atoi(t); err == nil && n > 0 {
			c.AccountCooldownBase = n
		}
	}
	if am := os.Getenv("AGENT_MODE"); am != "" {
		switch strings.ToLower(am) {
		case "1", "true", "yes", "on", "modern":
			c.AgentMode = true
		case "legacy":
			c.AgentMode = true
			c.AgentModeVariant = "legacy"
		case "0", "false", "no", "off":
			c.AgentMode = false
		}
	}
	if v := os.Getenv("AGENT_MODE_VARIANT"); v != "" {
		switch strings.ToLower(v) {
		case "legacy":
			c.AgentModeVariant = "legacy"
		case "modern":
			c.AgentModeVariant = "modern"
		}
	}
	if l := os.Getenv("LOG_LEVEL"); l != "" {
		c.Logging.Level = l
	}
	if f := os.Getenv("LOG_FORMAT"); f != "" {
		c.Logging.Format = f
	}
	if t := os.Getenv("THINKING"); t != "" {
		switch strings.ToLower(t) {
		case "1", "true", "yes", "on":
			c.Thinking = true
		case "0", "false", "no", "off":
			c.Thinking = false
		}
	}
	if t := os.Getenv("INIT_TOKEN_TTL"); t != "" {
		if n, err := strconv.Atoi(t); err == nil && n > 0 {
			c.InitTokenTTL = n
		}
	}
	return c
}

var config = loadConfig()

// agentModern reports whether the modern agent-mode shim is active.
func (c *Config) agentModern() bool {
	return c.AgentMode && !strings.EqualFold(c.AgentModeVariant, "legacy")
}

// agentLegacy reports whether the legacy agent-mode shim is active.
func (c *Config) agentLegacy() bool {
	return c.AgentMode && strings.EqualFold(c.AgentModeVariant, "legacy")
}

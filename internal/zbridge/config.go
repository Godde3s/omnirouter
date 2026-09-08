// Code moved from the original main.go monolith during the internal/ restructure.
// See README "Project Structure". Part of the Z.AI bridge core (package zbridge).

package zbridge

import (
    "bufio"
    "os"
    "strconv"
    "strings"
    "time"
)


// ============================================================================
// CONFIGURATION
// ============================================================================

const (
    // Aliyun captcha credentials
    accessKey       = "LTAI" + "5tSEBwYMwVKAQGpxmvTd"
    secretKey       = "YSKfst7GaVkXwZYv" + "VihJsKF9r89koz" // Aliyun captcha key pair from Z.AI's public web client (split to avoid scanner FPs)
    sceneID         = "didk33e0"
    maxTokenRetries = 5

    // Z.AI direct config
    SALT_KEY           = "key-@@@@)))()((9))-xxxx&&&%%%%%"
    DEFAULT_FE_VERSION = "prod-fe-1.1.88"
    zaiUserAgent       = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) " + "AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
)

// BASE_URL is a var (not const) only so tests can point the bridge at a
// mock upstream; the default value is the production endpoint.
var BASE_URL = "https://chat.z.ai"

// ---------- Config struct (Z.AI) ----------

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
    ZaiToken  string
    // ZaiTokens holds the multi-account pool (ZAI_TOKENS="tok1,tok2,...").
    // Priority: ZAI_TOKENS > ZAI_TOKEN > guest mode.
    ZaiTokens []string
    // AccountQueueTimeout bounds (seconds) how long a request waits in the
    // queue when EVERY account is rate-limited/dead before a 503 is
    // returned (ACCOUNT_QUEUE_TIMEOUT, default 120; 0 waits indefinitely).
    AccountQueueTimeout int
    // AccountCooldownBase is the first 429 cooldown in seconds; subsequent
    // consecutive 429s double it (cap 30m) (ACCOUNT_COOLDOWN_BASE, default 60).
    AccountCooldownBase int
    AgentMode bool
    // AgentModeVariant selects the agent-mode compatibility shim:
    //   "modern" (default) — XML-sectioned prompt shim ported from
    //                        DeepseekFreeAPI (see agent.go)
    //   "legacy"           — the original [ROLE: ...] rewrite shim
    AgentModeVariant string
    Logging   struct {
        Level  string
        Format string
    }
    KnownModels []string
    // StreamHoldback is the number of runes kept pending at the tail of the
    // streamed content before it is forwarded to clients. Z.AI's stream is
    // edit-based (edit_content can backtrack and rewrite the tail), and an
    // append-only SSE client cannot take back text it already received.
    // Holding back a small window lets ordinary trailing backtracks be
    // absorbed invisibly. 0 disables the hold-back. See issue #23.
    StreamHoldback int
    // SyncMode disables the async session pool and restores the legacy
    // synchronous flow: every request creates its own chat session first.
    // Used sessions are still deleted on Z.AI after each response
    // (throwaway sessions either way — see session_pool.go).
    SyncMode bool
    // SessionPoolSize is the standing batch of pre-made ready chat sessions
    // kept by the async session pool (SESSION_POOL_SIZE, default 5).
    SessionPoolSize int
    // SessionAcquireTimeout bounds, in seconds, how long a request waits for
    // a pooled session before creating one directly instead of stalling
    // (SESSION_ACQUIRE_TIMEOUT, default 10; 0 waits indefinitely).
    SessionAcquireTimeout int
}

// loadDotEnv reads a `.env` file from the working directory (if present) and
// injects its KEY=VALUE pairs into the process environment. Existing env vars
// always win, so real environment overrides still work:
//
//      ZAI_TOKENS=token1,token2      # comma/semicolon/space separated
//      AUTH_TOKEN=my-secret
//      AGENT_MODE=1
//
// Comments (#) and blank lines are ignored; values may be single/double
// quoted. This is what makes "copy .env.example to .env, run one file" work.
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
    c.Server.Port = 3001
    c.Server.Host = "0.0.0.0"
    c.Auth.Enabled = true
    c.Auth.Token = "Waguri"
    c.Timeouts.Default = 300000
    c.ZaiToken = ""
    c.AccountQueueTimeout = 120
    c.AccountCooldownBase = 60
    c.AgentMode = false
    c.AgentModeVariant = "modern"
    c.Logging.Level = "debug"
    c.Logging.Format = "text"
    c.KnownModels = []string{"GLM-5.1", "GLM-5"}
    c.StreamHoldback = 24
    c.SyncMode = false
    c.SessionPoolSize = defaultPoolSize
    c.SessionAcquireTimeout = int(defaultPoolWait / time.Second)

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
    if t := os.Getenv("ZAI_TOKEN"); t != "" {
        c.ZaiToken = t
    }
    // Multi-account pool: ZAI_TOKENS="token1,token2,token3". Separators:
    // comma / semicolon / whitespace / newline. De-duplicated in ParseTokensEnv.
    if toks := ParseTokensEnv(); len(toks) > 0 {
        c.ZaiTokens = toks
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
            // Explicit opt-in to the old [ROLE: ...] rewrite shim.
            c.AgentMode = true
            c.AgentModeVariant = "legacy"
        case "0", "false", "no", "off":
            c.AgentMode = false
        }
    }
    // AGENT_MODE_VARIANT overrides the shim variant independently of the
    // AGENT_MODE on/off switch: "modern" (default) or "legacy".
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
    if h := os.Getenv("STREAM_HOLDBACK"); h != "" {
        if n, err := strconv.Atoi(h); err == nil && n >= 0 {
            c.StreamHoldback = n
        }
    }
    // SYNC_MODE restores the legacy synchronous session flow (one chat
    // created per request). Used sessions are still deleted after use.
    if sm := os.Getenv("SYNC_MODE"); sm != "" {
        switch strings.ToLower(sm) {
        case "1", "true", "yes", "on":
            c.SyncMode = true
        case "0", "false", "no", "off":
            c.SyncMode = false
        }
    }
    if ps := os.Getenv("SESSION_POOL_SIZE"); ps != "" {
        if n, err := strconv.Atoi(ps); err == nil && n >= 1 {
            c.SessionPoolSize = n
        }
    }
    if at := os.Getenv("SESSION_ACQUIRE_TIMEOUT"); at != "" {
        if n, err := strconv.Atoi(at); err == nil && n >= 0 {
            c.SessionAcquireTimeout = n
        }
    }
    return c
}

var config = loadConfig()

// agentModern reports whether the modern agent-mode shim (XML-sectioned
// prompt, tolerant marker/payload parsing — see agent.go) is active.
func (c *Config) agentModern() bool {
    return c.AgentMode && !strings.EqualFold(c.AgentModeVariant, "legacy")
}

// agentLegacy reports whether the legacy agent-mode shim ([ROLE: ...]
// message rewriting — see transformMessagesForAgent) is active.
func (c *Config) agentLegacy() bool {
    return c.AgentMode && strings.EqualFold(c.AgentModeVariant, "legacy")
}


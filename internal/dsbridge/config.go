// config.go — configuration for the DeepSeek bridge (package dsbridge).
// .env-first with sane defaults; same loader as the other bridges so one
// .env drives the whole router.

package dsbridge

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// BASE_URL is a var so tests can point the bridge at a mock upstream.
var BASE_URL = "https://chat.deepseek.com"

const defaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) " +
	"AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

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
	// DeepSeekTokens is the account pool (DEEPSEEK_TOKENS="t1,t2,...").
	// DeepSeek has NO guest mode: an account token is always required.
	DeepSeekTokens []string
	// AccountQueueTimeout / AccountCooldownBase mirror the other bridges.
	AccountQueueTimeout int
	AccountCooldownBase int
	AgentMode           bool
	AgentModeVariant    string
	Logging             struct {
		Level  string
		Format string
	}
}

func loadDotEnv() {
	f, err := os.Open(".env")
	if err != nil {
		return
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		eq := strings.Index(line, "=")
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		if i := strings.Index(val, " #"); i >= 0 {
			val = strings.TrimSpace(val[:i])
		}
		if len(val) >= 2 && (val[0] == '"' || val[0] == '\'') && val[len(val)-1] == val[0] {
			val = val[1 : len(val)-1]
		}
		if key == "" {
			continue
		}
		if os.Getenv(key) == "" {
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
	c.Auth.Token = "deepseek"
	c.Timeouts.Default = 300000
	c.AccountQueueTimeout = 120
	c.AccountCooldownBase = 120
	c.AgentMode = false
	c.AgentModeVariant = "modern"

	if t := os.Getenv("AUTH_TOKEN"); t != "" {
		c.Auth.Token = t
	}
	if toks := parseTokensEnv(os.Getenv("DEEPSEEK_TOKENS")); len(toks) > 0 {
		c.DeepSeekTokens = toks
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
	return c
}

var config = loadConfig()

func (c *Config) agentModern() bool {
	return c.AgentMode && !strings.EqualFold(c.AgentModeVariant, "legacy")
}

func (c *Config) agentLegacy() bool {
	return c.AgentMode && strings.EqualFold(c.AgentModeVariant, "legacy")
}

// parseTokensEnv splits DEEPSEEK_TOKENS on comma / semicolon / whitespace and
// de-duplicates (same semantics as the other bridges' ParseTokensEnv).
func parseTokensEnv(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
	seen := make(map[string]bool)
	var out []string
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	return out
}

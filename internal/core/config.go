// config.go — OmniRouter core configuration (.env-first, same loader family
// as the embedded bridges).

package core

import (
	"bufio"
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/Godde3s/omnirouter/internal/authtoken"
)

type Config struct {
	Host string
	Port int

	// AdminPassword guards the dashboard + /admin/api.
	AdminPassword string
	// RouterKey seeds the first client API key (ROUTER_KEY). When empty a
	// random sk- key is generated and stored.
	RouterKey string
	// InternalToken is the shared bridge secret (AUTH_TOKEN from .env). The
	// embedded bridges were configured from the very same env at package
	// init, so core and bridges always agree.
	InternalToken string

	DataDir  string
	LogLevel string

	// RetryPerProvider: attempts against one provider before failing over.
	RetryPerProvider int
	// CooldownSeconds: after a provider exhausts its retries it is benched
	// for this long so later requests prefer healthier candidates.
	CooldownSeconds int64
	// RequestTimeoutSec: hard upstream timeout for NON-stream requests
	// (streams are unbounded — reasoning models think for minutes).
	RequestTimeoutSec int64
}

func loadCoreConfig() *Config {
	c := &Config{}
	c.Host = envOr("HOST", "0.0.0.0")
	c.Port = envIntOr("PORT", 8080)
	c.AdminPassword = loadAdminPassword()
	c.RouterKey = strings.TrimSpace(os.Getenv("ROUTER_KEY"))
	c.InternalToken = authtoken.Get()
	c.DataDir = envOr("OMNI_DATA_DIR", "data")
	c.LogLevel = envOr("LOG_LEVEL", "info")
	c.RetryPerProvider = envIntOr("RETRY_PER_PROVIDER", 1)
	c.CooldownSeconds = int64(envIntOr("COOLDOWN_SECONDS", 20))
	c.RequestTimeoutSec = int64(envIntOr("REQUEST_TIMEOUT", 300))
	return c
}

// loadAdminPassword returns ADMIN_PASSWORD, or a random per-run password when
// it is unset. Shipping "admin" as a default meant every install that skipped
// the .env step exposed its dashboard to anyone who could reach the port.
func loadAdminPassword() string {
	pw := strings.TrimSpace(os.Getenv("ADMIN_PASSWORD"))
	if pw != "" {
		if pw == "admin" {
			log.Printf("[admin] ADMIN_PASSWORD is the well-known default \"admin\" — change it before exposing this port")
		}
		return pw
	}
	pw = randomHex(9)
	log.Printf("[admin] ADMIN_PASSWORD is not set — generated a random dashboard password for this run: %s", pw)
	log.Printf("[admin] set ADMIN_PASSWORD in .env to keep it stable across restarts")
	return pw
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envIntOr(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// loadDotEnv is run by the embedded bridge packages at their package init —
// by the time core starts, .env values are already in the process env. This
// duplicate is a no-op safety net for exotic builds (kept identical).
func loadDotEnvCore() {
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
		if key != "" && os.Getenv(key) == "" {
			_ = os.Setenv(key, val)
		}
	}
}

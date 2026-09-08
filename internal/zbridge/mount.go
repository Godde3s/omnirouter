// mount.go — exported integration surface for the OmniRouter core.
//
// The router embeds this bridge as a library: it calls Init() once at boot
// (replacing the init part of Run()) and Handler() to obtain the bridge's
// full HTTP surface, which the core then serves on an internal listener.
// Run() itself is never called — the router owns the public port, auth and
// graceful shutdown.

package zbridge

import (
	"log"
	"net/http"
	"os"
	"time"
)

// Init prepares every piece of bridge state that Run() used to set up before
// serving: the device-token database, the account pool, the captcha cache
// (agent mode), the session pool and the background session/model warmup.
// Safe to call exactly once at startup (the router guarantees this).
func Init() {
	if os.Getenv("GLM_TOKEN_DB") != "" {
		dbPath = os.Getenv("GLM_TOKEN_DB")
	}
	if dbPath == "" {
		dbPath = "tokens.sqlite"
	}

	if _, err := os.Stat(dbPath); err != nil {
		// Auto-create an EMPTY token database so the server always boots.
		log.Printf("[GLM] '%s' not found — creating an empty token database", dbPath)
		log.Printf("[GLM] chat needs device tokens: fill '%s' (token-collector) or run with ZAI_TOKENS", dbPath)
		if err := initDB(); err != nil {
			log.Printf("[GLM] Failed to create database: %v", err)
			return
		}
		if _, err := globalDB.Exec(`CREATE TABLE IF NOT EXISTS tokens (
            id    INTEGER PRIMARY KEY AUTOINCREMENT,
            token TEXT    NOT NULL,
            batch INTEGER NOT NULL
        )`); err != nil {
			log.Printf("[GLM] Failed to initialize database schema: %v", err)
			return
		}
	}
	if err := initDB(); err != nil {
		log.Printf("[GLM] Failed to open database: %v", err)
		return
	}
	// NOTE: globalDB stays open for the process lifetime (no defer Close —
	// the router owns process shutdown).

	initAccounts()
	gRunning.Store(true)

	if config.AgentMode {
		go captchaCache.Run()
		logInfo("Agent mode: Captcha background cache started")
	}

	if config.SyncMode {
		log.Println("[GLM] Session mode: SYNC (fresh chat per request, deleted on Z.AI after use)")
	} else {
		poolWait = time.Duration(config.SessionAcquireTimeout) * time.Second
		if config.SessionAcquireTimeout <= 0 {
			poolWait = 0
		}
		sessionPool = NewSessionPool(NewZAIChatBackend(), config.SessionPoolSize)
		log.Printf("[GLM] Session mode: ASYNC (pre-made chat batch x%d, throwaway)", sessionPool.Size())
		sessionPool.Start()
	}

	go func() {
		if accounts != nil {
			if err := initializeSession(); err != nil {
				logInfo("[GLM] Guest metadata init skipped (account mode active) — harmless.")
			}
		} else if err := initializeSession(); err != nil {
			log.Println("[GLM] Session init deferred — will retry on first request.")
		}
		fetchModelsFromZAI()
	}()
}

// Handler returns the bridge's complete HTTP surface (routes + auth + CORS),
// exactly what NewHandler assembles. The router forwards to it over an
// internal listener; the bridge's own AUTH_TOKEN acts as the internal secret.
func Handler() http.Handler { return NewHandler() }

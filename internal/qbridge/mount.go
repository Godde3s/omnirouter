// mount.go — exported integration surface for the OmniRouter core.
//
// The router embeds this bridge as a library: it calls Init() once at boot
// (replacing the init part of Run()) and Handler() to obtain the bridge's
// full HTTP surface, which the core then serves on an internal listener.
// Run() itself is never called — the router owns the public port, auth and
// graceful shutdown.

package qbridge

import (
	"log"
	"net/http"
)

// Init prepares every piece of bridge state that Run() used to set up before
// serving: the multi-account pool (or guest mode) and the background session
// warmup + model cache. Safe to call exactly once at startup.
func Init() {
	initAccounts()
	gRunning.Store(true)

	go func() {
		if err := initializeSession(); err != nil {
			log.Println("[Qwen] Session init deferred — will retry on first request.")
		}
		fetchModelsFromQwen()
	}()
}

// Handler returns the bridge's complete HTTP surface (routes + auth + CORS),
// exactly what NewHandler assembles. The router forwards to it over an
// internal listener; the bridge's own AUTH_TOKEN acts as the internal secret.
func Handler() http.Handler { return NewHandler() }

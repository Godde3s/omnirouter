// mount.go — exported integration surface for the OmniRouter core.
//
// The router embeds this bridge as a library: it calls Init() once at boot
// and Handler() to obtain the bridge's full HTTP surface, served on an
// internal loopback listener. Run() itself is never called here.

package gbridge

import (
	"log"
	"net/http"
)

// Init prepares the account pool (guest mode when GEMINI_COOKIES is unset)
// and warms the session + live model cache in the background.
func Init() {
	initAccounts()
	gRunning.Store(true)

	go func() {
		if err := initializeSession(); err != nil {
			log.Println("[Gemini] Session init deferred — will retry on first request.")
		}
		fetchModelsFromGemini()
	}()
}

// Handler returns the bridge's complete HTTP surface (routes + auth + CORS).
func Handler() http.Handler { return NewHandler() }

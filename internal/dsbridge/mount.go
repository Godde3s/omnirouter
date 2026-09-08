// mount.go — exported integration surface for the OmniRouter core.

package dsbridge

import (
	"net/http"
)

// NewHandler assembles the DeepSeek bridge's HTTP surface (routes + auth +
// CORS) — same shape as the other bridges.
func NewHandler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/v1/models", authMiddleware(modelsHandler))
	mux.HandleFunc("/models", authMiddleware(modelsHandler2))
	mux.HandleFunc("/v1/chat/completions", authMiddleware(chatCompletionsHandler))
	mux.HandleFunc("/v1/messages", authMiddleware(anthropicMessagesHandler))
	mux.HandleFunc("/admin/clients", clientsHandler)
	mux.HandleFunc("/inject.js", injectHandler)
	mux.HandleFunc("/stop", authMiddleware(stopHandler))

	return corsMiddleware(mux)
}

// Init prepares the account pool. DeepSeek requires account tokens — with no
// DEEPSEEK_TOKENS the bridge stays in a clean "disabled" state and returns
// bilingual guidance at request time instead of crashing.
func Init() {
	initAccounts()
	gRunning.Store(true)
}

// Handler returns the bridge's complete HTTP surface for the router core.
func Handler() http.Handler { return NewHandler() }

// mount.go — exported integration surface for the OmniRouter core.
//
// The router embeds this bridge as a library: it calls Init() once at boot
// and Handler() to obtain the bridge's full HTTP surface, served on an
// internal loopback listener. There are no accounts to warm: the Zen free
// tier is keyless, so the bridge is healthy the moment it starts.

package obridge

import "net/http"

// Init reads env config (no network calls — models are fetched lazily).
func Init() {
	initConfig()
}

// Handler returns the bridge's complete HTTP surface (routes + auth + CORS).
func Handler() http.Handler { return NewHandler() }

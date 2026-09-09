// heartbeat.go — proactive cookie freshness for account mode.
//
// Google rotates __Secure-1PSIDTS roughly every hour. The bridge already
// rotates lazily on auth failures, but a proactive heartbeat keeps the
// cookie chain warm BEFORE it breaks (same idea as OmniBridge's cookie
// refresh loop): every ~50 minutes each cookie account gets one
// RotateCookies call, and a Google-issued refresh is persisted to the
// PSIDTS cache so it survives restarts.
//
// Guest mode (no cookies) needs no heartbeat — nothing to keep fresh.

package gbridge

import (
	"os"
	"strconv"
	"time"
)

// startCookieHeartbeat launches the background PSIDTS refresh loop for all
// cookie accounts. Safe to call exactly once at startup; no-op in guest mode.
func startCookieHeartbeat() {
	if accounts == nil || len(accounts.accounts) == 0 {
		return
	}
	interval := 50 * time.Minute
	if v := os.Getenv("GEMINI_HEARTBEAT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 60 {
			interval = time.Duration(n) * time.Second
		}
	}
	logInfo("[Heartbeat] proactive PSIDTS refresh every " + interval.String())
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for range t.C {
			for _, acc := range accounts.accounts {
				if acc == nil || acc.PSID == "" {
					continue
				}
				// rotateAccountCookies logs only when Google actually
				// issued a new PSIDTS — silent keep-alives otherwise.
				rotateAccountCookies(acc)
			}
		}
	}()
}

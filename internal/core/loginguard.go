// loginguard.go — brute-force protection for POST /admin/login.
//
// The dashboard password is the only barrier in front of every provider API
// key, so unlimited guesses were the cheapest way in. This guard allows a
// handful of failures per client IP inside a rolling window; a successful
// login clears the counter for that IP.

package core

import (
	"net"
	"net/http"
	"sync"
	"time"
)

const (
	loginMaxFailures = 5
	loginWindow      = time.Minute
)

type loginAttempt struct {
	count int
	since time.Time
}

type loginGuard struct {
	mu       sync.Mutex
	failures map[string]loginAttempt
}

var adminLoginGuard = &loginGuard{failures: map[string]loginAttempt{}}

// blocked reports whether key has exhausted its attempts inside the window.
func (g *loginGuard) blocked(key string, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	a, ok := g.failures[key]
	if !ok {
		return false
	}
	if now.Sub(a.since) > loginWindow {
		delete(g.failures, key)
		return false
	}
	return a.count >= loginMaxFailures
}

// fail records one failed attempt, restarting the window when it has expired.
func (g *loginGuard) fail(key string, now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	a, ok := g.failures[key]
	if !ok || now.Sub(a.since) > loginWindow {
		g.failures[key] = loginAttempt{count: 1, since: now}
		return
	}
	a.count++
	g.failures[key] = a
}

// reset clears the counter after a successful login.
func (g *loginGuard) reset(key string) {
	g.mu.Lock()
	delete(g.failures, key)
	g.mu.Unlock()
}

// clientIP strips the port so all requests from one host share a bucket.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

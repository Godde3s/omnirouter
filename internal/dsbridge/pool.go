// pool.go — DeepSeek account pool: round-robin + 429 cooldown + failover.
// Same semantics as the other bridges' pools (ported & simplified):
// cooldown doubles per consecutive 429 (cap 30m), dead accounts are skipped,
// and Pick blocks (bounded by AccountQueueTimeout) while everything is cooling.

package dsbridge

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

type Account struct {
	ID          int    `json:"id"`
	Token       string `json:"-"`
	Label       string `json:"label"`
	Status      string `json:"status"`
	Failures    int    `json:"failures"`
	Cooldown    int64  `json:"cooldown_ms"`
	LastUsed    int64  `json:"last_used_ms"`
	Requests    int64  `json:"requests"`
	consec429   int
	cooldownTil time.Time
	mu          sync.Mutex
}

func (a *Account) Available() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return time.Now().After(a.cooldownTil)
}

func (a *Account) CooldownLeft() time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	if d := time.Until(a.cooldownTil); d > 0 {
		return d
	}
	return 0
}

func (a *Account) ReportOK() {
	a.mu.Lock()
	a.consec429 = 0
	a.Status = "ok"
	a.Failures = 0
	a.LastUsed = time.Now().UnixMilli()
	a.Requests++
	a.mu.Unlock()
}

func (a *Account) Report429() {
	a.mu.Lock()
	a.consec429++
	base := time.Duration(config.AccountCooldownBase) * time.Second
	if base <= 0 {
		base = 120 * time.Second
	}
	d := base << (a.consec429 - 1)
	if d > 30*time.Minute {
		d = 30 * time.Minute
	}
	a.cooldownTil = time.Now().Add(d)
	a.Status = "rate_limited"
	a.LastUsed = time.Now().UnixMilli()
	a.mu.Unlock()
}

func (a *Account) ReportAuthFail(reason string) {
	a.mu.Lock()
	a.Status = "dead:" + reason
	a.cooldownTil = time.Now().Add(24 * time.Hour)
	a.LastUsed = time.Now().UnixMilli()
	a.mu.Unlock()
}

func (a *Account) ReportError() {
	a.mu.Lock()
	a.Failures++
	a.Status = "error"
	a.LastUsed = time.Now().UnixMilli()
	a.mu.Unlock()
}

type AccountSnapshot struct {
	ID        int    `json:"id"`
	Label     string `json:"label"`
	Status    string `json:"status"`
	CooldownS int    `json:"cooldown_s"`
	Requests  int64  `json:"requests"`
}

func (a *Account) Snapshot() AccountSnapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	return AccountSnapshot{
		ID:        a.ID,
		Label:     a.Label,
		Status:    a.Status,
		CooldownS: int(a.CooldownLeft().Seconds()),
		Requests:  a.Requests,
	}
}

type AccountPool struct {
	mu      sync.Mutex
	accs    []*Account
	next    int
	queueTO time.Duration
}

var accounts *AccountPool

func NewAccountPool(tokens []string) *AccountPool {
	p := &AccountPool{queueTO: time.Duration(config.AccountQueueTimeout) * time.Second}
	for i, t := range tokens {
		label := "ds-" + shortToken(t)
		p.accs = append(p.accs, &Account{ID: i + 1, Token: t, Label: label, Status: "ok"})
	}
	return p
}

func shortToken(t string) string {
	t = strings.TrimSpace(t)
	if len(t) <= 10 {
		return t
	}
	return t[:6] + "…" + t[len(t)-4:]
}

func (p *AccountPool) Len() int {
	if p == nil {
		return 0
	}
	return len(p.accs)
}

func (p *AccountPool) HealthyCount() int {
	if p == nil {
		return 0
	}
	n := 0
	for _, a := range p.accs {
		if a.Available() {
			n++
		}
	}
	return n
}

func (p *AccountPool) StatusJSON() []AccountSnapshot {
	if p == nil {
		return nil
	}
	out := make([]AccountSnapshot, 0, len(p.accs))
	for _, a := range p.accs {
		out = append(out, a.Snapshot())
	}
	return out
}

// Pick returns the next available account, blocking (bounded) while all are
// cooling down. Returns a clean bilingual error when nothing is usable.
func (p *AccountPool) Pick(ctx context.Context) (*Account, error) {
	if p == nil || len(p.accs) == 0 {
		return nil, errors.New(noAccountsMessage())
	}
	deadline := time.Time{}
	if p.queueTO > 0 {
		deadline = time.Now().Add(p.queueTO)
	}
	for {
		p.mu.Lock()
		n := len(p.accs)
		for i := 0; i < n; i++ {
			a := p.accs[(p.next+i)%n]
			if a.Available() {
				p.next = (p.next + i + 1) % n
				p.mu.Unlock()
				return a, nil
			}
		}
		p.mu.Unlock()
		if !deadline.IsZero() && time.Now().After(deadline) {
			return nil, fmt.Errorf("همه‌ی اکانت‌های DeepSeek در حالت cooldown هستند / all DeepSeek accounts are cooling down — try again in a moment")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (p *AccountPool) pickOther(exclude *Account) *Account {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accs {
		if a != exclude && a.Available() {
			return a
		}
	}
	return nil
}

func noAccountsMessage() string {
	return "هیچ اکانت DeepSeek ثبت نشده — توکن را در DEEPSEEK_TOKENS بگذار یا ./ds-login را اجرا کن / " +
		"No DeepSeek account yet — put a userToken into DEEPSEEK_TOKENS or run ./ds-login " +
		"(chat.deepseek.com → F12 → Console → JSON.parse(localStorage.userToken).value)"
}

func initAccounts() {
	toks := config.DeepSeekTokens
	if len(toks) == 0 {
		accounts = nil
		logAlways("No DEEPSEEK_TOKENS configured — DeepSeek models disabled (bilingual hint available at request time)")
		return
	}
	accounts = NewAccountPool(toks)
	logAlways(fmt.Sprintf("DeepSeek account pool: %d account(s), round-robin + failover", accounts.Len()))
}

// acquireAccountForRequest is the handler-facing picker.
func acquireAccountForRequest(ctx context.Context) (*Account, error) {
	if accounts == nil {
		return nil, errors.New(noAccountsMessage())
	}
	return accounts.Pick(ctx)
}

var _ = os.Getenv // keep os import if unused elsewhere

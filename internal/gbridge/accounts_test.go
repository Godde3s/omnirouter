// Unit tests for the multi-account cookie pool (accounts.go).
//
// Covers:
//   - ParseCookiesEnv: JSON list, cookie-header form, bare PSID, dedup
//   - NewAccountPool / round-robin pickNow ordering
//   - Report429 exponential cooldown + Available() gating + pickOther skip
//   - ReportAuthFail marks the account dead (never picked again)
//   - Pick() bounded queue: returns error on ACCOUNT_QUEUE_TIMEOUT expiry
//   - Pool.Report error classification (429 vs 401/403 vs generic)
//   - PSIDTS cache round-trip

package gbridge

import (
    "context"
    "errors"
    "os"
    "path/filepath"
    "testing"
    "time"
)

func TestParseCookiesEnv(t *testing.T) {
    cases := []struct {
        name     string
        raw      string
        wantN    int
        wantPSID string
    }{
        {"empty", "", 0, ""},
        {"json list", `[{"psid":"g.a001","psidts":"ts1"}]`, 1, "g.a001"},
        {"json multi", `[{"psid":"g.a001","psidts":"ts1"},{"psid":"g.a002","psidts":"ts2"}]`, 2, "g.a001"},
        {"cookie header", "__Secure-1PSID=g.a003; __Secure-1PSIDTS=ts3", 1, "g.a003"},
        {"bare psid", "g.a004", 1, "g.a004"},
        {"multi header", "__Secure-1PSID=g.a005; __Secure-1PSIDTS=ts5,\ng.a006", 2, "g.a005"},
        {"json generic keys", `[{"__Secure-1PSID":"g.a007","__Secure-1PSIDTS":"ts7"}]`, 1, "g.a007"},
        {"dedup", `[{"psid":"g.a008"},{"psid":"g.a008"}]`, 1, "g.a008"},
    }
    for _, tc := range cases {
        t.Run(tc.name, func(t *testing.T) {
            got := ParseCookiesEnv(tc.raw)
            if len(got) != tc.wantN {
                t.Fatalf("ParseCookiesEnv() = %d pairs, want %d (%+v)", len(got), tc.wantN, got)
            }
            if tc.wantN > 0 && got[0].PSID != tc.wantPSID {
                t.Fatalf("PSID = %q, want %q", got[0].PSID, tc.wantPSID)
            }
        })
    }
}

func TestParseCookiesFileShape(t *testing.T) {
    dir := t.TempDir()
    path := filepath.Join(dir, "cookies.json")
    _ = os.WriteFile(path, []byte(`[{"psid":"g.file1","psidts":"tsf1"}]`), 0o600)

    old := config.GeminiCookiesFle
    config.GeminiCookiesFle = path
    defer func() { config.GeminiCookiesFle = old }()

    pairs := loadCookieFile()
    if len(pairs) != 1 || pairs[0].PSID != "g.file1" || pairs[0].PSIDTS != "tsf1" {
        t.Fatalf("loadCookieFile() = %+v", pairs)
    }
}

func TestPoolRoundRobin(t *testing.T) {
    p := NewAccountPool([]cookiePair{{PSID: "a"}, {PSID: "b"}, {PSID: "c"}})
    if p == nil || p.Len() != 3 {
        t.Fatalf("expected 3-account pool")
    }
    order := []int{}
    for i := 0; i < 6; i++ {
        acc, err := p.Pick(context.Background())
        if err != nil || acc == nil {
            t.Fatalf("Pick() = %v, %v", acc, err)
        }
        order = append(order, acc.ID)
    }
    want := []int{1, 2, 3, 1, 2, 3}
    for i := range want {
        if order[i] != want[i] {
            t.Fatalf("round-robin order = %v, want %v", order, want)
        }
    }
}

func TestPoolCooldownOn429(t *testing.T) {
    config.AccountCooldownBase = 60
    p := NewAccountPool([]cookiePair{{PSID: "a"}, {PSID: "b"}})
    a := p.accounts[0]

    a.Report429()
    if a.Available() {
        t.Fatal("account in cooldown must not be available")
    }
    if a.CooldownLeft() <= 0 || a.CooldownLeft() > time.Minute {
        t.Fatalf("cooldown left = %v, want (0, 60s]", a.CooldownLeft())
    }
    // Next pick must skip to the second account.
    acc, err := p.Pick(context.Background())
    if err != nil || acc == nil || acc.ID != 2 {
        t.Fatalf("Pick() after cooldown = %v, %v — want account 2", acc, err)
    }
    // Second 429 doubles the cooldown.
    a.Report429()
    if d := a.CooldownLeft(); d <= time.Minute {
        t.Fatalf("second 429 should double cooldown, got %v", d)
    }
}

func TestPoolDeadOnAuthFail(t *testing.T) {
    p := NewAccountPool([]cookiePair{{PSID: "a"}, {PSID: "b"}})
    p.accounts[0].ReportAuthFail("401: cookies expired")

    if p.accounts[0].Available() {
        t.Fatal("dead account must never be available")
    }
    // Only account 2 remains; every pick returns it.
    for i := 0; i < 3; i++ {
        acc, err := p.Pick(context.Background())
        if err != nil || acc == nil || acc.ID != 2 {
            t.Fatalf("Pick() after auth-fail = %v, %v", acc, err)
        }
    }
    snap := p.accounts[0].Snapshot()
    if !snap.Dead || snap.Healthy {
        t.Fatalf("snapshot should show dead+unhealthy: %+v", snap)
    }
}

func TestPoolQueueTimeout(t *testing.T) {
    old := config.AccountQueueTimeout
    config.AccountQueueTimeout = 1 // second
    defer func() { config.AccountQueueTimeout = old }()

    p := NewAccountPool([]cookiePair{{PSID: "a"}})
    p.accounts[0].Report429() // put into 60s cooldown

    start := time.Now()
    _, err := p.Pick(context.Background())
    if err == nil {
        t.Fatal("expected timeout error when all accounts cool down")
    }
    if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
        t.Fatalf("Pick() returned too early (%v) — bounded queue must wait", elapsed)
    }
}

func TestPoolReportClassification(t *testing.T) {
    p := NewAccountPool([]cookiePair{{PSID: "a"}})
    accountsBackup := accounts
    accounts = nil // rotation path needs no live pool
    defer func() { accounts = accountsBackup }()

    p.Report(p.accounts[0], errors.New("google error HTTP 429: rate limited"))
    if p.accounts[0].rateHits.Load() != 1 || p.accounts[0].errors.Load() != 1 {
        t.Fatal("429 should be classified as a rate hit")
    }

    p.Report(p.accounts[0], errors.New("HTTP 401: کوکی‌های اکانت گوگل رد شدند"))
    if !p.accounts[0].dead {
        t.Fatal("401 should mark the account dead (rotation failed in test env)")
    }
}

func TestPickOtherExcludesCurrent(t *testing.T) {
    p := NewAccountPool([]cookiePair{{PSID: "a"}, {PSID: "b"}})
    got := p.pickOther(p.accounts[0])
    if got == nil || got.ID != 2 {
        t.Fatalf("pickOther(#1) = %v — want #2", got)
    }
    // Both cooling -> nil.
    p.accounts[1].Report429()
    if p.pickOther(p.accounts[0]) != nil {
        t.Fatal("pickOther must return nil when every other account is cooling")
    }
}

func TestNilPoolIsInert(t *testing.T) {
    var p *AccountPool
    if acc, err := p.Pick(context.Background()); acc != nil || err != nil {
        t.Fatal("nil pool Pick must be (nil, nil)")
    }
    if p.Len() != 0 || p.HealthyCount() != 0 || p.StatusJSON() != nil {
        t.Fatal("nil pool accessors must be inert")
    }
    if got := p.pickOther(nil); got != nil {
        t.Fatal("nil pool pickOther must be nil")
    }
}

func TestPSIDTSCacheRoundTrip(t *testing.T) {
    dir := t.TempDir()
    old := os.Getenv("GEMINI_CACHE_FILE")
    _ = os.Setenv("GEMINI_CACHE_FILE", filepath.Join(dir, "cache.json"))
    defer func() {
        if old != "" {
            _ = os.Setenv("GEMINI_CACHE_FILE", old)
        } else {
            _ = os.Unsetenv("GEMINI_CACHE_FILE")
        }
    }()

    p := NewAccountPool([]cookiePair{{PSID: "g.c1", PSIDTS: "ts-old"}})
    accountsBackup := accounts
    accounts = p
    defer func() { accounts = accountsBackup }()

    p.accounts[0].updatePSIDTS("ts-new")
    if p.accounts[0].PSIDTS != "ts-new" {
        t.Fatal("updatePSIDTS must store the fresh value")
    }
    if _, err := os.Stat(cacheFilePath()); err != nil {
        t.Fatalf("cache file must exist after rotation: %v", err)
    }

    // A rebuilt pool must restore the rotated value.
    p2 := NewAccountPool([]cookiePair{{PSID: "g.c1", PSIDTS: "ts-old"}})
    loadPSIDTSCache(p2)
    if p2.accounts[0].PSIDTS != "ts-new" {
        t.Fatalf("rotated PSIDTS must survive restarts, got %q", p2.accounts[0].PSIDTS)
    }
}

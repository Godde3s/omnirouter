// ds-login — one-time interactive login for the DeepSeek bridge.
//
// Opens a visible Chromium window, you sign in to chat.deepseek.com by hand
// (solving the AWS WAF human-check if shown), then the tool polls
// localStorage.userToken and prints a ready-to-paste DEEPSEEK_TOKENS value.
//
//   go run ./cmd/ds-login          (or the prebuilt ds-login binary)
package main

import (
        "fmt"
        "log"
        "os"
        "strings"
        "time"

        "github.com/mxschmitt/playwright-go"
)

const readTokenJS = `() => {
  try {
    const raw = window.localStorage.getItem('userToken');
    if (!raw) return null;
    const o = JSON.parse(raw);
    return (o && o.value) ? o.value : null;
  } catch (e) { return null; }
}`

func main() {
        fmt.Println(`
╔════════════════════════════════════════════════════╗
║   DeepSeek login helper (OmniRouter)               ║
║   یک‌بار مرورگر باز می‌شود؛ وارد شو و تمام          ║
╚════════════════════════════════════════════════════╝`)

        if err := playwright.Install(&playwright.RunOptions{Browsers: []string{"chromium"}}); err != nil {
                log.Printf("[login] playwright chromium already installed? (%v)", err)
        }
        pw, err := playwright.Run()
        if err != nil {
                log.Fatal(err)
        }
        defer pw.Stop()

        browser, err := pw.Chromium.Launch(playwright.BrowserTypeLaunchOptions{
                Headless: playwright.Bool(false),
                Args:     []string{"--disable-blink-features=AutomationControlled"},
        })
        if err != nil {
                log.Fatal(err)
        }
        defer browser.Close()

        ctx, err := browser.NewContext()
        if err != nil {
                log.Fatal(err)
        }
        page, err := ctx.NewPage()
        if err != nil {
                log.Fatal(err)
        }

        if _, err := page.Goto("https://chat.deepseek.com/sign_in", playwright.PageGotoOptions{
                WaitUntil: playwright.WaitUntilStateCommit,
                Timeout:   playwright.Float(60000),
        }); err != nil {
                log.Printf("[login] navigation interrupted (%v) — continuing; finish signing in.", err)
        }

        fmt.Println("▸ Please sign in inside the opened window (solve the human-check if shown).")
        fmt.Println("▸ لطفاً در پنجره‌ی بازشده وارد شو (اگر چک انسانی آمد، حلش کن). Waiting…")

        deadline := time.Now().Add(5 * time.Minute)
        token := ""
        for time.Now().Before(deadline) {
                v, err := page.Evaluate(readTokenJS)
                if err == nil {
                        if s, ok := v.(string); ok && s != "" {
                                token = s
                                break
                        }
                }
                time.Sleep(1 * time.Second)
        }
        if token == "" {
                fmt.Println("✗ timed out — no token captured / توکنی گرفته نشد")
                os.Exit(1)
        }

        fmt.Println()
        fmt.Println("✓ Token captured!")
        fmt.Println("──────────────────────────────────────────────")
        fmt.Println("Put this into your .env (این را در .env بگذار):")
        fmt.Println()
        fmt.Printf("DEEPSEEK_TOKENS=%s\n", strings.TrimSpace(token))
        fmt.Println()
        fmt.Println("──────────────────────────────────────────────")
}

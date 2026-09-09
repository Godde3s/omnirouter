// oauth.go — OAuth-style Device Authorization Grant (RFC 8628), the same
// flow GitHub Copilot / Cline / Kilo Code use, so agents can connect to
// OmniRouter without copy-pasting API keys.
//
//      POST /oauth/device/start   {client} → device_code + user_code
//      GET  /oauth/authorize?user_code=XXXX-XXXX   → consent page (Persian RTL)
//      POST /oauth/authorize/decision {user_code, decision} → approve | deny
//      POST /oauth/device/poll    {device_code} → pending | approved + token
//
// Approving mints a dedicated virtual key (sk-omni-…) tagged with the
// client id — visible and revocable in the dashboard "Connections" panel.
// The consent decision is accepted from loopback clients without admin auth
// (the approving browser is the local user's, same trust model as
// 9router/GitHub device flow); non-loopback requests need an admin session.

package core

import (
        "crypto/rand"
        "encoding/json"
        "fmt"
        "html"
        "net/http"
        "strings"
)

// knownClients maps client ids → display metadata for the consent page and
// dashboard. Unknown ids are accepted free-form (like GitHub's device flow).
type clientMeta struct {
        Label string // human name (shown as-is; may be Latin)
        Desc  string // Persian one-liner
}

var knownClients = map[string]clientMeta{
        "cline":       {"Cline", "دستیار کدنویسی VS Code — OpenAI + Anthropic"},
        "kilo":        {"Kilo Code", "ایجنت کدنویسی — پروفایل Architect/Code/Debug"},
        "hermes":      {"Hermes Agent", "ایجنت خودکار با tool-calling کامل"},
        "claude-code": {"Claude Code", "ترمینال Anthropic — پروتکل /v1/messages"},
        "codex":       {"OpenAI Codex CLI", "ترمینال OpenAI — config.toml"},
        "cursor":      {"Cursor", "ادیتور AI — Override Base URL"},
        "roo":         {"Roo Code", "انشعاب Cline با حالت‌های چندگانه"},
        "continue":    {"Continue", "ادیتور متن‌باز VS Code/JetBrains"},
        "droid":       {"Factory Droid", "ایجنت Factory AI"},
        "opencode":    {"OpenCode", "کلاینت ترمینال TUI — بدون کلید بیرونی"},
        "openclaw":    {"OpenClaw", "کلاینت Open Claw"},
        "copilot":     {"GitHub Copilot", "اشتراک Copilot به‌عنوان کلاینت روتر"},
        "custom":      {"Custom Agent", "هر ابزار OpenAI/Anthropic-سازگار"},
}

func clientLabel(id string) string {
        id = strings.ToLower(strings.TrimSpace(id))
        if m, ok := knownClients[id]; ok {
                return m.Label
        }
        if id == "" {
                return "Unknown Client"
        }
        return id
}

// userCodeAlphabet excludes ambiguous glyphs (0/O, 1/I) like GitHub's codes.
const userCodeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

func newUserCode() string {
        b := make([]byte, 8)
        _, _ = rand.Read(b)
        out := make([]byte, 0, 9)
        for i, c := range b {
                if i == 4 {
                        out = append(out, '-')
                }
                out = append(out, userCodeAlphabet[int(c)%len(userCodeAlphabet)])
        }
        return string(out)
}

func isLoopback(r *http.Request) bool {
        host := r.RemoteAddr
        if i := strings.LastIndex(host, ":"); i > 0 {
                host = host[:i]
        }
        host = strings.Trim(host, "[]")
        return host == "127.0.0.1" || host == "::1" || host == "localhost"
}

// registerOAuthRoutes wires the device-flow endpoints onto the mux.
func registerOAuthRoutes(mux *http.ServeMux, st *Store, cfg *Config) {
        mux.HandleFunc("/oauth/device/start", func(w http.ResponseWriter, r *http.Request) {
                if r.Method != "POST" {
                        http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
                        return
                }
                var body struct {
                        Client   string `json:"client"`
                        ClientID string `json:"client_id"` // GitHub-style alias
                }
                _ = json.NewDecoder(r.Body).Decode(&body)
                client := strings.ToLower(strings.TrimSpace(body.Client))
                if client == "" {
                        client = strings.ToLower(strings.TrimSpace(body.ClientID))
                }
                if client == "" {
                        writeJSON(w, 400, map[string]interface{}{"error": "client field is required — مثلا «cline» یا «hermes»"})
                        return
                }
                deviceCode := "omni_" + randomHex(20)
                userCode := newUserCode()
                g := st.CreateDeviceGrant(deviceCode, userCode, client)
                scheme := "http"
                if r.TLS != nil {
                        scheme = "https"
                }
                base := scheme + "://" + r.Host
                writeJSON(w, 200, map[string]interface{}{
                        "device_code":               g.DeviceCode,
                        "user_code":                 g.UserCode,
                        "verification_uri":          base + "/oauth/authorize",
                        "verification_uri_complete": base + "/oauth/authorize?user_code=" + g.UserCode,
                        "expires_in":                deviceGrantTTL,
                        "interval":                  2,
                })
        })

        mux.HandleFunc("/oauth/device/poll", func(w http.ResponseWriter, r *http.Request) {
                if r.Method != "POST" {
                        http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
                        return
                }
                var body struct {
                        DeviceCode string `json:"device_code"`
                        // GitHub-style clients also send client_id + grant_type — accepted
                        // and ignored so the same request shape works everywhere.
                        ClientID  string `json:"client_id"`
                        GrantType string `json:"grant_type"`
                }
                _ = json.NewDecoder(r.Body).Decode(&body)
                if body.DeviceCode == "" {
                        writeJSON(w, 400, map[string]interface{}{"error": "device_code is required"})
                        return
                }
                g, ok := st.LookupDeviceGrant(body.DeviceCode)
                if !ok {
                        writeJSON(w, 410, map[string]interface{}{"status": "expired", "error": "code expired or unknown — start again /oauth/device/start"})
                        return
                }
                switch g.Status {
                case "pending":
                        writeJSON(w, 200, map[string]interface{}{"status": "pending", "error": "authorization_pending", "interval": 2})
                case "denied":
                        st.DeleteDeviceGrant(g.DeviceCode)
                        writeJSON(w, 403, map[string]interface{}{"status": "denied", "error": "access_denied — the user declined the request"})
                case "approved":
                        st.DeleteDeviceGrant(g.DeviceCode)
                        writeJSON(w, 200, map[string]interface{}{
                                "status":       "approved",
                                "access_token": g.Key,
                                "token_type":   "bearer",
                                "scope":        "router:all",
                                "client":       g.Client,
                        })
                default:
                        writeJSON(w, 410, map[string]interface{}{"status": "expired", "error": "code expired — start again"})
                }
        })

        // Consent page — Persian RTL, same visual family as the dashboard.
        mux.HandleFunc("/oauth/authorize", func(w http.ResponseWriter, r *http.Request) {
                userCode := strings.TrimSpace(r.URL.Query().Get("user_code"))
                if userCode == "" {
                        userCode = strings.TrimSpace(r.URL.Query().Get("code"))
                }
                g, ok := st.LookupUserGrant(userCode)
                if userCode == "" || !ok {
                        w.Header().Set("Content-Type", "text/html; charset=utf-8")
                        fmt.Fprint(w, consentShell("کد پیدا نشد", "<p class='st err'>این کد نامعتبر یا منقضی شده است. در ابزار خود دوباره درخواست بدهید.</p>"))
                        return
                }
                meta, known := knownClients[g.Client]
                desc := meta.Desc
                label := clientLabel(g.Client)
                if !known {
                        desc = "کلاینت خارجی با پروتکل OpenAI/Anthropic"
                }
                w.Header().Set("Content-Type", "text/html; charset=utf-8")
                body := fmt.Sprintf(`
  <div class="card">
    <div class="brand"><span class="logo">◈</span> OmniRouter</div>
    <div class="client">
      <div class="cl-avatar">%s</div>
      <div><div class="cl-name">%s</div><div class="cl-desc">%s</div></div>
    </div>
    <p class="ask">درخواست اتصال به روتر</p>
    <div class="code">%s</div>
    <p class="hint">با تأیید، یک کلید مجازی اختصاصی برای این ابزار ساخته می‌شود که هر لحظه از داشبورد قابل لغو است.</p>
    <div class="actions">
      <button class="ok" id="btn-ok">تأیید و اتصال</button>
      <button class="no" id="btn-no">رد کردن</button>
    </div>
    <div id="res"></div>
  </div>
  <script>
  var uc = %q;
  function decide(d){
    fetch('/oauth/authorize/decision',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({user_code:uc,decision:d})})
      .then(function(r){return r.json().then(function(j){return {ok:r.ok,j:j}})})
      .then(function(x){
        var el=document.getElementById('res');
        if(x.ok && x.j.ok){el.innerHTML="<p class='st good'>متصل شد ✓ — به ابزار خود برگردید؛ کلید به‌صورت خودکار دریافت می‌شود.</p>";document.getElementById('btn-ok').style.display='none';document.getElementById('btn-no').style.display='none';}
        else if(x.ok){el.innerHTML="<p class='st err'>درخواست رد شد — می‌توانید این صفحه را ببندید.</p>";document.getElementById('btn-ok').style.display='none';document.getElementById('btn-no').style.display='none';}
        else{el.innerHTML="<p class='st err'>"+(x.j.error||'خطا')+"</p>";}
      }).catch(function(){document.getElementById('res').innerHTML="<p class='st err'>خطای شبکه</p>"});
  }
  document.getElementById('btn-ok').onclick=function(){decide('approve')};
  document.getElementById('btn-no').onclick=function(){decide('deny')};
  </script>`, clientInitial(label), html.EscapeString(label), html.EscapeString(desc), html.EscapeString(g.UserCode), g.UserCode)
                fmt.Fprint(w, consentShell("تأیید اتصال "+label, body))
        })

        // Decision endpoint: loopback is trusted (the local user's browser);
        // remote callers must hold an admin session.
        mux.HandleFunc("/oauth/authorize/decision", func(w http.ResponseWriter, r *http.Request) {
                if r.Method != "POST" {
                        http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
                        return
                }
                var body struct {
                        UserCode string `json:"user_code"`
                        Decision string `json:"decision"`
                }
                if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.UserCode == "" {
                        writeJSON(w, 400, map[string]interface{}{"error": "invalid body"})
                        return
                }
                if !isLoopback(r) && !admin.check(adminTokenFrom(r)) {
                        writeJSON(w, 401, map[string]interface{}{"error": "تأیید از راه دور فقط با نشست داشبورد مجاز است — از localhost تأیید کنید"})
                        return
                }
                decision := "approve"
                if strings.EqualFold(body.Decision, "deny") {
                        decision = "deny"
                }
                g, ok := st.ResolveDeviceGrant(strings.ToUpper(strings.TrimSpace(body.UserCode)), decision)
                if !ok {
                        writeJSON(w, 404, map[string]interface{}{"ok": false, "error": "کد فعال پیدا نشد — ممکن است منقضی یا تأیید شده باشد"})
                        return
                }
                writeJSON(w, 200, map[string]interface{}{"ok": true, "status": g.Status, "client": g.Client})
        })
}

func clientInitial(label string) string {
        for _, r := range label {
                if r != ' ' {
                        return strings.ToUpper(string(r))
                }
        }
        return "?"
}

// consentShell wraps the consent body in the shared page chrome.
func consentShell(title, body string) string {
        return `<!DOCTYPE html>
<html lang="fa" dir="rtl">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>` + html.EscapeString(title) + ` — OmniRouter</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
<link href="https://fonts.googleapis.com/css2?family=Vazirmatn:wght@400;600;700;800&display=swap" rel="stylesheet">
<style>
*{margin:0;padding:0;box-sizing:border-box}
body{min-height:100vh;display:flex;align-items:center;justify-content:center;padding:22px;
  background:#070b1a;color:#eceaf6;font-family:'Vazirmatn',system-ui,sans-serif;line-height:1.9;
  background-image:radial-gradient(700px 420px at 85% -10%,rgba(139,124,255,.16),transparent 60%),
    radial-gradient(560px 380px at 0% 100%,rgba(62,224,255,.10),transparent 60%)}
.card{width:min(430px,100%);background:rgba(18,22,44,.88);border:1px solid rgba(139,124,255,.22);
  border-radius:20px;padding:30px 28px;box-shadow:0 30px 80px rgba(0,0,0,.55)}
.brand{font-size:17px;font-weight:800;margin-bottom:20px;display:flex;align-items:center;gap:9px}
.brand .logo{color:#8b7cff}
.client{display:flex;align-items:center;gap:13px;background:rgba(139,124,255,.07);
  border:1px solid rgba(139,124,255,.16);border-radius:14px;padding:14px 16px}
.cl-avatar{width:44px;height:44px;border-radius:12px;background:linear-gradient(135deg,#8b7cff,#3ee0ff);
  display:flex;align-items:center;justify-content:center;font-weight:800;font-size:19px;color:#0b0e1e;flex-shrink:0}
.cl-name{font-weight:800;font-size:15px}
.cl-desc{font-size:12px;color:#9b98b3;margin-top:1px}
.ask{margin:18px 0 10px;font-size:13px;color:#9b98b3}
.code{font-family:'Vazirmatn',monospace;font-size:26px;font-weight:800;letter-spacing:5px;text-align:center;
  direction:ltr;background:#0b0e1e;border:1px dashed rgba(62,224,255,.35);border-radius:12px;padding:13px 8px;
  color:#3ee0ff;margin-bottom:14px}
.hint{font-size:12px;color:#6d6a85;margin-bottom:20px}
.actions{display:flex;gap:10px}
.actions button{flex:1;padding:12px;border:none;border-radius:11px;font:inherit;font-weight:700;font-size:14px;cursor:pointer;transition:.15s}
.actions .ok{background:linear-gradient(90deg,#8b7cff,#3ee0ff);color:#0b0e1e}
.actions .ok:hover{filter:brightness(1.12)}
.actions .no{background:transparent;border:1px solid rgba(248,113,113,.4);color:#f87171}
.actions .no:hover{background:rgba(248,113,113,.1)}
.st{margin-top:16px;text-align:center;font-size:13.5px;padding:11px;border-radius:10px}
.st.good{background:rgba(52,211,153,.12);color:#34d399}
.st.err{background:rgba(248,113,113,.12);color:#f87171}
</style>
</head>
<body>` + body + `</body>
</html>`
}

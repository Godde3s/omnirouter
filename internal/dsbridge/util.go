// util.go — small shared helpers for the DeepSeek bridge (package dsbridge).
// Ported subset of the Qwen bridge utilities (nothing upstream-specific).

package dsbridge

import (
        "crypto/rand"
        "encoding/hex"
        "encoding/json"
        "fmt"
        "strings"
        "sync"
)

// ---------- logging ----------

var logMu sync.Mutex

func logError(msg string) {
        logMu.Lock()
        defer logMu.Unlock()
        fmt.Println("[DS] [ERROR]", msg)
}

func logInfo(msg string) {
        logMu.Lock()
        defer logMu.Unlock()
        fmt.Println("[DS] [INFO]", msg)
}

func logAlways(msg string) {
        logMu.Lock()
        defer logMu.Unlock()
        fmt.Println("[DS]", msg)
}

// ---------- token estimation (matches the other bridges) ----------

func estimateTokens(text string) int {
        if text == "" {
                return 0
        }
        return (len(text) + 3) / 4
}

// ---------- message helpers ----------

func getMessageContent(content json.RawMessage) string {
        if len(content) == 0 {
                return ""
        }
        var s string
        if err := json.Unmarshal(content, &s); err == nil {
                return s
        }
        var arr []interface{}
        if err := json.Unmarshal(content, &arr); err == nil {
                var texts []string
                for _, item := range arr {
                        switch v := item.(type) {
                        case string:
                                texts = append(texts, v)
                        case map[string]interface{}:
                                t, _ := v["type"].(string)
                                if t == "text" {
                                        if txt, ok := v["text"].(string); ok {
                                                texts = append(texts, txt)
                                        }
                                }
                        }
                }
                return strings.Join(texts, "\n")
        }
        return string(content)
}

func messagesToPrompt(messages []Message) string {
        var sb strings.Builder
        for _, msg := range messages {
                content := getMessageContent(msg.Content)
                sb.WriteString(content)
                sb.WriteString("\n\n")
        }
        return strings.TrimSpace(sb.String())
}

func boolPtr(b bool) *bool { return &b }

func minInt(a, b int) int {
        if a < b {
                return a
        }
        return b
}

// ---------- ids ----------

func generateID() string {
        b := make([]byte, 16)
        rand.Read(b)
        return hex.EncodeToString(b)
}

func randomUUID() string {
        b := make([]byte, 16)
        rand.Read(b)
        b[6] = (b[6] & 0x0f) | 0x40
        b[8] = (b[8] & 0x3f) | 0x80
        return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

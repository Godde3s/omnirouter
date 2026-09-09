// Live model discovery for the Gemini bridge (package gbridge).
//
// The web app exposes the account's model registry through the batchexecute
// RPC "otAQ7b" (GET_USER_STATUS): part_body[15] lists every model with its
// internal hex id, display name and proto number; part_body[14] is the
// account status code. The bridge maps these to OpenAI-style ids:
//
//      Flash (3.6 Flash)      -> gemini-3.6-flash      (default)
//      Flash-Lite             -> gemini-3.5-flash-lite
//      Pro (3.1 Pro)          -> gemini-3.1-pro
//
// Discovery runs at startup and on every /v1/models call (5 min TTL), so new
// Google models appear without code changes — no hardcoded model names.

package gbridge

import (
        "context"
        "encoding/json"
        "fmt"
        "io"
        "net/http"
        "regexp"
        "sort"
        "strconv"
        "strings"
        "sync"
        "time"
)

// geminiModel is the resolved identity used to build requests.
type geminiModel struct {
        ModelID     string // internal hex id (e.g. fbb127bbb056c959)
        Name        string // OpenAI-style public id (e.g. gemini-3.6-flash)
        DisplayName string
        Description string
        Capacity    int
        ModelNumber int
        Aliases     []string
}

// registry cache
var (
        geminiModelMu       sync.Mutex
        geminiModelRegistry []geminiModel // ordered (first = default)
        geminiModelAt       time.Time
        accountStatusCode   int
        accountStatusAt     time.Time
)

const modelRegistryTTL = 10 * time.Minute

// fetchModelsFromGemini refreshes the registry via the otAQ7b RPC.
func fetchModelsFromGemini() []geminiModel {
        geminiModelMu.Lock()
        if len(geminiModelRegistry) > 0 && time.Since(geminiModelAt) < modelRegistryTTL {
                models := geminiModelRegistry
                geminiModelMu.Unlock()
                return models
        }
        geminiModelMu.Unlock()

        models, status := fetchModelsRPC()
        geminiModelMu.Lock()
        defer geminiModelMu.Unlock()
        if len(models) > 0 {
                geminiModelRegistry = models
                geminiModelAt = time.Now()
                logInfo(fmt.Sprintf("[Models] discovered %d model(s) live", len(models)))
        }
        if status > 0 {
                accountStatusCode = status
                accountStatusAt = time.Now()
        }
        if len(geminiModelRegistry) > 0 {
                return geminiModelRegistry
        }
        return nil // resolveGeminiModel falls back to static defaults
}

// batchUserStatus performs one batchexecute call for the user-status RPC.
func batchUserStatus(acc *Account) (string, int, error) {
        it, err := getInitTokens(acc)
        if err != nil {
                return "", 0, err
        }
        freq, _ := json.Marshal([]interface{}{
                []interface{}{[]interface{}{"otAQ7b", "[]", nil, "generic"}},
        })

        ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
        defer cancel()
        req, err := http.NewRequestWithContext(ctx, "POST", URL_BATCH_EXECUTE,
                strings.NewReader("f.req="+urlEncode(string(freq), "")))
        if err != nil {
                return "", 0, err
        }
        req.Header.Set("User-Agent", geminiUserAgent)
        req.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=UTF-8")
        req.Header.Set("Origin", "https://gemini.google.com")
        req.Header.Set("Referer", "https://gemini.google.com/")
        req.Header.Set(modelHeaderKey, "[1,null,null,null,null,null,null,null,[4,5,6,8],null,null,null,null,null,null,null]")
        if it.guest {
                if cc := guestConsentCookie(); cc != "" {
                        req.Header.Set("Cookie", cc)
                }
        } else {
                req.Header.Set("Cookie", accountCookieHeader(acc))
        }

        resp, err := geminiHTTPClient.Do(req)
        if err != nil {
                return "", 0, err
        }
        defer resp.Body.Close()
        body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
        return string(body), resp.StatusCode, nil
}

// fetchModelsRPC calls the RPC and parses models + status code.
func fetchModelsRPC() ([]geminiModel, int) {
        var acc *Account
        if accounts != nil && len(accounts.accounts) > 0 {
                acc = accounts.accounts[0]
        }
        body, _, err := batchUserStatus(acc)
        if err != nil {
                logError("model discovery RPC: " + err.Error())
                return nil, 0
        }

        status := 0
        var models []geminiModel

        for _, frame := range splitFramesLoose(body) {
                var envelope []interface{}
                if json.Unmarshal(frame, &envelope) != nil {
                        continue
                }
                for _, entry := range envelope {
                        part, ok := entry.([]interface{})
                        if !ok || len(part) < 3 {
                                continue
                        }
                        rpcID, _ := part[1].(string)
                        if rpcID != "" && rpcID != "otAQ7b" {
                                continue
                        }
                        innerStr, ok := part[2].(string)
                        if !ok {
                                continue
                        }
                        var pb []interface{}
                        if json.Unmarshal([]byte(innerStr), &pb) != nil {
                                continue
                        }
                        // status: pb[14]
                        if st, found := nestedInt(pb, 14); found && st != 0 {
                                status = st
                        }
                        // models: pb[15][i]
                        modelsList, found := nestedList(pb, 15)
                        if !found {
                                continue
                        }
                        capacity := computeCapacity(pb)
                        for _, mdata := range modelsList {
                                if m := parseModelRPC(mdata, capacity); m != nil {
                                        models = append(models, *m)
                                }
                        }
                }
                if status != 0 || len(models) > 0 {
                        break
                }
        }
        return models, status
}

// computeCapacity derives the tier capacity for model headers:
// free=1, plus/advanced=4 (capability flag 115) or 2 (tier flag 8 / cap 19).
func computeCapacity(pb []interface{}) int {
        tierFlags, _ := nestedList(pb, 16)
        capFlags, _ := nestedList(pb, 17)
        has := func(list []interface{}, want float64) bool {
                for _, v := range list {
                        if f, ok := v.(float64); ok && f == want {
                                return true
                        }
                }
                return false
        }
        switch {
        case has(capFlags, 115):
                return 4 // Plus
        case has(tierFlags, 16), has(capFlags, 106):
                return 3 // Pro (uncommon)
        case has(tierFlags, 8), has(capFlags, 19):
                return 2 // Pro
        }
        return 1 // Free
}

// parseModelRPC maps one registry entry to a geminiModel with aliases.
func parseModelRPC(mdata interface{}, capacity int) *geminiModel {
        arr, ok := mdata.([]interface{})
        if !ok || len(arr) == 0 {
                return nil
        }
        modelID, _ := arr[0].(string)
        if modelID == "" {
                return nil
        }
        category, _ := nestedString(arr, 1)
        if category == "" {
                category, _ = nestedString(arr, 10)
        }
        display, _ := nestedString(arr, 11)
        if display == "" {
                display, _ = nestedString(arr, 19)
        }
        if display == "" {
                display = category
        }
        desc, _ := nestedString(arr, 12)
        if desc == "" {
                desc, _ = nestedString(arr, 2)
        }
        num := 1
        if n, found := nestedInt(arr, 17); found && n > 0 {
                num = n
        } else if n, found := nestedInt(arr, 9); found && n > 0 {
                num = n
        }

        name := publicModelName(modelID, category, display)

        aliases := []string{modelID, strings.ToLower(name)}
        for _, a := range []string{category, display, strings.ToLower(display)} {
                if a != "" {
                        aliases = append(aliases, strings.ToLower(a), "gemini-"+strings.ToLower(strings.ReplaceAll(a, " ", "-")))
                }
        }
        aliases = dedupStrings(aliases)

        return &geminiModel{
                ModelID:     modelID,
                Name:        name,
                DisplayName: display,
                Description: desc,
                Capacity:    capacity,
                ModelNumber: num,
                Aliases:     aliases,
        }
}

// publicModelName maps category+version to a stable OpenAI-style id.
func publicModelName(modelID, category, display string) string {
        ver := ""
        if m := verRe.FindStringSubmatch(display); m != nil {
                ver = m[1]
        } else if m := verRe.FindStringSubmatch(category); m != nil {
                ver = m[1]
        }
        switch strings.ToLower(strings.TrimSpace(category)) {
        case "pro":
                if ver != "" {
                        return "gemini-" + ver + "-pro"
                }
                return "gemini-pro"
        case "flash":
                if ver != "" {
                        return "gemini-" + ver + "-flash"
                }
                return "gemini-flash"
        case "flash-lite":
                if ver != "" {
                        return "gemini-" + ver + "-flash-lite"
                }
                return "gemini-flash-lite"
        }
        if display != "" {
                slug := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(display), " ", "-"))
                if slug != "" {
                        return "gemini-" + slug
                }
        }
        return "gemini-" + modelID
}

var verRe = regexp.MustCompile(`(\d+(?:\.\d+)?)`)

// resolveGeminiModel maps a requested public id (with alias tolerance) to a
// registry model; unknown ids fall back to the first registry model and
// ultimately to the static default.
func resolveGeminiModel(name string) *geminiModel {
        fetchModelsFromGemini()

        geminiModelMu.Lock()
        registry := geminiModelRegistry
        geminiModelMu.Unlock()

        key := strings.ToLower(strings.TrimSpace(name))
        if key == "" {
                key = "gemini-3.6-flash"
        }

        for i := range registry {
                for _, a := range registry[i].Aliases {
                        if a == key {
                                m := registry[i]
                                return &m
                        }
                }
        }
        // Prefix tolerance: "gemini-3.6-flash-001" -> "gemini-3.6-flash"
        for i := range registry {
                for _, a := range registry[i].Aliases {
                        if strings.HasPrefix(a, key) || strings.HasPrefix(key, a+"-") {
                                m := registry[i]
                                return &m
                        }
                }
        }
        if len(registry) > 0 {
                m := registry[0]
                return &m
        }
        return staticModelFallback(key)
}

// staticModelFallback covers registries that could not be fetched.
func staticModelFallback(key string) *geminiModel {
        known := []geminiModel{
                {ModelID: "fbb127bbb056c959", Name: "gemini-3.6-flash", DisplayName: "3.6 Flash", Capacity: 1, ModelNumber: 1, Aliases: []string{"gemini-flash", "flash", "gemini-3.6-flash"}},
                {ModelID: "cf41b0e0dd7d53e5", Name: "gemini-3.5-flash-lite", DisplayName: "3.5 Flash-Lite", Capacity: 1, ModelNumber: 6, Aliases: []string{"gemini-flash-lite", "flash-lite", "gemini-3.5-flash-lite"}},
                {ModelID: "9d8ca3786ebdfbea", Name: "gemini-3.1-pro", DisplayName: "3.1 Pro", Capacity: 1, ModelNumber: 3, Aliases: []string{"gemini-pro", "pro", "gemini-3.1-pro"}},
        }
        for i := range known {
                for _, a := range known[i].Aliases {
                        if a == key {
                                return &known[i]
                        }
                }
        }
        return &known[0] // flash default
}

// listGeminiModelInfos converts the registry to the OpenAI /v1/models shape.
func listGeminiModelInfos() []ModelInfo {
        fetchModelsFromGemini()
        geminiModelMu.Lock()
        registry := geminiModelRegistry
        geminiModelMu.Unlock()

        var out []ModelInfo
        seen := map[string]bool{}
        for _, m := range registry {
                if seen[m.Name] {
                        continue
                }
                seen[m.Name] = true
                out = append(out, ModelInfo{
                        ID:           m.Name,
                        Name:         m.DisplayName,
                        Description:  m.Description,
                        Capabilities: map[string]interface{}{"thinking": true, "vision": true},
                        Created:      time.Now().Unix(),
                })
        }
        if len(out) == 0 {
                for _, m := range fallbackModels {
                        out = append(out, ModelInfo{
                                ID:           m.ID,
                                Name:         m.Name,
                                Description:  m.Description,
                                Capabilities: map[string]interface{}{"thinking": true, "vision": true},
                                Created:      time.Now().Unix(),
                        })
                }
        }
        sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
        return out
}

// ---------- tiny helpers ----------

func nestedList(v interface{}, path ...int) ([]interface{}, bool) {
        cur := v
        for _, idx := range path {
                arr, ok := cur.([]interface{})
                if !ok || idx < 0 || idx >= len(arr) {
                        return nil, false
                }
                cur = arr[idx]
        }
        list, ok := cur.([]interface{})
        return list, ok
}

func dedupStrings(in []string) []string {
        seen := map[string]bool{}
        var out []string
        for _, s := range in {
                if s == "" || seen[s] {
                        continue
                }
                seen[s] = true
                out = append(out, s)
        }
        return out
}

// splitFramesLoose splits a batchexecute body on length markers best-effort
// (tolerates partial trailing frames).
func splitFramesLoose(body string) []json.RawMessage {
        body = strings.TrimPrefix(body, ")]}'")
        var frames []json.RawMessage
        i := 0
        for i < len(body) {
                for i < len(body) && (body[i] == '\n' || body[i] == '\r' || body[i] == ' ' || body[i] == '\t') {
                        i++
                }
                d := i
                for d < len(body) && body[d] >= '0' && body[d] <= '9' {
                        d++
                }
                if d == i || d >= len(body) || body[d] != '\n' {
                        break
                }
                length, err := strconv.Atoi(body[i:d])
                if err != nil {
                        break
                }
                start := d + 1
                end := start + length
                if end > len(body) {
                        end = len(body)
                }
                seg := strings.TrimSpace(body[start:end])
                if seg != "" {
                        frames = append(frames, json.RawMessage(seg))
                }
                i = end
        }
        return frames
}

// ============================================================================
// /v1/models & /models HTTP HANDLERS
// ============================================================================

func modelsHandler(w http.ResponseWriter, r *http.Request) {
	now := time.Now().Unix()
	models := listGeminiModelInfos()
	data := make([]map[string]interface{}, 0, len(models))
	for _, m := range models {
		created := m.Created
		if created == 0 {
			created = now
		}
		entry := map[string]interface{}{
			"id":           m.ID,
			"object":       "model",
			"created":      created,
			"owned_by":     "google",
			"display_name": m.Name,
			"description":  m.Description,
		}
		data = append(data, entry)
	}
	writeJSON(w, 200, map[string]interface{}{
		"object": "list",
		"data":   data,
	})
}

func modelsHandler2(w http.ResponseWriter, r *http.Request) {
	models := listGeminiModelInfos()
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	currentModel := "gemini-3.6-flash"
	if len(ids) > 0 {
		currentModel = ids[0]
	}
	writeJSON(w, 200, map[string]interface{}{
		"models":       ids,
		"currentModel": currentModel,
	})
}

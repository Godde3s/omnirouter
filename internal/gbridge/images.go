// Real vision support for the Gemini bridge (package gbridge).
//
// Gemini web accepts images through Google's content-push endpoint:
//
//      POST https://content-push.googleapis.com/upload
//           X-Tenant-Id: bard-storage
//           Push-ID: <push id from init page>
//           multipart field "file"
//      -> plain-text file reference like "/contrib_service/ttl_1d/..."
//
// The reference goes into message_content[3] (req_file_data) of the generate
// request. OpenAI-style image_url parts (data URIs and http(s) URLs) are
// downloaded/decoded and uploaded under the identity that will serve the
// completion.

package gbridge

import (
        "bytes"
        "context"
        "encoding/base64"
        "encoding/json"
        "fmt"
        "io"
        "mime"
        "net/http"
        "strings"
        "time"
)

type imageUpload struct {
        Ref      string // /contrib_service/ttl_1d/...
        Filename string
        MimeType string
}

// processVisionMessagesAs scans messages for image parts, uploads each one
// and returns (cleaned text-only messages, uploads, error).
func processVisionMessagesAs(ctx context.Context, acc *Account, raw json.RawMessage) (json.RawMessage, []imageUpload, error) {
        var msgs []Message
        if err := json.Unmarshal(raw, &msgs); err != nil {
                return raw, nil, nil // not our shape — pass through untouched
        }

        var uploads []imageUpload
        changed := false
        for i, m := range msgs {
                var parts []map[string]interface{}
                if json.Unmarshal(m.Content, &parts) != nil {
                        continue
                }
                out := make([]map[string]interface{}, 0, len(parts))
                for _, p := range parts {
                        t, ok := p["type"].(string)
                        if !ok || (t != "image_url" && t != "image" && t != "input_image") {
                                out = append(out, p)
                                continue
                        }
                        var src string
                        if iu, ok := p["image_url"].(map[string]interface{}); ok {
                                src, _ = iu["url"].(string)
                        } else if s, ok := p["url"].(string); ok {
                                src = s
                        } else if s, ok := p["image"].(string); ok {
                                src = s
                        }
                        if src == "" {
                                return raw, nil, fmt.Errorf("تصویر بدون URL در image_url")
                        }
                        up, err := uploadImageFromSrc(ctx, acc, src)
                        if err != nil {
                                return raw, nil, fmt.Errorf("آپلود تصویر ناموفق بود: %v", err)
                        }
                        uploads = append(uploads, *up)
                        changed = true
                }
                if changed {
                        if rawOut, err := json.Marshal(out); err == nil {
                                msgs[i].Content = rawOut
                        }
                }
        }
        if !changed || len(uploads) == 0 {
                return raw, nil, nil
        }
        rawOut, err := json.Marshal(msgs)
        if err != nil {
                return raw, nil, nil
        }
        return rawOut, uploads, nil
}

// uploadImageFromSrc accepts data URIs and http(s) URLs.
func uploadImageFromSrc(ctx context.Context, acc *Account, src string) (*imageUpload, error) {
        var data []byte
        var mimeType string
        var filename string

        if strings.HasPrefix(src, "data:") {
                // data:image/png;base64,AAAA...
                comma := strings.Index(src, ",")
                if comma < 0 {
                        return nil, fmt.Errorf("data URI نامعتبر")
                }
                header := src[:comma]
                payload := src[comma+1:]
                if i := strings.Index(header, ";"); i > 0 {
                        mimeType = header[len("data:"):i]
                } else {
                        mimeType = header[len("data:"):]
                }
                decoded, err := base64.StdEncoding.DecodeString(payload)
                if err != nil {
                        return nil, fmt.Errorf("base64 نامعتبر: %v", err)
                }
                data = decoded
        } else if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") {
                dl, err := downloadImage(ctx, src)
                if err != nil {
                        return nil, err
                }
                data, mimeType = dl.data, dl.mimeType
        } else {
                return nil, fmt.Errorf("پشتیبانی نمی‌شود — فقط data URI یا http(s) URL")
        }

        if mimeType == "" || !strings.Contains(mimeType, "/") {
                mimeType = "image/png"
        }
        filename = fmt.Sprintf("input_%d%s", time.Now().UnixNano()%10000000, extFromMime(mimeType))

        ref, err := pushUpload(ctx, acc, filename, mimeType, data)
        if err != nil {
                return nil, err
        }
        return &imageUpload{Ref: ref, Filename: filename, MimeType: mimeType}, nil
}

type dlResult struct {
        data     []byte
        mimeType string
}

func downloadImage(ctx context.Context, url string) (*dlResult, error) {
        ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
        defer cancel()
        req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
        if err != nil {
                return nil, err
        }
        req.Header.Set("User-Agent", geminiUserAgent)
        resp, err := geminiHTTPClient.Do(req)
        if err != nil {
                return nil, err
        }
        defer resp.Body.Close()
        if resp.StatusCode != 200 {
                return nil, fmt.Errorf("دانلود تصویر وضعیت %d داد", resp.StatusCode)
        }
        data, err := io.ReadAll(io.LimitReader(resp.Body, 20<<20))
        if err != nil {
                return nil, err
        }
        ct := resp.Header.Get("Content-Type")
        if i := strings.Index(ct, ";"); i > 0 {
                ct = ct[:i]
        }
        return &dlResult{data: data, mimeType: strings.TrimSpace(ct)}, nil
}

func extFromMime(m string) string {
        if exts, _ := mime.ExtensionsByType(m); len(exts) > 0 {
                return exts[0]
        }
        return ".png"
}

// pushUpload posts one file to content-push and returns the reference.
func pushUpload(ctx context.Context, acc *Account, filename, mimeType string, data []byte) (string, error) {
        if len(data) > 20<<20 {
                return "", fmt.Errorf("تصویر بزرگ‌تر از ۲۰ مگابایت است")
        }
        it, err := getInitTokens(acc)
        if err != nil {
                return "", err
        }

        body := &strings.Builder{}
        boundary := "-------geminiBridge" + generateID()
        // multipart/form-data with a single "file" field
        body.WriteString("--" + boundary + "\r\n")
        body.WriteString(fmt.Sprintf("Content-Disposition: form-data; name=\"file\"; filename=\"%s\"\r\n", filename))
        body.WriteString("Content-Type: " + mimeType + "\r\n\r\n")
        head := body.String()

        ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
        defer cancel()
        req, err := http.NewRequestWithContext(ctx, "POST", URL_UPLOAD,
                io.MultiReader(strings.NewReader(head), bytes.NewReader(data), strings.NewReader(tailBoundary(boundary))))
        if err != nil {
                return "", err
        }
        req.Header.Set("User-Agent", geminiUserAgent)
        req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
        req.Header.Set("Origin", "https://gemini.google.com")
        req.Header.Set("Referer", "https://gemini.google.com/")
        req.Header.Set("X-Tenant-Id", "bard-storage")
        req.Header.Set("Push-ID", it.PushID)
        if it.guest {
                if cc := guestConsentCookie(); cc != "" {
                        req.Header.Set("Cookie", cc)
                }
        } else {
                req.Header.Set("Cookie", accountCookieHeader(acc))
        }

        resp, err := geminiHTTPClient.Do(req)
        if err != nil {
                return "", err
        }
        defer resp.Body.Close()
        ref, err := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
        if err != nil {
                return "", err
        }
        if resp.StatusCode != 200 {
                return "", fmt.Errorf("content-push وضعیت %d داد", resp.StatusCode)
        }
        refStr := strings.TrimSpace(string(ref))
        if !strings.HasPrefix(refStr, "/") {
                return "", fmt.Errorf("پاسخ آپلود قابل قبول نبود: %s", truncateStr(refStr, 80))
        }
        return refStr, nil
}

func tailBoundary(boundary string) string {
        return "\r\n--" + boundary + "--\r\n"
}

func truncateStr(s string, n int) string {
        if len(s) <= n {
                return s
        }
        return s[:n] + "..."
}

// fileDataFromUploads builds the req_file_data slot: [["ref1","file1","mime1"],...]
func fileDataFromUploads(uploads []imageUpload) interface{} {
        if len(uploads) == 0 {
                return nil
        }
        var arr []interface{}
        for _, u := range uploads {
                arr = append(arr, []interface{}{u.Ref, u.Filename, u.MimeType})
        }
        return []interface{}{arr}
}

// extractImageParts finds image parts in raw messages (compat shim used by
// the legacy agent transform; Gemini vision goes through direct upload, so
// this returns nil and attach is a no-op).
func extractImageParts(raw json.RawMessage) []map[string]interface{} {
	return nil
}

// attachImageParts re-attaches image parts after the agent transform (no-op).
func attachImageParts(transformed json.RawMessage, imageParts []map[string]interface{}) ([]byte, error) {
	return []byte(transformed), nil
}

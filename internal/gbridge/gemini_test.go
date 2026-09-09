// Unit tests for the Gemini upstream pieces: the streaming frame parser,
// snapshot delta logic, model header construction, request list building,
// error classification and the model registry mapping.

package gbridge

import (
        "encoding/json"
        "strconv"
        "strings"
        "testing"
)

// ---------- frame parser ----------
// NOTE: empirically the marker counts the fragment's UTF-16 units + 2, so
// Google-shaped test data uses marker = len(json) + 2. The parser itself is
// boundary-driven and never slices by the numeric marker.

func mustFrame(t *testing.T, fp *frameParser, data string) []json.RawMessage {
	t.Helper()
	frames, err := fp.feed([]byte(data))
	if err != nil {
		t.Fatalf("feed error: %v", err)
	}
	return frames
}

func TestFrameParserGoogleShapedBody(t *testing.T) {
	// Mirrors the real dump: prefix, blank line, frames, final e-frame
	// without a trailing marker (ends at EOF).
	body := ")]}'\n\n" +
		"177\n" + `[["wrb.fr",null,"[null,1]"]]` + "\n" + // json=175? no: marker = len+2 for realism
		"1394\n" + `X` + "\n" +
		"27\n" + `[["e",10,null,null,4866]]`
	// build correct markers: marker = len(json)+2
	f1 := `[["wrb.fr",null,"[null,1]"]]`
	f2 := `[["wrb.fr",null,"[null,2]"]]`
	f3 := `[["e",10,null,null,4866]]`
	body = ")]}'\n\n" +
		itoa(len(f1)+2) + "\n" + f1 + "\n" +
		itoa(len(f2)+2) + "\n" + f2 + "\n" +
		itoa(len(f3)+2) + "\n" + f3

	fp := newFrameParser()
	frames := mustFrame(t, fp, body)
	tail, err := fp.flush()
	if err != nil {
		t.Fatalf("flush: %v", err)
	}
	frames = append(frames, tail...)
	if len(frames) != 3 {
		t.Fatalf("expected 3 frames, got %d: %s", len(frames), frames)
	}
	var a []interface{}
	if err := json.Unmarshal(frames[0], &a); err != nil {
		t.Fatalf("frame 0 unmarshal: %v", err)
	}
	var c []interface{}
	if err := json.Unmarshal(frames[2], &c); err != nil {
		t.Fatalf("final frame unmarshal: %v", err)
	}
	if arr, ok := c[0].([]interface{}); !ok || arr[0] != "e" {
		t.Fatalf("final frame content wrong: %s", frames[2])
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

func TestFrameParserIncrementalChunks(t *testing.T) {
	frag := `["wrb.fr",null,"[1,2,3]"]`
	body := ")]}'\n\n" + itoa(len(frag)+2) + "\n" + frag + "\n" + itoa(len(frag)+2) + "\n" + frag
	// split the body into awkward chunk boundaries
	var chunks []string
	pos := 0
	for _, cut := range []int{3, 5, 9, 17, 33, 40, 55} {
		if cut > len(body) {
			break
		}
		chunks = append(chunks, body[pos:cut])
		pos = cut
	}
	chunks = append(chunks, body[pos:])

	fp := newFrameParser()
	var all []json.RawMessage
	for i, c := range chunks {
		frames, err := fp.feed([]byte(c))
		if err != nil {
			t.Fatalf("chunk %d error: %v", i, err)
		}
		all = append(all, frames...)
	}
	tail, _ := fp.flush()
	all = append(all, tail...)
	if len(all) != 2 {
		t.Fatalf("expected exactly 2 frames after all chunks, got %d", len(all))
	}
	var env []interface{}
	if err := json.Unmarshal(all[1], &env); err != nil {
		t.Fatalf("frame unmarshal: %v — %s", err, all[1])
	}
}

func TestFrameParserUTF16Astral(t *testing.T) {
	// An astral emoji inside the fragment; marker = utf16 units + 2.
	frag := `["😀x"]`           // 5 chars, 6 utf-16 units
	body := ")]}'\n\n8\n" + frag + "\n10\n" + frag // both markers = units+2
	fp := newFrameParser()
	frames := mustFrame(t, fp, body)
	tail, _ := fp.flush()
	frames = append(frames, tail...)
	if len(frames) != 2 {
		t.Fatalf("astral frames must parse via boundaries, got %d", len(frames))
	}
	var s []interface{}
	if err := json.Unmarshal(frames[0], &s); err != nil || s[0] != "😀x" {
		t.Fatalf("astral content wrong: %s", frames[0])
	}
}

func TestFrameParserNoPrefix(t *testing.T) {
	// Some proxies strip the XSSI prefix — parser must cope.
	frag := `[1,2]`
	fp := newFrameParser()
	frames := mustFrame(t, fp, itoa(len(frag)+2)+"\n"+frag+"\n12\n[3]\n")
	tail, _ := fp.flush()
	frames = append(frames, tail...)
	if len(frames) != 2 {
		t.Fatalf("prefix-less body must parse, got %d", len(frames))
	}
}

func TestFrameParserSingleFinalFrameAtEOF(t *testing.T) {
	// The last frame has no following marker; it must surface via flush().
	frag := `[["e",10,null,null,4866]]`
	fp := newFrameParser()
	frames := mustFrame(t, fp, ")]}'\n\n"+itoa(len(frag)+2)+"\n"+frag+"\n")
	if len(frames) != 0 {
		t.Fatalf("final frame must wait for EOF, got %d early", len(frames))
	}
	tail, _ := fp.flush()
	if len(tail) != 1 {
		t.Fatalf("flush must yield the final frame, got %d", len(tail))
	}
	var e []interface{}
	if err := json.Unmarshal(tail[0], &e); err != nil {
		t.Fatalf("final frame unmarshal: %v", err)
	}
	if arr, ok := e[0].([]interface{}); !ok || arr[0] != "e" {
		t.Fatalf("final frame content wrong: %s", tail[0])
	}
}

// ---------- snapshot deltas ----------

func TestSnapshotDeltaGrowing(t *testing.T) {
        store := map[string]string{}
        if d := snapshotDelta(store, "rc1", "سلام"); d != "سلام" {
                t.Fatalf("first delta = %q", d)
        }
        if d := snapshotDelta(store, "rc1", "سلام دنیا"); d != " دنیا" {
                t.Fatalf("grow delta = %q", d)
        }
        if d := snapshotDelta(store, "rc1", "سلام دنیا"); d != "" {
                t.Fatalf("no-change delta must be empty, got %q", d)
        }
}

func TestSnapshotDeltaRewrite(t *testing.T) {
        store := map[string]string{}
        _ = snapshotDelta(store, "r", "abc def")
        // rewrite that shares the "abc " prefix
        d := snapshotDelta(store, "r", "abc XYZ")
        if d == "" || strings.Contains(d, "def") {
                t.Fatalf("rewrite delta = %q — must not contain stale tail", d)
        }
}

// ---------- model header + request list ----------

func TestBuildModelHeader(t *testing.T) {
        h := buildModelHeader("fbb127bbb056c959", 1, 1, true, "UUID-1")
        var arr []interface{}
        if err := json.Unmarshal([]byte(h), &arr); err != nil {
                t.Fatalf("header is not JSON: %v", err)
        }
        if arr[4] != "fbb127bbb056c959" {
                t.Fatalf("model id slot wrong: %v", arr[4])
        }
        if arr[15] != float64(2) { // thinking flag
                t.Fatalf("thinking flag slot wrong: %v", arr[15])
        }
        if arr[16] != "UUID-1" {
                t.Fatalf("session uuid slot wrong: %v", arr[16])
        }
        h2 := buildModelHeader("x", 1, 1, false, "U")
        var arr2 []interface{}
        _ = json.Unmarshal([]byte(h2), &arr2)
        if arr2[15] != float64(1) {
                t.Fatalf("non-thinking flag must be 1, got %v", arr2[15])
        }
}

func TestBuildInnerReqList(t *testing.T) {
        arr := buildInnerReqList("hello", nil, "en", false, 3)
        if len(arr) != 81 {
                t.Fatalf("inner list length = %d, want 81", len(arr))
        }
        msg := arr[0].([]interface{})
        if msg[0] != "hello" {
                t.Fatalf("prompt slot wrong: %v", msg[0])
        }
        if arr[STREAMING_INDEX] != 1 {
                t.Fatal("streaming flag must be 1")
        }
        if arr[TEMPCHAT_INDEX] != 1 {
                t.Fatal("temporary-chat flag must be set for stateless bridge")
        }
        if arr[79] != 3 {
                t.Fatalf("model number slot = %v, want 3", arr[79])
        }
}

// ---------- text cleaning ----------

func TestCleanGeminiText(t *testing.T) {
        if got := cleanGeminiText("متن\n```"); got != "متن" {
                t.Fatalf("code-fence suffix must be stripped, got %q", got)
        }
        if got := cleanGeminiText("متن<FollowUp x/>"); got != "متن" {
                t.Fatalf("follow-up chip must be stripped, got %q", got)
        }
        if got := cleanGeminiText("سالم"); got != "سالم" {
                t.Fatalf("clean text must pass through, got %q", got)
        }
}

// ---------- error classification ----------

func TestClassifyGeminiAPICode(t *testing.T) {
        cases := []struct {
                code   int
                status int
        }{
                {1037, 429},
                {1050, 400},
                {1052, 400},
                {1060, 403},
                {1013, 502},
        }
        for _, tc := range cases {
                err := classifyGeminiAPICode(tc.code)
                ue, ok := err.(*upstreamErr)
                if !ok {
                        t.Fatalf("code %d: expected *upstreamErr", tc.code)
                }
                if ue.status != tc.status {
                        t.Fatalf("code %d status = %d, want %d", tc.code, ue.status, tc.status)
                }
        }
}

func TestStatusFromError(t *testing.T) {
        if statusFromError("سقف درخواست گوگل برای این IP/اکانت پر شده است (HTTP 429)") != 429 {
                t.Fatal("429 not detected")
        }
        if statusFromError("کوکی‌های اکانت گوگل رد شدند (HTTP 401)") != 401 {
                t.Fatal("401 not detected")
        }
        if statusFromError("unknown failure") != 502 {
                t.Fatal("default must be 502")
        }
}

// ---------- model registry mapping ----------

func TestParseModelRPC(t *testing.T) {
        m := parseModelRPC([]interface{}{
                "fbb127bbb056c959", "Flash", nil, nil, nil, nil, nil, nil, nil, 1,
                nil, "3.6 Flash", "Fast model", nil, nil, nil, nil, 1,
        }, 1)
        if m == nil {
                t.Fatal("parseModelRPC returned nil")
        }
        if m.ModelID != "fbb127bbb056c959" || m.ModelNumber != 1 {
                t.Fatalf("model identity wrong: %+v", m)
        }
        if m.Name != "gemini-3.6-flash" {
                t.Fatalf("public name = %q, want gemini-3.6-flash", m.Name)
        }
}

func TestResolveGeminiModelFallback(t *testing.T) {
        // Registry is empty in tests -> static fallback must answer aliases.
        m := resolveGeminiModel("gemini-pro")
        if m == nil || m.ModelID != "9d8ca3786ebdfbea" {
                t.Fatalf("gemini-pro alias must resolve to pro id, got %+v", m)
        }
        m2 := resolveGeminiModel("3.6-flash")
        if m2 == nil || m2.ModelID != "fbb127bbb056c959" {
                t.Fatalf("flash alias must resolve, got %+v", m2)
        }
        m3 := resolveGeminiModel("totally-unknown")
        if m3 == nil || m3.Name != "gemini-3.6-flash" {
                t.Fatalf("unknown model must fall back to flash default, got %+v", m3)
        }
}

func TestPublicModelName(t *testing.T) {
        cases := []struct {
                id, cat, disp, want string
        }{
                {"x", "Flash", "3.6 Flash", "gemini-3.6-flash"},
                {"y", "Flash-Lite", "3.5 Flash-Lite", "gemini-3.5-flash-lite"},
                {"z", "Pro", "3.1 Pro", "gemini-3.1-pro"},
                {"w", "Pro", "", "gemini-pro"},
        }
        for _, tc := range cases {
                if got := publicModelName(tc.id, tc.cat, tc.disp); got != tc.want {
                        t.Fatalf("publicModelName(%q,%q,%q) = %q, want %q", tc.id, tc.cat, tc.disp, got, tc.want)
                }
        }
}

// ---------- nested helpers ----------

func TestNestedStringAndList(t *testing.T) {
        v := []interface{}{"a", []interface{}{1.0, "mid", []interface{}{"deep"}}}
        if s, ok := nestedString(v, 1, 2, 0); !ok || s != "deep" {
                t.Fatalf("nestedString failed: %q %v", s, ok)
        }
        if _, ok := nestedString(v, 5); ok {
                t.Fatal("out-of-range must not be found")
        }
        if n, found := nestedInt(v, 1, 0); !found || n != 1 {
                t.Fatalf("nestedInt failed: %v %v", n, found)
        }
}

// ---------- cookie header ----------

func TestAccountCookieHeader(t *testing.T) {
        a := newAccount(1, "PSID-VAL", "PSIDTS-VAL")
        if h := accountCookieHeader(a); h != "__Secure-1PSID=PSID-VAL; __Secure-1PSIDTS=PSIDTS-VAL" {
                t.Fatalf("cookie header wrong: %q", h)
        }
        if h := accountCookieHeader(nil); h != "" {
                t.Fatalf("nil account header must be empty")
        }
}

func TestExtractSetCookie(t *testing.T) {
        raw := "__Secure-1PSIDTS=abCd; Path=/; Secure; HttpOnly"
        if got := extractSetCookie(raw, "__Secure-1PSIDTS"); got != "abCd" {
                t.Fatalf("extract = %q", got)
        }
        if got := extractSetCookie(raw, "__Secure-1PSID"); got != "" {
                t.Fatalf("wrong cookie must not match, got %q", got)
        }
}

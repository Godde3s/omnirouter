// tokensaver.go — RTK-style Token Saver + prompt modes (9router parity).
//
// RTK Token Saver: tool outputs (git diff, grep, find/ls/tree, log dumps…)
// routinely eat 30-50% of an agent's prompt budget. Before a request is
// forwarded, every tool_result/tool message is passed through an
// auto-detected, lossless-when-possible compressor. If a filter errors or
// fails to shrink the payload the original text is kept — errors never
// break a request (safe by design, exactly like 9router's RTK).
//
//   - git-diff        → file list + per-file delta summary, hunks dropped
//   - git-status      → collapsed short-status table
//   - grep            → dedup + per-file line caps
//   - find/ls/tree    → path listing collapsed to dirs + counts
//   - dedup-log       → repeated lines collapsed to "line ×N"
//   - smart-truncate  → generic oversized text: head+tail with a marker
//
// Prompt modes (output-side savers, stack with RTK):
//   - caveman       → terse replies, technical substance preserved
//   - ponytail-*    → "lazy senior dev": minimal, YAGNI-first code
//
// Per-request control:
//   X-Omni-Token-Saver: off   → bypass everything for this request
//   X-Omni-Prompt-Mode: off|caveman|ponytail-lite|ponytail-full|ponytail-ultra
//
// Runtime state lives in saverState; defaults come from RTK / PROMPT_MODE
// env and are changed live from the dashboard (/admin/api/saver).

package core

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
)

// ---------- runtime settings ----------

type saverState struct {
	rtk        atomic.Bool
	promptMode atomic.Value // string
	savedBytes atomic.Int64
	savedCalls atomic.Int64
}

// saver starts with production defaults (RTK on, prompt mode off) so that
// unit tests and early requests see sane values even before initSaver().
var saver = func() (s saverState) {
	s.rtk.Store(true)
	s.promptMode.Store("off")
	return
}()

func initSaver() {
	saver.rtk.Store(strings.EqualFold(envOr("RTK", "on"), "on"))
	saver.promptMode.Store(strings.ToLower(envOr("PROMPT_MODE", "off")))
}

// promptModeNow reads the mode safely (zero value before initSaver = "off").
func promptModeNow() string {
	if v := saver.promptMode.Load(); v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return "off"
}

// SaverSnapshot is the admin/dashboard view of the saver state.
func SaverSnapshot() map[string]interface{} {
	return map[string]interface{}{
		"rtk":         saver.rtk.Load(),
		"prompt_mode": promptModeNow(),
		"saved_bytes": saver.savedBytes.Load(),
		"saved_calls": saver.savedCalls.Load(),
	}
}

// ---------- public entry points ----------

const (
	ponytailLite  = "Build exactly what is asked. When a lazier alternative exists, name it in one short line after the code."
	ponytailFull  = "You are a lazy senior developer. Practice the YAGNI ladder: stdlib before new deps, native before wrappers, existing deps before new code, one-liners before abstractions, minimal code before architecture. Write the shortest working diff. No speculative features, no \"just in case\" scaffolding, no unrequested abstractions. Never trade away input validation, error handling that prevents data loss, security, accessibility, or anything explicitly requested."
	ponytailUltra = "You are a YAGNI extremist. Deletion first. Ship the shortest working diff — ideally a one-liner. Challenge every requirement that smells speculative, in the same response, in one line. No abstractions, no wrappers, no future-proofing. Never trade away input validation, error handling that prevents data loss, security, accessibility, or anything explicitly requested."
	cavemanText   = "Reply in terse caveman-engineer style: short sentences, no filler, no apologies, no restating the question. Keep all technical substance: code, exact commands, file paths, numbers. Skip pleasantries and transitions entirely."
)

var promptModeText = map[string]string{
	"caveman":         cavemanText,
	"ponytail-lite":   ponytailLite,
	"ponytail-full":   ponytailFull,
	"ponytail-ultra":  ponytailUltra,
}

// ValidPromptMode reports whether a mode id is supported.
func ValidPromptMode(m string) bool {
	switch m {
	case "off", "caveman", "ponytail-lite", "ponytail-full", "ponytail-ultra":
		return true
	}
	return false
}

// applySaver mutates a request body before forwarding. header carries the
// per-request bypass switches. Returns the (possibly) rewritten body and
// how many bytes the RTK pass shaved off the tool payloads.
func applySaver(body []byte, header http.Header) ([]byte, int64) {
	bypass := strings.EqualFold(strings.TrimSpace(header.Get("X-Omni-Token-Saver")), "off")
	mode := strings.ToLower(strings.TrimSpace(header.Get("X-Omni-Prompt-Mode")))
	if mode == "" {
		mode = promptModeNow()
	}
	if mode != "" && !ValidPromptMode(mode) {
		mode = "off"
	}

	orig := body
	var saved int64
	if saver.rtk.Load() && !bypass {
		if out, n := rtkCompressBody(body); n > 0 {
			body = out
			saved = n
			saver.savedBytes.Add(n)
			saver.savedCalls.Add(1)
		}
	}
	if !bypass && mode != "" && mode != "off" {
		if out, ok := injectPromptMode(body, mode); ok {
			body = out
		}
	}
	if len(body) != len(orig) {
		return body, saved
	}
	return body, saved
}

// ---------- body-level plumbing (OpenAI + Anthropic shapes) ----------

// rtkCompressBody walks the two supported request shapes and compresses
// every tool payload it finds. Unknown shapes pass through untouched.
func rtkCompressBody(body []byte) ([]byte, int64) {
	var top map[string]json.RawMessage
	if json.Unmarshal(body, &top) != nil {
		return body, 0
	}
	rawMsgs, ok := top["messages"]
	if !ok {
		return body, 0
	}
	var msgs []map[string]interface{}
	if json.Unmarshal(rawMsgs, &msgs) != nil {
		return body, 0
	}

	changed := false
	var saved int64

	// Anthropic: messages[].content[] blocks with type=tool_result.
	// OpenAI:    messages[] with role=tool and a string content.
	for i := range msgs {
		role, _ := msgs[i]["role"].(string)
		if role == "tool" {
			if c, ok := msgs[i]["content"].(string); ok && c != "" {
				if out := rtkCompress(c); len(out) < len(c) {
					msgs[i]["content"] = out
					saved += int64(len(c) - len(out))
					changed = true
				}
				continue
			}
		}
		blocks, ok := msgs[i]["content"].([]interface{})
		if !ok {
			continue
		}
		for j := range blocks {
			b, ok := blocks[j].(map[string]interface{})
			if !ok {
				continue
			}
			if t, _ := b["type"].(string); t == "tool_result" {
				inner, ok := b["content"].(string)
				if !ok || inner == "" {
					continue
				}
				if out := rtkCompress(inner); len(out) < len(inner) {
					b["content"] = out
					saved += int64(len(inner) - len(out))
					changed = true
				}
			}
		}
	}
	if !changed {
		return body, 0
	}
	msgsRaw, _ := json.Marshal(msgs)
	top["messages"] = msgsRaw
	out, err := json.Marshal(top)
	if err != nil {
		return body, 0
	}
	return out, saved
}

// injectPromptMode appends the mode instruction. OpenAI: prepend a system
// message. Anthropic: append to the system field (string or blocks).
func injectPromptMode(body []byte, mode string) ([]byte, bool) {
	instr, ok := promptModeText[mode]
	if !ok {
		return body, false
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(body, &top) != nil {
		return body, false
	}

	if _, isAnthropic := top["system"]; isAnthropic {
		switch {
		case len(top["system"]) > 0 && top["system"][0] == '"':
			var s string
			_ = json.Unmarshal(top["system"], &s)
			merged := strings.TrimSpace(s) + "\n\n" + instr
			top["system"], _ = json.Marshal(strings.TrimSpace(merged))
		default:
			var blocks []map[string]interface{}
			if json.Unmarshal(top["system"], &blocks) == nil && blocks != nil {
				blocks = append(blocks, map[string]interface{}{"type": "text", "text": instr})
				top["system"], _ = json.Marshal(blocks)
			} else {
				top["system"], _ = json.Marshal(instr)
			}
		}
		out, err := json.Marshal(top)
		if err != nil {
			return body, false
		}
		return out, true
	}

	rawMsgs, ok := top["messages"]
	if !ok {
		return body, false
	}
	var msgs []map[string]interface{}
	if json.Unmarshal(rawMsgs, &msgs) != nil {
		return body, false
	}
	// prepend a system message; merge into an existing leading one
	if len(msgs) > 0 {
		if r, _ := msgs[0]["role"].(string); r == "system" {
			if c, ok := msgs[0]["content"].(string); ok {
				msgs[0]["content"] = strings.TrimSpace(c) + "\n\n" + instr
				msgsRaw, _ := json.Marshal(msgs)
				top["messages"] = msgsRaw
				out, err := json.Marshal(top)
				if err != nil {
					return body, false
				}
				return out, true
			}
		}
	}
	sysMsg := map[string]interface{}{"role": "system", "content": instr}
	msgs = append([]map[string]interface{}{sysMsg}, msgs...)
	msgsRaw, _ := json.Marshal(msgs)
	top["messages"] = msgsRaw
	out, err := json.Marshal(top)
	if err != nil {
		return body, false
	}
	return out, true
}

// ---------- auto-detect + filters ----------

const (
	rtkPeekBytes  = 1024 // like RTK: peek the first 1KB to pick a filter
	rtkMinPayload = 240  // don't bother with tiny payloads
	rtkMaxHead    = 3584 // smart-truncate head budget
	rtkMaxTail    = 1024 // smart-truncate tail budget
)

// rtkCompress auto-detects the payload kind from its first 1KB and applies
// the matching filter. Safe by design: any miss returns the original.
func rtkCompress(s string) string {
	if len(s) < rtkMinPayload {
		return s
	}
	peek := s
	if len(peek) > rtkPeekBytes {
		peek = peek[:rtkPeekBytes]
	}
	switch {
	case strings.Contains(peek, "diff --git "):
		return filterGitDiff(s)
	case strings.HasPrefix(peek, "M  ") || strings.HasPrefix(peek, "A  ") ||
		strings.HasPrefix(peek, "?? ") || strings.Contains(peek, "nothing to commit"):
		return filterGitStatus(s)
	case looksLikeGrep(peek):
		return filterGrep(s)
	case looksLikePathList(peek):
		return filterPathList(s)
	default:
		return filterSmartTruncate(s)
	}
}

func looksLikeGrep(peek string) bool {
	// path:line:text — at least two lines matching the shape
	lines := strings.Split(strings.TrimRight(peek, "\n"), "\n")
	hit := 0
	for _, ln := range lines {
		if grepLineRe.MatchString(ln) {
			hit++
			if hit >= 2 {
				return true
			}
		}
	}
	return false
}

var grepLineRe = regexp.MustCompile(`^[^:\s]{1,200}:\d+:`)

func looksLikePathList(peek string) bool {
	lines := strings.Split(strings.TrimRight(peek, "\n"), "\n")
	if len(lines) < 4 {
		return false
	}
	hit := 0
	for _, ln := range lines {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		if strings.Contains(ln, " ") || strings.Contains(ln, "\t") {
			continue
		}
		if strings.Count(ln, "/") >= 1 || strings.HasSuffix(ln, "/") || strings.Contains(ln, ".") {
			hit++
		}
	}
	return hit*2 >= len(lines)
}

// filterGitDiff → per-file stats + small context, hunks dropped.
func filterGitDiff(s string) string {
	var b strings.Builder
	b.WriteString("[rtk:git-diff compressed — hunks dropped, file changes kept]\n")
	fileRe := regexp.MustCompile(`diff --git a/(\S+) b/(\S+)`)
	addRe := regexp.MustCompile(`^\+[^+]`)
	delRe := regexp.MustCompile(`^-[^-]`)
	binRe := regexp.MustCompile(`^Binary files`)
	var cur string
	var add, del, files int
	flush := func() {
		if cur != "" {
			fmt.Fprintf(&b, "  %s  +%d -%d\n", cur, add, del)
			files++
		}
	}
	for _, ln := range strings.Split(s, "\n") {
		if m := fileRe.FindStringSubmatch(ln); m != nil {
			flush()
			cur = m[2]
			add, del = 0, 0
			continue
		}
		if binRe.MatchString(ln) {
			flush()
			cur = ""
			b.WriteString("  [binary]\n")
			files++
			continue
		}
		if addRe.MatchString(ln) {
			add++
		} else if delRe.MatchString(ln) {
			del++
		}
	}
	flush()
	fmt.Fprintf(&b, "files changed: %d\n", files)
	return b.String()
}

// filterGitStatus collapses the long/short status listing.
func filterGitStatus(s string) string {
	var b strings.Builder
	b.WriteString("[rtk:git-status compressed]\n")
	counts := map[string]int{}
	for _, ln := range strings.Split(s, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		code := "other"
		if len(ln) >= 2 {
			code = strings.TrimSpace(ln[:2])
		}
		counts[code]++
	}
	for k, v := range counts {
		fmt.Fprintf(&b, "%s: %d file(s)\n", k, v)
	}
	return b.String()
}

// filterGrep dedups and caps the match list.
func filterGrep(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	seen := map[string]int{}
	var out []string
	perFile := map[string]int{}
	for _, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		key := ln
		seen[key]++
		if seen[key] > 1 {
			continue
		}
		if m := grepLineRe.FindStringSubmatchIndex(ln); m != nil {
			file := ln[:strings.Index(ln, ":")]
			if perFile[file] >= 12 { // cap: 12 matches per file
				perFile[file]++
				continue
			}
			perFile[file]++
		}
		out = append(out, ln)
		if len(out) >= 120 { // global cap
			out = append(out, "[rtk:grep — more matches truncated]")
			break
		}
	}
	var b strings.Builder
	b.WriteString("[rtk:grep compressed — dedup + caps]\n")
	b.WriteString(strings.Join(out, "\n"))
	return b.String()
}

// filterPathList collapses find/ls/tree listings.
func filterPathList(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	files, dirs := 0, 0
	var sample []string
	for _, ln := range lines {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		if strings.HasSuffix(ln, "/") || !strings.Contains(ln[1:], ".") {
			dirs++
		} else {
			files++
		}
		if len(sample) < 40 {
			sample = append(sample, ln)
		}
	}
	var b strings.Builder
	b.WriteString("[rtk:file-list compressed]\n")
	fmt.Fprintf(&b, "entries: %d (%d files, %d dirs) — first entries:\n", files+dirs, files, dirs)
	b.WriteString(strings.Join(sample, "\n"))
	if files+dirs > len(sample) {
		b.WriteString("\n…")
	}
	return b.String()
}

// filterDedupLog collapses repeated lines (used by dedup-log detection in
// future versions; smart-truncate catches log dumps today).
func filterDedupLog(s string) string {
	lines := strings.Split(s, "\n")
	var out []string
	var prev string
	var n int
	flush := func() {
		if n > 0 {
			if n > 1 {
				out = append(out, prev+"  ×"+strconv.Itoa(n))
			} else {
				out = append(out, prev)
			}
		}
	}
	for _, ln := range lines {
		if ln == prev {
			n++
			continue
		}
		flush()
		prev, n = ln, 1
	}
	flush()
	return strings.Join(out, "\n")
}

// filterSmartTruncate keeps head+tail with an elision marker for generic
// oversized payloads (logs, dumps, big reads).
func filterSmartTruncate(s string) string {
	if len(s) <= rtkMaxHead+rtkMaxTail+120 {
		// small enough to try dedup-log which may still shrink logs
		out := filterDedupLog(s)
		if len(out) < len(s) {
			return "[rtk:dedup-log]\n" + out
		}
		return s
	}
	head := s[:rtkMaxHead]
	if i := strings.LastIndexByte(head, '\n'); i > 0 {
		head = head[:i]
	}
	tail := s[len(s)-rtkMaxTail:]
	if i := strings.IndexByte(tail, '\n'); i >= 0 && i < len(tail)-1 {
		tail = tail[i+1:]
	}
	return "[rtk:smart-truncate — middle elided]\n" + head +
		"\n…[" + strconv.Itoa(len(s)-len(head)-len(tail)) + " bytes elided]…\n" + tail
}

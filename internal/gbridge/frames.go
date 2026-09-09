// Incremental parser for Google's length-prefixed streaming frames.
//
// The StreamGenerate endpoint answers with a chunked body shaped like:
//
//	)]}'\n              <- anti-XSSI prefix (once, at the very start)
//	\n177\n<fragment>\n <- frame: <length marker>\n<json fragment>
//	\n1394\n<fragment>\n
//	...
//
// Empirically (verified live against two independent capture sessions) the
// marker counts the fragment's UTF-16 units PLUS TWO extra units — slicing
// exactly `marker` bytes yields the JSON + "\n" + the first digit of the
// next marker, and json.Unmarshal fails with "extra data". The canonical
// "length = units" assumption does NOT hold for the current upstream.
//
// Therefore this parser locates frame boundaries by the structural marker
// pattern: a fragment ends at the first raw "\n" that is followed by
// digits and another "\n" (the next length marker). Raw newlines never
// appear inside JSON fragments (strings escape them), so the pattern is
// unambiguous. The numeric marker is kept only as a minimum-length hint.
//
// The parser keeps partial state between network chunks and only returns
// COMPLETE frames — no rescanning of the whole buffer per chunk.

package gbridge

import (
	"encoding/json"
	"fmt"
	"sync"
)

type frameParser struct {
	mu        sync.Mutex
	buf       []byte
	prefixCut bool      // )]}' stripped?
	inFrag    bool      // marker consumed, currently collecting a fragment
	fragStart int       // fragment start index within buf
	markerLen int       // marker value (hint, UTF-16 units + 2)
}

func newFrameParser() *frameParser { return &frameParser{} }

// feed appends raw bytes and returns every frame that became complete.
func (p *frameParser) feed(data []byte) ([]json.RawMessage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.buf = append(p.buf, data...)
	return p.drain(false)
}

// flush finishes the stream (EOF): any pending fragment is parsed as-is.
func (p *frameParser) flush() ([]json.RawMessage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.drain(true)
}

func (p *frameParser) drain(eof bool) ([]json.RawMessage, error) {
	var frames []json.RawMessage

	for {
		// 1) Strip the anti-XSSI prefix once.
		if !p.prefixCut {
			if len(p.buf) < len(prefix) {
				if len(p.buf) == 0 || stringIsPrefix(prefix, p.buf) {
					return frames, nil // wait for more bytes
				}
			}
			if stringIsPrefix(prefix, p.buf) && len(p.buf) >= len(prefix) {
				p.buf = p.buf[len(prefix):]
			} else if len(p.buf) >= len(prefix) && !stringIsPrefix(prefix, p.buf) {
				// body does not carry the prefix — proceed
			} else {
				return frames, nil
			}
			p.prefixCut = true
		}

		// 2) Marker line, unless we are mid-fragment.
		if !p.inFrag {
			// Skip whitespace between frames.
			i := 0
			for i < len(p.buf) && (p.buf[i] == '\n' || p.buf[i] == '\r' || p.buf[i] == ' ' || p.buf[i] == '\t') {
				i++
			}
			p.buf = p.buf[i:]
			if len(p.buf) == 0 {
				return frames, nil
			}
			// digits up to '\n'
			digitsEnd := -1
			for j := 0; j < len(p.buf); j++ {
				c := p.buf[j]
				if c >= '0' && c <= '9' {
					continue
				}
				if c == '\n' {
					digitsEnd = j
					break
				}
				// malformed gap — drop one byte and resync
				p.buf = p.buf[1:]
				digitsEnd = -2
				break
			}
			if digitsEnd == -2 {
				continue // rescan after dropping the bad byte
			}
			if digitsEnd < 0 {
				return frames, nil // digits not terminated yet — wait
			}
			length := 0
			for _, d := range p.buf[:digitsEnd] {
				length = length*10 + int(d-'0')
				if length > 1<<30 {
					return frames, fmt.Errorf("frame length overflow")
				}
			}
			p.inFrag = true
			p.fragStart = digitsEnd + 1
			p.markerLen = length
			_ = length
		}

		// 3) Fragment collection: find the next structural boundary
		//    ('\n' + digits + '\n') or, at EOF, take the whole remainder.
		rel := scanFragmentEnd(p.buf[p.fragStart:])
		if rel < 0 {
			if eof {
				// final frame without a trailing marker
				frag := trimTrailingWS(p.buf[p.fragStart:])
				p.buf = nil
				p.inFrag = false
				if len(frag) > 0 {
					var parsed json.RawMessage
					if err := json.Unmarshal(frag, &parsed); err == nil {
						frames = append(frames, parsed)
					}
				}
				return frames, nil
			}
			return frames, nil // wait for more bytes
		}

		fragEnd := p.fragStart + rel
		frag := p.buf[p.fragStart:fragEnd]
		rest := p.buf[fragEnd+1:] // skip the boundary '\n'

		var parsed json.RawMessage
		if err := json.Unmarshal(frag, &parsed); err != nil {
			trimmed := trimTrailingWS(frag)
			if err2 := json.Unmarshal(trimmed, &parsed); err2 != nil {
				// Unrecoverable fragment — skip and resync at next marker.
				p.buf = rest
				p.inFrag = false
				continue
			}
		}
		frames = append(frames, parsed)
		p.buf = rest
		p.inFrag = false
	}
}

// scanFragmentEnd returns the index of the '\n' that terminates the current
// fragment: the first raw '\n' followed by 1..12 digits and another '\n'.
// Returns -1 while the fragment is still streaming in.
func scanFragmentEnd(data []byte) int {
	for i := 0; i < len(data); i++ {
		if data[i] != '\n' {
			continue
		}
		j := i + 1
		digits := 0
		for j < len(data) && data[j] >= '0' && data[j] <= '9' && digits <= 12 {
			j++
			digits++
		}
		if digits > 0 && digits <= 12 && j < len(data) && data[j] == '\n' {
			return i
		}
	}
	return -1
}

const prefix = ")]}'"

// stringIsPrefix reports whether b is a (partial) prefix of the XSSI marker.
func stringIsPrefix(prefix string, b []byte) bool {
	if len(b) > len(prefix) {
		return string(b[:len(prefix)]) == prefix
	}
	return string(prefix[:len(b)]) == string(b)
}

func trimTrailingWS(b []byte) []byte {
	end := len(b)
	for end > 0 && (b[end-1] == '\n' || b[end-1] == '\r' || b[end-1] == ' ' || b[end-1] == '\t') {
		end--
	}
	return b[:end]
}

package orchestrator

import (
	"strings"
	"unicode/utf8"
)

// replyStream extracts the top-level "userMessage" string value from a JSON
// object as it arrives in arbitrary chunks. It is a byte-level state machine:
// each input byte is processed once, so Feed is O(len(chunk)).
//
// Text that could still change (incomplete escapes, a pending high surrogate,
// an incomplete UTF-8 rune) is held back, so Text never contains a partial
// escape or an invalid rune.
type replyStream struct {
	out    strings.Builder // decoded userMessage value so far
	keyBuf strings.Builder // decoded current top-level key

	started    bool // first '{' seen
	ended      bool // top-level object closed
	done       bool // userMessage value finished (or is not a string)
	depth      int  // nesting depth; top-level object is depth 1
	expectKey  bool // at depth 1, next string is a key
	matched    bool // current top-level key is "userMessage"
	afterColon bool // at depth 1, after ':' following a matched key

	role  int    // role of the string currently being read
	inStr bool   // inside a string literal
	esc   bool   // previous byte inside string was a backslash
	uni   bool   // collecting the 4 hex digits of \uXXXX
	uN    int    // hex digits collected so far
	uVal  rune   // hex value collected so far
	surr  rune   // pending high surrogate, 0 if none
	pend  []byte // incomplete raw UTF-8 sequence
}

const (
	roleNone = iota
	roleKey
	roleValue
)

// Feed appends chunk and returns the cumulative decoded userMessage text and
// whether it changed since the previous call.
func (r *replyStream) Feed(chunk string) (text string, changed bool) {
	before := r.out.Len()
	for i := 0; i < len(chunk) && !r.done; i++ {
		r.step(chunk[i])
	}
	return r.out.String(), r.out.Len() != before
}

// Text returns the cumulative decoded text so far.
func (r *replyStream) Text() string {
	return r.out.String()
}

// Done reports whether the closing quote of the userMessage value has been seen,
// or the value is not a string.
func (r *replyStream) Done() bool {
	return r.done
}

// Reset clears all state.
func (r *replyStream) Reset() {
	*r = replyStream{}
}

func (r *replyStream) step(c byte) {
	if !r.started {
		// Ignore fences, whitespace and prose until the first '{'.
		if c == '{' {
			r.started = true
			r.depth = 1
			r.expectKey = true
		}
		return
	}
	if r.ended {
		return
	}
	if r.inStr {
		r.strStep(c)
		return
	}
	switch c {
	case '"':
		r.inStr = true
		r.esc = false
		switch {
		case r.depth == 1 && r.expectKey:
			r.role = roleKey
			r.keyBuf.Reset()
			r.expectKey = false
		case r.depth == 1 && r.matched && r.afterColon:
			r.role = roleValue
			r.afterColon = false
		default:
			r.role = roleNone
		}
	case '{', '[':
		if r.depth == 1 && r.matched && r.afterColon {
			r.done = true // userMessage is not a string
			return
		}
		r.depth++
		r.expectKey = false
	case '}', ']':
		r.depth--
		if r.depth == 0 {
			r.ended = true
		}
	case ':':
		if r.depth == 1 && r.matched {
			r.afterColon = true
		}
	case ',':
		if r.depth == 1 {
			r.expectKey = true
			r.matched = false
			r.afterColon = false
		}
	case ' ', '\t', '\r', '\n':
	default:
		// Literal (null, number, true, false) as userMessage value.
		if r.depth == 1 && r.matched && r.afterColon {
			r.done = true
		}
	}
}

func (r *replyStream) strStep(c byte) {
	if r.uni {
		r.uVal = r.uVal<<4 | hexVal(c)
		r.uN++
		if r.uN == 4 {
			r.uni = false
			r.unicodeDone(r.uVal)
		}
		return
	}
	if r.esc {
		r.esc = false
		if c == 'u' {
			r.uni = true
			r.uN = 0
			r.uVal = 0
			return
		}
		r.flushSurr()
		switch c {
		case 'b':
			r.emit('\b')
		case 'f':
			r.emit('\f')
		case 'n':
			r.emit('\n')
		case 'r':
			r.emit('\r')
		case 't':
			r.emit('\t')
		default: // \" \\ \/
			r.emit(rune(c))
		}
		return
	}
	switch {
	case c == '\\':
		r.flushPend(true)
		r.esc = true
	case c == '"':
		r.flushPend(true)
		r.flushSurr()
		r.inStr = false
		r.endString()
	case c < utf8.RuneSelf:
		r.flushPend(true)
		r.flushSurr()
		r.emit(rune(c))
	default:
		r.flushSurr()
		r.pend = append(r.pend, c)
		r.flushPend(false)
	}
}

// unicodeDone handles a completed \uXXXX escape, pairing surrogates.
func (r *replyStream) unicodeDone(v rune) {
	if r.surr != 0 {
		if v >= 0xDC00 && v <= 0xDFFF {
			r.emit((((r.surr - 0xD800) << 10) | (v - 0xDC00)) + 0x10000)
			r.surr = 0
			return
		}
		r.flushSurr()
	}
	switch {
	case v >= 0xD800 && v <= 0xDBFF:
		r.surr = v
	case v >= 0xDC00 && v <= 0xDFFF:
		r.emit(utf8.RuneError)
	default:
		r.emit(v)
	}
}

// flushSurr emits U+FFFD for a high surrogate that never got its pair.
func (r *replyStream) flushSurr() {
	if r.surr != 0 {
		r.emit(utf8.RuneError)
		r.surr = 0
	}
}

// flushPend emits complete UTF-8 runes from pend. If force is false, an
// incomplete trailing sequence is kept for the next chunk.
func (r *replyStream) flushPend(force bool) {
	for len(r.pend) > 0 {
		if !force && !utf8.FullRune(r.pend) {
			break
		}
		ru, w := utf8.DecodeRune(r.pend)
		r.emit(ru)
		r.pend = r.pend[w:]
	}
	if len(r.pend) == 0 {
		r.pend = nil
	}
}

func (r *replyStream) endString() {
	switch r.role {
	case roleKey:
		r.matched = r.keyBuf.String() == "userMessage"
		r.expectKey = false
	case roleValue:
		r.done = true
	}
	r.role = roleNone
}

func (r *replyStream) emit(ru rune) {
	switch r.role {
	case roleKey:
		r.keyBuf.WriteRune(ru)
	case roleValue:
		r.out.WriteRune(ru)
	}
}

func hexVal(c byte) rune {
	switch {
	case c >= '0' && c <= '9':
		return rune(c - '0')
	case c >= 'a' && c <= 'f':
		return rune(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return rune(c-'A') + 10
	}
	return 0
}

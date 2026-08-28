package rewrite

import "bytes"

// urlNamedRefs is every named character reference that decodes to a character
// an origin is built from. It is deliberately this short: html.UnescapeString
// on a whole attribute value would also decode the legacy no-semicolon forms,
// so a query string carrying "&copy=1" would come back as "©=1" — a corrupted
// link on a page that had nothing wrong with it. Numeric references have no
// such ambiguity and are decoded in full below.
var urlNamedRefs = map[string]byte{
	"sol":     '/',
	"colon":   ':',
	"period":  '.',
	"quest":   '?',
	"num":     '#',
	"percnt":  '%',
	"lowbar":  '_',
	"hyphen":  '-',
	"commat":  '@',
	"NewLine": '\n',
}

// decodeURLRefs replaces the character references in v that could form part of
// an origin, leaving every other byte exactly where it was. It returns v itself
// and false when there is nothing to decode, which is the ordinary case.
//
// Position-preserving matters: this feeds a re-match whose only job is to catch
// an origin the raw scan could not see, so everything it does not understand
// has to survive untouched.
func decodeURLRefs(v []byte) ([]byte, bool) {
	if bytes.IndexByte(v, '&') < 0 {
		return v, false
	}
	var out []byte
	prev := 0
	for i := 0; i < len(v); i++ {
		if v[i] != '&' {
			continue
		}
		c, n := parseURLRef(v[i:])
		if n == 0 {
			continue
		}
		out = append(out, v[prev:i]...)
		out = append(out, c)
		prev = i + n
		i = prev - 1
	}
	if out == nil {
		return v, false
	}
	return append(out, v[prev:]...), true
}

// parseURLRef decodes one reference at the start of b, returning the byte and
// how much of b it spans. n == 0 means "not a reference this cares about".
func parseURLRef(b []byte) (byte, int) {
	if len(b) < 3 || b[0] != '&' {
		return 0, 0
	}
	if b[1] != '#' {
		// Named. Bounded lookahead: the longest name here is 7 bytes.
		lim := min(len(b), 12)
		end := bytes.IndexByte(b[1:lim], ';')
		if end < 0 {
			return 0, 0
		}
		if c, ok := urlNamedRefs[string(b[1:1+end])]; ok {
			return c, end + 2
		}
		return 0, 0
	}

	j, base := 2, 10
	if j < len(b) && (b[j] == 'x' || b[j] == 'X') {
		base, j = 16, j+1
	}
	start := j
	val := 0
	for j < len(b) {
		d, ok := digitVal(b[j], base)
		if !ok {
			break
		}
		val = val*base + d
		if val > 0x10FFFF {
			return 0, 0 // out of range; leave it alone rather than guess
		}
		j++
	}
	if j == start {
		return 0, 0
	}
	if j < len(b) && b[j] == ';' {
		j++ // browsers accept a numeric reference without one
	}
	// Printable ASCII only. '&' is excluded because decoding it could splice a
	// new reference together out of unrelated neighbouring text.
	if val < 0x21 || val > 0x7e || val == '&' {
		return 0, 0
	}
	return byte(val), j
}

func digitVal(c byte, base int) (int, bool) {
	var v int
	switch {
	case c >= '0' && c <= '9':
		v = int(c - '0')
	case base == 16 && c >= 'a' && c <= 'f':
		v = int(c-'a') + 10
	case base == 16 && c >= 'A' && c <= 'F':
		v = int(c-'A') + 10
	default:
		return 0, false
	}
	return v, true
}

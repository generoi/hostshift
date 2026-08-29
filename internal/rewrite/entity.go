package rewrite

import "bytes"

// urlNamedRefs is every named character reference that decodes to a character
// an origin is built from. It is deliberately this short: html.UnescapeString
// on a whole attribute value would also decode the legacy no-semicolon forms,
// so a query string carrying "&copy=1" would come back as "©=1" — a corrupted
// link on a page that had nothing wrong with it. Numeric references have no
// such ambiguity and are decoded in full below.
var urlNamedRefs = map[string]byte{
	"sol":    '/',
	"colon":  ':',
	"period": '.',
	"quest":  '?',
	"num":    '#',
	"percnt": '%',
	"lowbar": '_',
	"hyphen": '-',
	"commat": '@',
}

// NewLine is deliberately absent. It decodes to a raw 0x0A, which no origin
// contains and which the printable-ASCII guard below would reject anyway — but
// the named table is consulted first, so listing it here was a way past that
// guard and into a spliced attribute value.

// decodeURLRefs replaces the character references in v that could form part of
// an origin, leaving every other byte exactly where it was. It returns v itself
// and false when there is nothing to decode, which is the ordinary case.
//
// Position-preserving matters: this feeds a re-match whose only job is to catch
// an origin the raw scan could not see, so everything it does not understand
// has to survive untouched.
// fusesWithPending reports whether appending a decoded byte to out would
// complete a character reference out of a fragment the decoder did not consume.
//
// Excluding the structural characters stops this decoder *emitting* a '<'. It
// does not stop it emitting a digit that fuses with an adjacent, unconsumed
// fragment into a new complete reference: "&#6" is not a reference this decoder
// accepts (6 is below the printable range), so it passes through literally —
// and then decode("&#48;") appends '0', and the next literal byte is ';', and
// together they spell "&#60;".
//
// In an href that is only a '<' inside a URL. In an attribute the browser
// decodes a *second* time — srcdoc — it is a real one, and the same
// alert(1) came back through:
//
//	srcdoc="&#6&#48;;script&#6&#50;;alert(1)…"  ->  "<script>alert(1)…"
//
// A fixed point does not catch this: the second decode is refused by the
// structural guard, so the value looks stable. Adjacency is the actual
// condition, so test that.
func fusesWithPending(out []byte, c byte) bool {
	// ';' and '#' matter as much as the body characters, and guarding only the
	// body is how this shipped broken twice. A reference is '&', an optional
	// '#' (and 'x' for hex), a body, and a ';' — so *every* one of those four
	// positions can be the byte this decoder supplies to complete a fragment
	// the decoder itself refused:
	//
	//	"&#60" is refused (it would be '<') and passes through literally,
	//	then "&#59;" decodes to ';', and together they spell "&#60;".
	//
	// "&lt" + ";" and "&#x3c" + ";" are the same shape with a named and a hex
	// fragment. Harmless in an href; a real '<' in srcdoc, which the browser
	// decodes a second time.
	if c != ';' && c != '#' && !isRefBody(c) {
		return false
	}
	i := len(out)
	for i > 0 && isRefBody(out[i-1]) {
		i--
	}
	// The 'x' of a hex reference, then the '#', in that order.
	if i > 0 && (out[i-1] == 'x' || out[i-1] == 'X') {
		i--
	}
	if i > 0 && out[i-1] == '#' {
		i--
	}
	return i > 0 && out[i-1] == '&'
}

func isRefBody(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func decodeURLRefs(v []byte) ([]byte, bool) {
	return decodeURLRefsOnce(v)
}

func decodeURLRefsOnce(v []byte) ([]byte, bool) {
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
		// Decline the whole value rather than complete a reference out of a
		// fragment we did not consume — see fusesWithPending.
		if fusesWithPending(out, c) {
			return v, false
		}
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
	// Printable ASCII only, and never a character that is structural in the
	// markup this value sits in.
	//
	// '&' is excluded because decoding it could splice a new reference together
	// out of unrelated neighbouring text. The rest are excluded because the
	// decoded text is spliced back *between the attribute's quotes* without
	// being re-encoded, so decoding a quote, an angle bracket, '=' or '/' ends
	// the attribute — or the tag — and everything after it becomes markup.
	// href="…&#34;&#62;&#60;script&#62;alert(1)&#60;/script&#62;" came out as a
	// real <script> element: content that is inert on production, made to
	// execute on the developer's variant host, which is where their admin
	// session lives.
	//
	// Nothing legitimate is lost. This exists to catch an origin hidden behind
	// character references, and none of these can appear in one.
	switch {
	case val < 0x21 || val > 0x7e:
		return 0, 0
	case val == '&' || val == '"' || val == '\'' || val == '<' || val == '>' || val == '=' || val == '`':
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

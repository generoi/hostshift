package rewrite

import (
	"bytes"
	"strconv"
	"unicode/utf8"
)

// RepairSerialized rewrites a PHP-serialized payload and re-emits its length
// prefixes, so the result still parses.
//
// `s:LEN:"DATA";` counts DATA in bytes, so any rewrite that changes a byte
// count leaves LEN stale and PHP refuses the whole structure — `false` where an
// options array should be, into a shared database, with no undo and no error.
//
// # Why this parses the grammar instead of matching a header
//
// Three earlier attempts looked for `s:N:"` and decided span-by-span whether to
// believe the declared length. All three were wrong, because the question
// cannot be answered locally. A stale length lands somewhere inside its own
// data, and if the byte it lands on is a `"` followed by `;` the header looks
// perfect. Guarding on what follows does not help either: the next bytes are
// then `}` or `i:` or `s:` — which is exactly what correct serialized data
// looks like. Two bytes of context cannot separate a real boundary from a false
// one, and the version that tried destroyed a valid wp_options row.
//
// What does separate them is whether the *whole value* parses. A real span sits
// inside a structure whose arity and delimiters agree end to end; a stale one
// does not, because the bytes after the false close are the remainder of the
// string, not the container's next element. So this walks the grammar — null,
// bool, int, float, string, array, object — and repairs only a value it can
// parse completely. Anything else is declined and rewritten without repair,
// which is what the code did before repair existed and which round-trips: the
// length one direction breaks, the other restores.
//
// Both spellings are handled by the same walk. A form percent-encodes its
// delimiters, so `options.php` sends `s%3A51%3A%22`, and having only the
// literal parser guarded meant every wp-admin save took the unguarded path.
func RepairSerialized(b []byte, rw func([]byte) []byte) []byte {
	out, _ := RepairSerializedFound(b, rw)
	return out
}

// RepairSerializedFound is RepairSerialized, also reporting whether a
// serialized value was found — which is not the same question as whether
// anything changed, and callers that route on it need the first.
func RepairSerializedFound(b []byte, rw func([]byte) []byte) ([]byte, bool) {
	// Field by field. A decline used to abandon repair for the *whole buffer*,
	// so one option whose value merely begins `a:` — `a:hover{color:red}` is
	// ordinary CSS — left every other option in the same POST with a stale
	// length. `options.php` posts every option on a settings page in one body,
	// and the contaminating field need not contain a hostname at all.
	return repairField(b, rw)
}

// RepairSerializedFields is RepairSerialized for an
// application/x-www-form-urlencoded body, whose `&` really are separators.
//
// The split has to come from the caller's knowledge of the content type rather
// than from looking at the bytes. Guessing it — "no whitespace and a `=`, so
// this is a form" — classified a decoded JSON string and a multipart part as
// forms, and then cut a serialized value in half at the `&utm_medium=` inside
// an ordinary tracking URL: both halves declined and the length was re-emitted
// from neither. In a properly encoded form body a `&` inside a value is `%26`,
// so there the separator is unambiguous.
//
// Splitting matters because a decline is otherwise buffer-wide: one option
// holding `a:hover{color:red}` — ordinary CSS, no hostname — left every other
// option in the same POST with a stale length, and `options.php` posts them all
// in one body.
func RepairSerializedFields(b []byte, rw func([]byte) []byte) []byte {
	var out []byte
	found := false
	for start := 0; start <= len(b); {
		end := start + indexByteFrom(b[start:], '&')
		if end < start {
			end = len(b)
		}
		rep, ok := repairField(b[start:end], rw)
		out = append(out, rep...)
		found = found || ok
		if end < len(b) {
			out = append(out, '&')
		}
		start = end + 1
	}
	if !found {
		return rw(b)
	}
	return out
}

// indexByteFrom is bytes.IndexByte, returning -1 when absent.
func indexByteFrom(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}

// repairField repairs one `&`-delimited field.
func repairField(b []byte, rw func([]byte) []byte) ([]byte, bool) {
	var out []byte
	prev, found := 0, false
	for i := 0; i < len(b); {
		if !valueStart(b, i) {
			i++
			continue
		}
		rep, end, ok, committed := repairAt(b, i, rw)
		if !ok && !committed {
			// `https:` looks like a header until the length fails to parse.
			i++
			continue
		}
		if !ok {
			// A header-shaped candidate that does not parse means this buffer is
			// not something we understand well enough to re-emit lengths for.
			// Repairing the *pieces* of it would be worse than leaving it alone:
			// after a stale outer length, the inner spans still parse on their
			// own, and repairing those rewrites numbers that were already right
			// for the original — which is precisely how a valid row was
			// destroyed. Declining hands the whole body to rw untouched, and the
			// length one direction broke, the other restores.
			return rw(b), false
		}
		// A structurally complete parse is still not proof, and this is the last
		// place it can be caught. See fieldEnd.
		//
		// When a declared length is stale, the walk can consume a *prefix* of
		// the real string and still close every container — the false `";` ends
		// the string and the very next `}` closes the array with its arity
		// satisfied. What gives it away is what is left over: the remainder of
		// the true string, which begins with the bytes that should have been
		// inside it. Measured on an ordinary `custom_css` option, that residue
		// was `\n\n\n";}` and the row silently lost six bytes on every
		// view-and-save, parsing cleanly each time.
		//
		// So after a top-level value, skip whitespace and refuse to believe the
		// parse if what follows is string or container punctuation. A real value
		// is followed by the end of the body, a field separator, or ordinary
		// surrounding text — never by a stray `"`, `;` or `}`.
		if !occupiesItsField(b, i, end) {
			return rw(b), false
		}
		out = append(out, rw(b[prev:i])...)
		out = append(out, rep...)
		prev, i = end, end
		found = true
	}
	if !found {
		return rw(b), false
	}
	return append(out, rw(b[prev:])...), true
}

// repairAt tries each spelling at i. committed reports whether the candidate got
// past its length and opening delimiter — far enough that it really is a
// serialized header and a failure to parse means the buffer is untrustworthy.
//
// Without that distinction every `https:` in a URL was a candidate whose length
// parse failed immediately, and "a failed candidate declines the buffer" then
// declined every body containing a link.
func repairAt(b []byte, i int, rw func([]byte) []byte) (rep []byte, end int, ok, committed bool) {
	for _, syn := range []syntax{literalSyntax, percentSyntax, htmlSyntax, jsonSyntax, jsonHTMLSyntax} {
		r, e, o, c := repairValueC(b, i, rw, 0, syn)
		if o {
			return r, e, true, c
		}
		if c {
			committed = true
		}
	}
	return nil, 0, false, committed
}

// repairValueC is repairValue, also reporting whether the candidate committed.
func repairValueC(b []byte, i int, rw func([]byte) []byte, depth int, syn syntax) ([]byte, int, bool, bool) {
	// Every type that declares a length. `C` and `E` were missing, so a custom
	// or enum header whose length did not describe its data neither declined its
	// field nor raised the broken count — `C:3:"Foo":27:{…}` over a 24-byte
	// payload scored zero where the same mistake in an `s:` scored two. Worse,
	// not declining let the scan carry on at i+1 and repair spans *inside* an
	// opaque payload, which is the one thing repairCustom exists to avoid.
	if i < len(b) && (b[i] == 's' || b[i] == 'a' || b[i] == 'O' || b[i] == 'C' || b[i] == 'E') {
		if _, j, ok := readLen(b, i+1, syn); ok {
			d := byte('{')
			if b[i] != 'a' {
				d = '"'
			}
			if _, ok := syn.match(b, j, d); ok {
				r, e, o := repairValue(b, i, rw, depth, syn)
				return r, e, o, true
			}
		}
	}
	r, e, o := repairValue(b, i, rw, depth, syn)
	return r, e, o, false
}

// valueStart is a cheap gate: a serialized value begins with one of these, and
// only after something that can precede one. Without the second half the scan
// would try to parse at every byte of a large body.
func valueStart(b []byte, i int) bool {
	// The *shape* of a header, not just its first byte. Matching on the letter
	// alone made `a.test` inside an ordinary string look like the start of an
	// array, and made the word "see" a candidate — which matters because a
	// candidate that fails to parse now declines the whole buffer.
	switch b[i] {
	case 'N':
		if i+1 >= len(b) || !(b[i+1] == ';' || pctIs(b, i+1, ';')) {
			return false
		}
	case 'b', 'i', 'd', 's', 'a', 'O', 'R', 'r', 'E', 'C':
		if i+1 >= len(b) || !(b[i+1] == ':' || pctIs(b, i+1, ':')) {
			return false
		}
	default:
		return false
	}
	// And it must sit where a value can begin. Without this a URL path holding
	// `s:3:"a"` was a candidate: it commits, fails to close, and the detector
	// reported a healthy page as carrying broken serialized data.
	if i == 0 {
		return true
	}
	switch b[i-1] {
	// Whitespace and `>`, because a `<textarea>` WordPress indents and a text
	// node both put a value there. occupiesItsField still decides whether the
	// value fills its field; this gate only decides whether to look.
	case '{', ';', '"', '=', '&', ':', ',', '\'', '>', '[', ' ', '\t', '\r', '\n':
		return true
	}
	return pctIs(b, i-1, '{') || pctIs(b, i-1, ';') || pctIs(b, i-1, '"') ||
		(i >= 6 && string(b[i-6:i]) == "&quot;") ||
		(i >= 2 && b[i-2] == '\\' && b[i-1] == '"')
}

// maxSerializedDepth bounds the recursion. Exceeding it *declines* — it must
// not fall back to rewriting the data in place, which would leave that level's
// length stale, which is the corruption this whole file exists to prevent.
const maxSerializedDepth = 32

// syntax is how one spelling writes the delimiters and measures a length.
type syntax struct {
	// match reports the width of delimiter c at i, or ok=false.
	match func(b []byte, i int, c byte) (int, bool)
	// emit appends delimiter c.
	emit func(dst []byte, c byte) []byte
	// advance skips n decoded bytes from i.
	advance func(b []byte, i, n int) (int, bool)
	// unit reports how the spelling reads the bytes at i: their width in source
	// bytes, how many decoded bytes they count for, and a second count when the
	// source is ambiguous — 0 when it is not. Only the escaped spellings set it.
	unit func(b []byte, i int) (src, dec, alt int)
}

// refRun is the width of a character reference at i, or 0.
func refRun(b []byte, i int) int {
	if i >= len(b) || b[i] != '&' {
		return 0
	}
	for j := i + 1; j < len(b) && j-i <= 12; j++ {
		c := b[j]
		switch {
		case c == ';':
			if j == i+1 {
				return 0
			}
			return j - i + 1
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9', c == '#':
		default:
			return 0
		}
	}
	return 0
}

// htmlUnit reads the `esc_attr` spelling.
//
// `esc_attr` runs with $double_encode = false, so `&amp;` in an attribute is
// either an escaped `&`, counting one byte, or five literal bytes that were
// already `&amp;` in the data, counting five. Nothing local separates them.
// Charging every reference one byte was wrong. Charging only `&quot;` one and
// every other reference its literal width — which is where round 28 landed —
// is also wrong, just in the other direction, and it is why "Snellman & Co" and
// "Genero's" were served with lengths PHP refuses.
//
// So both readings are offered and the parse decides, which is what the rest of
// this file does with every boundary it cannot settle locally.
func htmlUnit(b []byte, i int) (src, dec, alt int) {
	if w := refRun(b, i); w > 0 {
		return w, 1, w
	}
	return 1, 1, 0
}

// jsonHTMLUnit reads the combined spelling. Only a bare reference is ambiguous:
// a `\` escape is unambiguously one, because json_encode writes a literal
// backslash as `\\`.
func jsonHTMLUnit(b []byte, i int) (src, dec, alt int) {
	if s, d := jsonHTMLRun(b, i); s > 0 {
		return s, d, 0
	}
	if i < len(b) && b[i] == '\\' {
		return 0, 0, 0
	}
	if w := refRun(b, i); w > 0 {
		return w, 1, w
	}
	return 1, 1, 0
}

// maxStringReadings bounds the search. Every ambiguous reference in the span
// can double the number of live readings, so a value carrying a great many of
// them is declined rather than explored — which is what the code did with all
// of them until now.
const maxStringReadings = 2048

// advanceReadings returns every offset at which exactly n decoded bytes have
// passed from i. More than one means the source is genuinely ambiguous and the
// caller must not choose.
func advanceReadings(b []byte, i, n int, unit func([]byte, int) (int, int, int)) []int {
	if n < 0 {
		return nil
	}
	type st struct{ i, n int }
	seen := map[st]bool{{i, n}: true}
	work := []st{{i, n}}
	var ends []int
	for len(work) > 0 {
		s := work[len(work)-1]
		work = work[:len(work)-1]
		if s.n == 0 {
			ends = append(ends, s.i)
			continue
		}
		if s.i >= len(b) {
			continue
		}
		src, dec, alt := unit(b, s.i)
		if src == 0 {
			continue
		}
		for _, d := range [2]int{dec, alt} {
			// A unit wider than what is left would straddle the declared end,
			// which valid data does not do: a length counts whole characters.
			if d <= 0 || d > s.n {
				continue
			}
			k := st{s.i + src, s.n - d}
			if seen[k] {
				continue
			}
			if len(seen) >= maxStringReadings {
				return nil
			}
			seen[k] = true
			work = append(work, k)
		}
	}
	return ends
}

// stringEnd finds where the data of a string of n decoded bytes ends, given
// that a `";` must follow it.
//
// The fast path is the spelling's own single reading, and for a span with no
// character reference in it that reading is the only one. Otherwise the closer
// picks: exactly one reading that closes is the answer, and two are an
// ambiguity this must not resolve by preference.
func stringEnd(b []byte, dataAt, n int, syn syntax) (dataEnd, cw, tw int, ok bool) {
	closes := func(e int) (int, int, bool) {
		cw, ok := syn.match(b, e, '"')
		if !ok {
			return 0, 0, false
		}
		tw, ok := syn.match(b, e+cw, ';')
		if !ok {
			return 0, 0, false
		}
		return cw, tw, true
	}
	if e, ok := syn.advance(b, dataAt, n); ok {
		if cw, tw, ok := closes(e); ok {
			if syn.unit == nil || bytes.IndexByte(b[dataAt:e], '&') < 0 {
				return e, cw, tw, true
			}
		}
	}
	if syn.unit == nil {
		return 0, 0, 0, false
	}
	found := 0
	for _, e := range advanceReadings(b, dataAt, n, syn.unit) {
		if c, t, ok := closes(e); ok {
			found++
			dataEnd, cw, tw = e, c, t
		}
	}
	if found != 1 {
		return 0, 0, 0, false
	}
	return dataEnd, cw, tw, true
}

var literalSyntax = syntax{
	match: func(b []byte, i int, c byte) (int, bool) {
		if i < len(b) && b[i] == c {
			return 1, true
		}
		return 0, false
	},
	emit: func(dst []byte, c byte) []byte { return append(dst, c) },
	advance: func(b []byte, i, n int) (int, bool) {
		if n < 0 || i+n > len(b) {
			return 0, false
		}
		return i + n, true
	},
}

// htmlSyntax is the spelling `esc_attr` and `esc_textarea` produce: the quotes
// become `&quot;`, everything else in the grammar stays literal.
//
// Without it the streamed HTML arm could not repair, so it served a blob whose
// length was stale while every other arm repaired — and that asymmetry is the
// whole of rounds twenty-two to twenty-six. The browser unescapes, posts back
// real quotes over a stale length, and the request direction then had to guess
// whether to believe it. With both directions repairing there is nothing to
// guess: the wire is never stale in the first place.
var htmlSyntax = syntax{
	match: func(b []byte, i int, c byte) (int, bool) {
		if c == '"' {
			for _, e := range []string{"&quot;", "&#34;", "&#034;"} {
				if i+len(e) <= len(b) && string(b[i:i+len(e)]) == e {
					return len(e), true
				}
			}
			return 0, false
		}
		if i < len(b) && b[i] == c {
			return 1, true
		}
		return 0, false
	},
	emit: func(dst []byte, c byte) []byte {
		if c == '"' {
			return append(dst, `&quot;`...)
		}
		return append(dst, c)
	},
	advance: advanceEntities,
	unit:    htmlUnit,
}

// jsonSyntax is the spelling a JSON string value carries: the quotes become
// `\"`, everything else stays literal.
var jsonSyntax = syntax{
	match: func(b []byte, i int, c byte) (int, bool) {
		if c == '"' {
			if i+1 < len(b) && b[i] == '\\' && b[i+1] == '"' {
				return 2, true
			}
			return 0, false
		}
		if i < len(b) && b[i] == c {
			return 1, true
		}
		return 0, false
	},
	emit: func(dst []byte, c byte) []byte {
		if c == '"' {
			return append(dst, '\\', '"')
		}
		return append(dst, c)
	},
	// `\uXXXX` is six source bytes decoding to one rune — one to three bytes of
	// UTF-8, four for a surrogate pair — and `wp_json_encode` writes every
	// non-ASCII character that way. So it is measured, by jsonUnicodeRun.
	//
	// It used to decline, under a comment claiming that "never writes a wrong
	// length". It does: a decline still rewrites the host, and re-emits nothing,
	// so the old length stays on a value whose byte count has changed. That is
	// every ä, ö and å in the fleet — measured on an Elementor `data-settings`
	// carrying "Läs mer", `unserialize()` returned false on the served bytes.
	advance: func(b []byte, i, n int) (int, bool) {
		if n < 0 {
			return 0, false
		}
		for n > 0 {
			if i >= len(b) {
				return 0, false
			}
			if b[i] == '\\' && i+1 < len(b) {
				if b[i+1] == 'u' {
					src, dec, ok := jsonUnicodeRun(b, i)
					// A character straddling the declared end is not something
					// valid data does: a length counts whole characters.
					if !ok || dec > n {
						return 0, false
					}
					i, n = i+src, n-dec
					continue
				}
				i, n = i+2, n-1
				continue
			}
			i, n = i+1, n-1
		}
		return i, true
	},
}

// jsonUnicodeRun measures a `\uXXXX` escape at i: how many source bytes it
// occupies, and how many bytes it decodes to, which is what a serialized length
// counts. Six source bytes for a rune in the BMP, twelve for a surrogate pair.
//
// A lone surrogate is refused rather than charged a width. It is not text, so
// no valid length was ever computed over it, and inventing one is the guess
// this file exists to avoid.
func jsonUnicodeRun(b []byte, i int) (src, dec int, ok bool) {
	r, ok := jsonHex4(b, i)
	if !ok {
		return 0, 0, false
	}
	src = 6
	switch {
	case r >= 0xD800 && r <= 0xDBFF:
		lo, ok := jsonHex4(b, i+6)
		if !ok || lo < 0xDC00 || lo > 0xDFFF {
			return 0, 0, false
		}
		r = 0x10000 + (r-0xD800)<<10 + (lo - 0xDC00)
		src = 12
	case r >= 0xDC00 && r <= 0xDFFF:
		return 0, 0, false
	}
	w := utf8.RuneLen(rune(r))
	if w < 0 {
		return 0, 0, false
	}
	return src, w, true
}

// jsonHex4 reads the four hex digits of a `\uXXXX` escape at i.
func jsonHex4(b []byte, i int) (int, bool) {
	if i+6 > len(b) || b[i] != '\\' || b[i+1] != 'u' {
		return 0, false
	}
	v := 0
	for _, c := range b[i+2 : i+6] {
		d, ok := hexDigit(c)
		if !ok {
			return 0, false
		}
		v = v<<4 | d
	}
	return v, true
}

func hexDigit(c byte) (int, bool) {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0'), true
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10, true
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10, true
	}
	return 0, false
}

// jsonHTMLSyntax is `esc_attr(wp_json_encode(…))`: the JSON escaping runs first,
// turning `"` into `\"`, and `esc_attr` then escapes that quote, giving
// `\&quot;`. Elementor's `data-settings`, WooCommerce block attributes and ACF
// all emit it.
//
// It is a spelling neither of its two halves can read, so the value was
// rewritten without repair — and, worse, the *canonical* page counted as broken
// too. The corpus diff subtracts the canonical count from the variant's, so a
// blind spot on both sides cancelled exactly and a page PHP refuses was reported
// GREEN. A false GREEN hides the class the check exists for, which is worse than
// the false RED it replaced.
var jsonHTMLSyntax = syntax{
	match: func(b []byte, i int, c byte) (int, bool) {
		if c == '"' {
			if i+7 <= len(b) && b[i] == '\\' && string(b[i+1:i+7]) == "&quot;" {
				return 7, true
			}
			return 0, false
		}
		if i < len(b) && b[i] == c {
			return 1, true
		}
		return 0, false
	},
	emit: func(dst []byte, c byte) []byte {
		if c == '"' {
			return append(dst, `\&quot;`...)
		}
		return append(dst, c)
	},
	advance: func(b []byte, i, n int) (int, bool) {
		if n < 0 {
			return 0, false
		}
		for n > 0 {
			if i >= len(b) {
				return 0, false
			}
			src, dec := jsonHTMLRun(b, i)
			if src == 0 {
				// Every backslash in this spelling opens something. One that
				// opens nothing measurable is not data we can count.
				if b[i] == '\\' {
					return 0, false
				}
				i, n = i+1, n-1
				continue
			}
			if dec > n {
				return 0, false
			}
			i, n = i+src, n-dec
		}
		return i, true
	},
	unit: jsonHTMLUnit,
}

// jsonHTMLRun measures one decoded unit of the combined spelling at i: how many
// source bytes it occupies and how many it counts toward a serialized length.
// src is zero when i is an ordinary byte, or a backslash opening nothing this
// can measure.
func jsonHTMLRun(b []byte, i int) (src, dec int) {
	if i >= len(b) || b[i] != '\\' {
		return 0, 0
	}
	if i+7 <= len(b) && string(b[i+1:i+7]) == "&quot;" {
		return 7, 1
	}
	if i+1 < len(b) && b[i+1] == 'u' {
		if s, d, ok := jsonUnicodeRun(b, i); ok {
			return s, d
		}
		return 0, 0
	}
	if i+2 <= len(b) {
		return 2, 1
	}
	return 0, 0
}

var percentSyntax = syntax{
	match: func(b []byte, i int, c byte) (int, bool) {
		if pctIs(b, i, c) {
			return 3, true
		}
		return 0, false
	},
	emit: func(dst []byte, c byte) []byte {
		const hex = "0123456789ABCDEF"
		return append(dst, '%', hex[c>>4], hex[c&0xf])
	},
	advance: advanceDecoded,
}

// repairValue parses one serialized value at i and returns its repaired bytes
// and the offset one past it. ok is false if it does not parse completely.
func repairValue(b []byte, i int, rw func([]byte) []byte, depth int, syn syntax) ([]byte, int, bool) {
	if depth > maxSerializedDepth || i >= len(b) {
		return nil, 0, false
	}
	switch b[i] {
	case 'N':
		if w, ok := syn.match(b, i+1, ';'); ok {
			return b[i : i+1+w], i + 1 + w, true
		}
		return nil, 0, false

	case 'R', 'r':
		// References and repeated object instances. `serialize()` emits these
		// routinely, and having no case for them made an array holding one fail
		// to parse — which cost the repair for that whole field.
		j, ok := scanScalar(b, i, syn)
		if !ok {
			return nil, 0, false
		}
		return b[i:j], j, true

	case 'E':
		// Enums, PHP 8.1+: E:len:"Enum:Case"; — length-prefixed like a string,
		// but the payload is an identifier, never a URL, so it is copied
		// verbatim rather than rewritten.
		return repairOpaqueString(b, i, syn)

	case 'b', 'i', 'd':
		// Each scalar matched against its real grammar. Accepting "anything up
		// to a `;`" made `background:url(https:&#47;` parse as a float — the
		// `;` of the character reference terminated it — so an ordinary style
		// attribute was treated as serialized data.
		j, ok := scanScalar(b, i, syn)
		if !ok {
			return nil, 0, false
		}
		return b[i:j], j, true

	case 's':
		return repairString(b, i, rw, depth, syn)

	case 'a':
		return repairArray(b, i, rw, depth, syn)

	case 'C':
		// Custom serialization: C:len:"Class":datalen:{opaque}.
		return repairCustom(b, i, rw, syn)

	case 'O':
		return repairObject(b, i, rw, depth, syn)
	}
	return nil, 0, false
}

// scanScalar matches `b:0;`, `i:-12;` or `d:1.5E+3;` — and nothing else.
func scanScalar(b []byte, i int, syn syntax) (int, bool) {
	kind := b[i]
	w, ok := syn.match(b, i+1, ':')
	if !ok {
		return 0, false
	}
	j := i + 1 + w
	start := j
	switch kind {
	case 'b':
		if j < len(b) && (b[j] == '0' || b[j] == '1') {
			j++
		}
	case 'R', 'r', 'i':
		if j < len(b) && (b[j] == '-' || b[j] == '+') {
			j++
		}
		for j < len(b) && b[j] >= '0' && b[j] <= '9' {
			j++
		}
	case 'd':
		// Digits, sign, decimal point, exponent — plus PHP's INF/-INF/NAN.
		if j+2 < len(b) && (string(b[j:j+3]) == "INF" || string(b[j:j+3]) == "NAN") {
			j += 3
		} else {
			if j < len(b) && (b[j] == '-' || b[j] == '+') {
				j++
			}
			if j+3 < len(b) && string(b[j:j+3]) == "INF" {
				j += 3
			} else {
				for j < len(b) && (b[j] >= '0' && b[j] <= '9' ||
					b[j] == '.' || b[j] == 'e' || b[j] == 'E' ||
					b[j] == '-' || b[j] == '+') {
					j++
				}
			}
		}
	}
	if j == start {
		return 0, false
	}
	tw, ok := syn.match(b, j, ';')
	if !ok {
		return 0, false
	}
	return j + tw, true
}

// readLen reads `:<digits>:` at i, returning the number and the offset after it.
func readLen(b []byte, i int, syn syntax) (int, int, bool) {
	w, ok := syn.match(b, i, ':')
	if !ok {
		return 0, 0, false
	}
	j := i + w
	start := j
	for j < len(b) && b[j] >= '0' && b[j] <= '9' {
		j++
	}
	if j == start || j-start > 18 {
		return 0, 0, false
	}
	n, err := strconv.Atoi(string(b[start:j]))
	// A declared length larger than the buffer cannot be honest, and using one
	// as an offset overflowed to a negative index and panicked.
	if err != nil || n < 0 || n > len(b) {
		return 0, 0, false
	}
	w, ok = syn.match(b, j, ':')
	if !ok {
		return 0, 0, false
	}
	return n, j + w, true
}

// repairNested repairs a serialized payload sitting inside a string's data, at
// whatever offset it starts. decline reports that the data holds something
// serialized which could not be accounted for, and that the enclosing string
// must therefore not re-emit its own length either.
//
// The single parse at offset zero this replaces only ever saw a payload written
// tight against the opening quote. One leading newline — an option edited in a
// textarea, an ACF field with an indented default, a line of prose introducing
// a blob — and that parse failed, control fell through to rewriting the data in
// place, and the *inner* length was left stale while the outer was faithfully
// re-emitted from the new bytes. Nothing could see it: the outer parses, so the
// detector reads its length, skips to the end of the string and never looks in.
// A 4000-case fuzz put it at 37 invisible regressions.
//
// It is the walk `repairField` runs over a buffer, with the spelling and depth
// fixed by the string that encloses it, and it inherits both of that walk's
// refusals. A header that commits and does not parse declines. So does a value
// that does not fill the data it sits in — which is what separates a payload
// merely indented inside its string, repaired, from one introduced by prose or
// wrapped in markup, declined, where the bytes around it are unconstrained and
// a false boundary cannot be told from a real one.
func repairNested(b []byte, rw func([]byte) []byte, depth int, syn syntax) (rep []byte, ok, decline bool) {
	var out []byte
	prev, found := 0, false
	for i := 0; i < len(b); {
		if !valueStart(b, i) {
			i++
			continue
		}
		r, end, parsed, committed := repairValueC(b, i, rw, depth, syn)
		if !committed {
			// Only a value with a length prefix has anything at stake here. A
			// bare scalar cannot hold a host and has no number to re-emit, so
			// finding one mid-string says nothing about the rest — and treating
			// it as a payload declined `s:12:"N;not really"`, an ordinary
			// twelve-byte string, taking the whole option down with it.
			i++
			continue
		}
		if !parsed || !occupiesItsField(b, i, end) {
			return nil, false, true
		}
		out = append(out, rw(b[prev:i])...)
		out = append(out, r...)
		prev, i = end, end
		found = true
	}
	if !found {
		return nil, false, false
	}
	return append(out, rw(b[prev:])...), true, false
}

func repairString(b []byte, i int, rw func([]byte) []byte, depth int, syn syntax) ([]byte, int, bool) {
	n, j, ok := readLen(b, i+1, syn)
	if !ok {
		return nil, 0, false
	}
	qw, ok := syn.match(b, j, '"')
	if !ok {
		return nil, 0, false
	}
	dataAt := j + qw
	dataEnd, cw, tw, ok := stringEnd(b, dataAt, n, syn)
	if !ok {
		return nil, 0, false
	}
	end := dataEnd + cw + tw

	data := b[dataAt:dataEnd]
	// A string can hold another serialized payload. Look for it *as input*,
	// where its lengths are still right — at whatever offset it starts.
	var repaired []byte
	inner, iok, idecline := repairNested(data, rw, depth+1, syn)
	switch {
	case iok:
		repaired = inner
	case idecline:
		// The nested walk found a serialized header it could not account for.
		// Re-emitting only the outer length is the worst outcome available: the
		// outer parses, so nothing errors, and the failure surfaces on a later
		// unserialize of the inner value — silently, because a detector that
		// reads the outer length and skips to the end of the string never looks
		// inside.
		//
		// Shape alone is not enough to decide this. Testing `valueStart` instead
		// declined on `a:hover{color:red}`, `d:\\shares\\logo.png`, `i:12345`
		// and `O:brien` — ordinary strings that begin with two bytes that happen
		// to look like a header. What decides it is the same pair of questions
		// the top-level walk asks: did the value parse, and did it account for
		// the field it sits in.
		//
		// Declining the whole span leaves both lengths alone, so the rewrite the
		// response direction made without repair is undone exactly — and the
		// outer length is then stale on the wire, which is what makes it
		// visible.
		return nil, 0, false
	default:
		repaired = rw(data)
	}

	// Unchanged data keeps the original bytes. Re-emitting unconditionally
	// changed them under an identity map, which test 24 forbids: strconv.Itoa
	// drops the leading zero from `s:05:` and the percent emitter forces
	// uppercase hex, so a client encoding in lowercase had its body altered by a
	// proxy asked to be a no-op.
	if string(repaired) == string(data) {
		return b[i:end], end, true
	}
	// The new length is the declared one plus the change in source bytes, not a
	// fresh measurement of the result.
	//
	// Measuring again asks dlen to reproduce whatever reading the parse settled
	// on, and where a reference was ambiguous it cannot: the walk may have read
	// `&amp;` as one byte and dlen would count five, putting a wrong number on a
	// value that parsed perfectly. The delta needs no reading at all. A rewrite
	// substitutes hostnames, which are plain in every spelling here, so a byte
	// added to the source is a byte added to the data.
	var out []byte
	out = append(out, 's')
	out = syn.emit(out, ':')
	out = append(out, strconv.Itoa(n+len(repaired)-len(data))...)
	out = syn.emit(out, ':')
	out = syn.emit(out, '"')
	out = append(out, repaired...)
	out = syn.emit(out, '"')
	out = syn.emit(out, ';')
	return out, end, true
}

func repairArray(b []byte, i int, rw func([]byte) []byte, depth int, syn syntax) ([]byte, int, bool) {
	n, j, ok := readLen(b, i+1, syn)
	if !ok {
		return nil, 0, false
	}
	ow, ok := syn.match(b, j, '{')
	if !ok {
		return nil, 0, false
	}
	at := j + ow
	var body []byte
	// n key-value pairs, and the arity has to be exact — that is what makes a
	// false boundary fail to parse.
	for k := 0; k < n*2; k++ {
		v, next, vok := repairValue(b, at, rw, depth+1, syn)
		if !vok {
			return nil, 0, false
		}
		body = append(body, v...)
		at = next
	}
	cw, ok := syn.match(b, at, '}')
	if !ok {
		return nil, 0, false
	}
	if string(body) == string(b[j+ow:at]) {
		return b[i : at+cw], at + cw, true
	}
	var out []byte
	out = append(out, 'a')
	out = syn.emit(out, ':')
	out = append(out, strconv.Itoa(n)...)
	out = syn.emit(out, ':')
	out = syn.emit(out, '{')
	out = append(out, body...)
	out = syn.emit(out, '}')
	return out, at + cw, true
}

func repairObject(b []byte, i int, rw func([]byte) []byte, depth int, syn syntax) ([]byte, int, bool) {
	// O:<len>:"<class>":<n>:{ … }
	nameLen, j, ok := readLen(b, i+1, syn)
	if !ok {
		return nil, 0, false
	}
	qw, ok := syn.match(b, j, '"')
	if !ok {
		return nil, 0, false
	}
	nameAt := j + qw
	nameEnd, ok := syn.advance(b, nameAt, nameLen)
	if !ok {
		return nil, 0, false
	}
	cw, ok := syn.match(b, nameEnd, '"')
	if !ok {
		return nil, 0, false
	}
	// The class name is an identifier, never a URL; it is copied verbatim.
	rest, end, ok := repairArrayTail(b, nameEnd+cw, rw, depth, syn)
	if !ok {
		return nil, 0, false
	}
	out := append([]byte{}, b[i:nameEnd+cw]...)
	return append(out, rest...), end, true
}

// repairArrayTail is `:<n>:{ … }`, shared by objects.
func repairArrayTail(b []byte, i int, rw func([]byte) []byte, depth int, syn syntax) ([]byte, int, bool) {
	n, j, ok := readLen(b, i, syn)
	if !ok {
		return nil, 0, false
	}
	ow, ok := syn.match(b, j, '{')
	if !ok {
		return nil, 0, false
	}
	at := j + ow
	var body []byte
	for k := 0; k < n*2; k++ {
		v, next, vok := repairValue(b, at, rw, depth+1, syn)
		if !vok {
			return nil, 0, false
		}
		body = append(body, v...)
		at = next
	}
	cw, ok := syn.match(b, at, '}')
	if !ok {
		return nil, 0, false
	}
	if string(body) == string(b[j+ow:at]) {
		return b[i : at+cw], at + cw, true
	}
	var out []byte
	out = syn.emit(out, ':')
	out = append(out, strconv.Itoa(n)...)
	out = syn.emit(out, ':')
	out = syn.emit(out, '{')
	out = append(out, body...)
	out = syn.emit(out, '}')
	return out, at + cw, true
}

// pctIs reports whether b at i is the percent escape for c.
func pctIs(b []byte, i int, c byte) bool {
	if i < 0 || i+2 >= len(b) || b[i] != '%' {
		return false
	}
	v, ok := unhex(b[i+1], b[i+2])
	return ok && v == c
}

// advanceDecoded walks n decoded bytes from i and returns the encoded offset.
func advanceDecoded(b []byte, i, n int) (int, bool) {
	if n < 0 {
		return 0, false
	}
	for ; n > 0; n-- {
		if i >= len(b) {
			return 0, false
		}
		if b[i] == '%' {
			if i+2 >= len(b) {
				return 0, false
			}
			if _, ok := unhex(b[i+1], b[i+2]); !ok {
				return 0, false
			}
			i += 3
			continue
		}
		i++
	}
	return i, true
}

func unhex(a, b byte) (byte, bool) {
	hi, ok1 := digitVal(a, 16)
	lo, ok2 := digitVal(b, 16)
	if !ok1 || !ok2 {
		return 0, false
	}
	return byte(hi<<4 | lo), true
}

// occupiesItsField reports whether the value parsed from start to end fills the
// whole field it sits in.
//
// This is the sixth rule tried here and the first that is structural rather than
// a pattern. The five before it all asked "does this boundary look real" —
// closes exactly; then also continues with a serialized token; then also leaves
// a residue that does not start with `"`, `;` or `}`. Every one was a guess
// about bytes whose values are unconstrained, and every one was wrong: a stale
// length lands somewhere inside the string's own data, and the residue then
// begins with whatever that string happened to contain. A measured sweep put
// the last rule at 65% — nine bodies in twenty thousand still destroyed, and
// `";` followed by a space is ordinary CSS.
//
// The question that *can* be answered is not "is this boundary real" but "did
// the parse account for everything". A serialized value arrives as an entire
// field: a form value between `=` and `&`, a decoded JSON string, a multipart
// part, a whole text body. If the walk consumed all of it, the declared lengths
// described the data. If anything is left over, some length was short, and
// which one cannot be known — so nothing is re-emitted and the caller rewrites
// without repair, which round-trips.
//
// What that costs: a serialized value embedded in a larger document — inside
// CDATA in a WXR export, or mid-paragraph — no longer has its length re-emitted.
// It is still rewritten, and the response direction declines for the same
// reason, so the two directions stay consistent and the round trip is exact.
func occupiesItsField(b []byte, start, end int) bool {
	// Matched delimiters. A trailing `"` is both a legitimate close — the JSON
	// string in `wp_localize_script`'s `{"opt":"…"}` — and the residue of a
	// parse that stopped short inside a string. Which one it is depends on how
	// the value *opened*, so the opener chooses what may close it.
	//
	// Requiring `&` or `=` alone accepted only a blob written tight against its
	// attribute quote, and threw away an indented `<textarea>`, a second option
	// in the same text node, and `wp_localize_script` — four of six realistic
	// vehicles, including the one round twenty-seven's commit message named.
	// Accepting any quote to fix that let a truncation residue through again.
	// Pairing them does both.
	const (
		ownField = iota
		dq
		sq
		textNode
		escQuote
		jsonQuote
		cdata
	)
	open := ownField
	for start > 0 {
		c := b[start-1]
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
			start--
			continue
		}
		switch {
		case c == '&' || c == '=':
		case c == '"':
			open = dq
		case c == '\'':
			open = sq
		case c == '>':
			open = textNode
		case c == '[':
			// A CDATA section: `<![CDATA[…]]>`. This is the WXR export shape,
			// which round twenty-seven documented as a limitation — a value
			// taken through the preview and imported elsewhere kept the host it
			// was rewritten to and the length it had before.
			open = cdata
		// The combined spelling has no opener of its own, and round 28 was wrong
		// to give it one. In `esc_attr(wp_json_encode(…))` the `\&quot;` are the
		// value's *internal* quotes — what jsonHTMLSyntax reads — while the
		// quote that opens the field is the structural one, `&quot;`, in every
		// shape WordPress can produce: a bare string, an object value, an array
		// element, a nested object. So the case below is the one that fires, and
		// the `\&quot;` case that used to sit here was unreachable anyway: its
		// six trailing bytes are `&quot;`, which this case matches first.
		case start >= 6 && string(b[start-6:start]) == "&quot;":
			open = escQuote
		case start >= 2 && b[start-2] == '\\' && c == '"':
			open = jsonQuote
		default:
			return false
		}
		break
	}
	for i := end; i < len(b); i++ {
		if b[i] == ' ' || b[i] == '\t' || b[i] == '\r' || b[i] == '\n' {
			continue
		}
		switch open {
		case ownField:
			// Its own field: only the next separator may follow, so a stray
			// quote is residue rather than a close.
			return b[i] == '&'
		case dq:
			return b[i] == '"'
		case sq:
			return b[i] == '\''
		case textNode:
			return b[i] == '<'
		case escQuote:
			return i+6 <= len(b) && string(b[i:i+6]) == "&quot;"
		case jsonQuote:
			return i+2 <= len(b) && b[i] == '\\' && b[i+1] == '"'
		case cdata:
			return b[i] == ']'
		}
		return false
	}
	// Ran out of buffer: only a value that starts its own field may end that
	// way, since every other opener promised a closer.
	return open == ownField
}

// repairOpaqueString parses `X:len:"data";` and copies it verbatim.
func repairOpaqueString(b []byte, i int, syn syntax) ([]byte, int, bool) {
	n, j, ok := readLen(b, i+1, syn)
	if !ok {
		return nil, 0, false
	}
	qw, ok := syn.match(b, j, '"')
	if !ok {
		return nil, 0, false
	}
	end, ok := syn.advance(b, j+qw, n)
	if !ok {
		return nil, 0, false
	}
	cw, ok := syn.match(b, end, '"')
	if !ok {
		return nil, 0, false
	}
	tw, ok := syn.match(b, end+cw, ';')
	if !ok {
		return nil, 0, false
	}
	return b[i : end+cw+tw], end + cw + tw, true
}

// repairCustom parses `C:len:"Class":datalen:{opaque}`, rewrites the payload and
// re-emits its length.
//
// Copying it verbatim was a leak. The payload is arbitrary bytes — whatever the
// class wrote — so it holds origins as readily as any string, and skipping it
// sent a production URL inside a WooCommerce `C:` blob to the browser while the
// structure was rewritten around it; in the other direction it wrote the
// variant hostname into the database. Before `C:` was handled at all the field
// declined and the whole-buffer rewrite covered the payload, so adding the case
// traded a stale length for a live origin.
//
// The length is declared, so it re-emits exactly like a string's. The class
// name is a PHP identifier and cannot hold a host, so it is copied.
func repairCustom(b []byte, i int, rw func([]byte) []byte, syn syntax) ([]byte, int, bool) {
	nameLen, j, ok := readLen(b, i+1, syn)
	if !ok {
		return nil, 0, false
	}
	qw, ok := syn.match(b, j, '"')
	if !ok {
		return nil, 0, false
	}
	nameEnd, ok := syn.advance(b, j+qw, nameLen)
	if !ok {
		return nil, 0, false
	}
	cw, ok := syn.match(b, nameEnd, '"')
	if !ok {
		return nil, 0, false
	}
	dataLen, k, ok := readLen(b, nameEnd+cw, syn)
	if !ok {
		return nil, 0, false
	}
	ow, ok := syn.match(b, k, '{')
	if !ok {
		return nil, 0, false
	}
	dataAt := k + ow
	dataEnd, ok := syn.advance(b, dataAt, dataLen)
	if !ok {
		return nil, 0, false
	}
	clw, ok := syn.match(b, dataEnd, '}')
	if !ok {
		return nil, 0, false
	}
	data := b[dataAt:dataEnd]
	repaired := rw(data)
	if string(repaired) == string(data) {
		return b[i : dataEnd+clw], dataEnd + clw, true
	}
	// By delta, for the reason repairString re-emits that way: a fresh
	// measurement has to reproduce whatever reading the walk settled on, and
	// where a character reference was ambiguous it cannot.
	out := append([]byte{}, b[i:nameEnd+cw]...)
	out = syn.emit(out, ':')
	out = append(out, strconv.Itoa(dataLen+len(repaired)-len(data))...)
	out = syn.emit(out, ':')
	out = syn.emit(out, '{')
	out = append(out, repaired...)
	out = syn.emit(out, '}')
	return out, dataEnd + clw, true
}

// BrokenSerialized reports how many serialized headers in b commit and then fail
// to parse — a length that does not describe its data — the corruption this file exists to prevent,
// measured on bytes rather than argued about.
//
// It exists because nothing could see that class. `hostshift diff` compares the
// proxy's output against its own engine's output, so when both were wrong the
// run was GREEN — and five consecutive rounds of silent row destruction went
// unreported by the one check PLAN §7 calls "the only test that validates
// against reality". A parse assertion on the served bytes would have caught
// every one of them on the first run.
//
// A value that parses is not counted, and neither is text that merely resembles
// one: only a header that *commits* — past a real length and its opening
// delimiter — and then fails to parse.
//
// One broken value can raise the count more than once, because a container
// fails when its child does. The number is a detector, not a census: zero means
// every serialized value served will parse, and anything else names a page to
// look at.
func BrokenSerialized(b []byte) int {
	n := 0
	for i := 0; i < len(b); {
		if !valueStart(b, i) {
			i++
			continue
		}
		id := func(x []byte) []byte { return x }
		var end int
		var ok, committed bool
		// Every spelling, including the HTML-escaped one. Knowing only two made
		// the detector flag correctly-escaped content — a serialized option in
		// an `esc_attr` input or a JSON string is not broken, it is escaped —
		// and a check that is always RED is a check nobody reads, which is the
		// mechanism that let five rounds of real corruption pass unnoticed.
		for _, syn := range []syntax{literalSyntax, percentSyntax, htmlSyntax, jsonSyntax, jsonHTMLSyntax} {
			_, e, o, c := repairValueC(b, i, id, 0, syn)
			if o {
				end, ok = e, true
				break
			}
			if c {
				committed = true
			}
		}
		if ok {
			i = end
			continue
		}
		if committed {
			n++
			// Past this header, so one broken value is counted once.
			i += 2
			continue
		}
		i++
	}
	return n
}

// entityRun is the length of a `&quot;` spelling at i, or 0.
//
// Only the quote. Charging every `&…;` one byte was wrong, and wrong in the
// direction that corrupts: `esc_attr` runs with `$double_encode = false`, so an
// `&amp;` already in the data passes through as its five literal bytes and the
// serialized length counts five — while a bare `&` in the data becomes `&amp;`
// and counts one. The two are indistinguishable in the attribute, and guessing
// either way writes a wrong length on the other.
//
// Counting only the quote makes the literal-`&amp;` reading the one taken, and
// the other case then fails to close. This is the fast path, and it is right
// whenever the span holds no reference at all — most values. Where it does,
// stringEnd runs both readings through htmlUnit and lets the closer decide.
//
// "Declining costs a repair; guessing costs a row" was the old justification
// for stopping here, and it was wrong about the first half. A decline still
// rewrites the host and re-emits nothing, so the old length stays on a value
// whose byte count has changed: declining costs a row too, just a recoverable
// one. That is why the reading is now decided rather than assumed.
func entityRun(b []byte, i int) int {
	if i >= len(b) || b[i] != '&' {
		return 0
	}
	for _, e := range []string{"&quot;", "&#34;", "&#034;"} {
		if i+len(e) <= len(b) && string(b[i:i+len(e)]) == e {
			return len(e)
		}
	}
	return 0
}

// advanceEntities walks n decoded bytes from i, counting a character reference
// as the one byte it decodes to.
func advanceEntities(b []byte, i, n int) (int, bool) {
	if n < 0 {
		return 0, false
	}
	for ; n > 0; n-- {
		if i >= len(b) {
			return 0, false
		}
		if w := entityRun(b, i); w > 0 {
			i += w
			continue
		}
		i++
	}
	return i, true
}

// mayHoldSerialized is the cheap gate before the walk: a serialized header is a
// type letter, a colon and then a digit or a quote. One pass, no allocation.
//
// It must never say no to something the walk would have repaired, so it accepts
// the percent-encoded colon too.
func mayHoldSerialized(b []byte) bool {
	// Pivot on the colon with IndexByte, which is vectorised, rather than
	// walking every byte in Go. Scanning byte by byte cost a third of the
	// identity map's throughput on real pages, which is most of what the gate
	// was added to recover.
	//
	// A digit has to follow the colon. Without that check every `https://`
	// matched on its own `s:`, so the gate admitted every page carrying a link
	// and saved nothing.
	for off := 0; off < len(b); {
		k := bytes.IndexByte(b[off:], ':')
		if k < 0 {
			break
		}
		i := off + k
		if i > 0 && i+1 < len(b) && b[i+1] >= '0' && b[i+1] <= '9' {
			switch b[i-1] {
			case 'b', 'i', 'd', 's', 'a', 'O', 'R', 'r', 'E', 'C':
				return true
			}
		}
		off = i + 1
	}
	// `N;` has no colon, and the percent spelling encodes both.
	for i := 0; i+1 < len(b); i++ {
		if b[i] == 'N' && (b[i+1] == ';' || pctIs(b, i+1, ';')) {
			return true
		}
		if b[i] == '%' && pctIs(b, i, ':') && i+3 < len(b) &&
			b[i+3] >= '0' && b[i+3] <= '9' {
			return true
		}
	}
	return false
}

package rewrite

import "strconv"

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
	for _, syn := range []syntax{literalSyntax, percentSyntax} {
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
	if i < len(b) && (b[i] == 's' || b[i] == 'a' || b[i] == 'O') {
		if _, j, ok := readLen(b, i+1, syn); ok {
			d := byte('{')
			if b[i] == 's' || b[i] == 'O' {
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
		return i+1 < len(b) && (b[i+1] == ';' || pctIs(b, i+1, ';'))
	case 'b', 'i', 'd', 's', 'a', 'O', 'R', 'r', 'E', 'C':
		return i+1 < len(b) && (b[i+1] == ':' || pctIs(b, i+1, ':'))
	}
	return false
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
	// dlen is how many bytes data decodes to.
	dlen func(data []byte) int
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
	dlen: func(data []byte) int { return len(data) },
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
	dlen:    decodedLen,
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
	dataEnd, ok := syn.advance(b, dataAt, n)
	if !ok {
		return nil, 0, false
	}
	cw, ok := syn.match(b, dataEnd, '"')
	if !ok {
		return nil, 0, false
	}
	tw, ok := syn.match(b, dataEnd+cw, ';')
	if !ok {
		return nil, 0, false
	}
	end := dataEnd + cw + tw

	data := b[dataAt:dataEnd]
	// A string can hold another serialized payload. Try to parse it *as input*,
	// where its lengths are still right.
	var repaired []byte
	inner, iend, iok, icommitted := repairValueC(data, 0, rw, depth+1, syn)
	switch {
	case iok && iend == len(data):
		repaired = inner
	case icommitted:
		// Committed — it really is a serialized header — but it does not parse
		// to the end. That is a nested payload whose own lengths are stale, and
		// re-emitting only the outer one is the worst outcome available: the
		// outer parses, so nothing errors, and the failure surfaces on a later
		// unserialize of the inner value.
		//
		// Shape alone is not enough to decide this. Testing `valueStart` instead
		// declined on `a:hover{color:red}`, `d:\\shares\\logo.png`, `i:12345`
		// and `O:brien` — ordinary strings that begin with two bytes that happen
		// to look like a header.
		// Serialized-shaped but it does not parse to the end. That is the
		// signature of a stale length: the declared end landed inside the data,
		// after something that happened to look like a complete value, and the
		// remainder is the rest of the string rather than the container's next
		// element. Believing it is how a valid row was destroyed. Declining the
		// whole span leaves both lengths alone, so the rewrite the response
		// direction made without repair is undone exactly.
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
	var out []byte
	out = append(out, 's')
	out = syn.emit(out, ':')
	out = append(out, strconv.Itoa(syn.dlen(repaired))...)
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

// decodedLen is how many bytes b decodes to. `+` counts as one byte, which is
// what it decodes to — treating it as an escape would be the bug.
func decodedLen(b []byte) int {
	n := 0
	for i := 0; i < len(b); n++ {
		if b[i] == '%' && i+2 < len(b) {
			if _, ok := unhex(b[i+1], b[i+2]); ok {
				i += 3
				continue
			}
		}
		i++
	}
	return n
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
	// It must begin the field as well as end it, or the bytes before it are
	// unaccounted for in the same way.
	if start > 0 {
		switch b[start-1] {
		case '&', '=':
		default:
			return false
		}
	}
	// And run to the end of the field: whitespace only, or the next separator.
	for i := end; i < len(b); i++ {
		switch b[i] {
		case ' ', '\t', '\r', '\n':
		case '&':
			return true
		default:
			return false
		}
	}
	return true
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
	out := append([]byte{}, b[i:nameEnd+cw]...)
	out = syn.emit(out, ':')
	out = append(out, strconv.Itoa(syn.dlen(repaired))...)
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
		for _, syn := range []syntax{literalSyntax, percentSyntax} {
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

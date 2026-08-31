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
		// place it can be caught.
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
		if isTruncationResidue(b, end) {
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
	case 'b', 'i', 'd', 's', 'a', 'O':
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
	case 'i':
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
	inner, iend, iok := repairValue(data, 0, rw, depth+1, syn)
	switch {
	case iok && iend == len(data):
		repaired = inner
	case len(data) > 0 && valueStart(data, 0):
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

// isTruncationResidue reports whether what follows a parsed value looks like the
// tail of a string the parse stopped short of.
func isTruncationResidue(b []byte, i int) bool {
	for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\r' || b[i] == '\n') {
		i++
	}
	if i >= len(b) {
		return false
	}
	switch b[i] {
	case '"', ';', '}':
		return true
	}
	// The percent spellings of the same three.
	return pctIs(b, i, '"') || pctIs(b, i, ';') || pctIs(b, i, '}')
}

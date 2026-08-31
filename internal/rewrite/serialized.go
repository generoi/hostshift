package rewrite

import (
	"bytes"
	"strconv"
)

// RepairSerialized rewrites a PHP-serialized payload and re-emits its length
// prefixes, so the result still parses.
//
// `s:LEN:"DATA";` counts DATA in bytes, so any rewrite that changes a byte
// count leaves LEN stale and PHP refuses the whole structure — `false` where an
// options array should be, into a shared database, with no undo and no error.
//
// Skipping such a body instead was tried and was worse. The response direction
// rewrites too, so the browser is served a blob whose prefix already counts the
// canonical host while the data holds the variant; passing the POST back
// untouched then writes that stale prefix *and* the variant hostname upstream.
// Repairing is the only option that is correct in both directions.
//
// Three spellings reach us and all three are handled in one pass: the literal
// `s:5:"…"`, the percent-encoded `s%3A5%3A%22…%22%3B` a form sends, and — via
// serializedJSONValue in json.go — the JSON-escaped one. Scanning for them
// separately meant a body carrying two spellings had the second rewritten by
// the first pass's gap handling and never repaired.
//
// The lengths are read from the *input*, where they are still right, so spans
// are found exactly rather than guessed. That is also why the recursion for a
// nested payload takes the original bytes: reading a length from data whose
// host has already changed finds nothing, which left every inner prefix stale
// while the outer one was repaired — the worst of both, because the outer then
// parses and only the second unserialize fails, silently.
func RepairSerialized(b []byte, rw func([]byte) []byte) []byte {
	out, _ := RepairSerializedFound(b, rw)
	return out
}

// RepairSerializedFound is RepairSerialized, also reporting whether a
// serialized span was found — which is not the same question as whether
// anything changed, and callers that route on it need the first.
func RepairSerializedFound(b []byte, rw func([]byte) []byte) ([]byte, bool) {
	out, ok := repairIn(b, rw, 0)
	if !ok {
		return rw(b), false
	}
	return out, true
}

// maxSerializedDepth bounds the recursion for a payload nested inside a
// serialized string, which WordPress produces whenever an option value is an
// array of serialized values.
const maxSerializedDepth = 8

// span is one `s:LEN:"DATA"` header, in whichever spelling matched.
type span struct {
	at, dataAt, dataEnd int // header start, data start, one past data end
	pct                 bool
}

func repairIn(b []byte, rw func([]byte) []byte, depth int) ([]byte, bool) {
	if depth > maxSerializedDepth {
		return b, false
	}
	var out []byte
	prev, found := 0, false
	for i := 0; i < len(b); {
		sp, ok := headerAt(b, i)
		if !ok {
			i++
			continue
		}
		// The gap since the last span, rewritten once.
		out = append(out, rw(b[prev:sp.at])...)

		inner := b[sp.dataAt:sp.dataEnd]
		// The nested repair runs on the *input* bytes, so the inner lengths are
		// still trustworthy. Only when the data holds nothing serialized does rw
		// apply to it directly — otherwise rw would run twice over the same
		// bytes, once here and once inside the nested call's own gap handling.
		rewritten, nested := repairIn(inner, rw, depth+1)
		if !nested {
			rewritten = rw(inner)
		}

		found = true
		out = appendHeader(out, b[sp.at:sp.dataAt], rewritten, inner, sp.pct)
		out = append(out, rewritten...)
		prev = sp.dataEnd
		i = sp.dataEnd
	}
	if !found {
		return b, false
	}
	return append(out, rw(b[prev:])...), true
}

// appendHeader writes the `s:LEN:"` prefix, keeping the original bytes verbatim
// when nothing about the data changed.
//
// Re-emitting unconditionally changed bytes under an identity map, which test 24
// forbids: strconv.Itoa drops a leading zero from `s:05:` and the literals here
// force uppercase hex, so a client that percent-encodes in lowercase had its
// body altered by a proxy asked to be a no-op.
func appendHeader(out, orig, rewritten, inner []byte, pct bool) []byte {
	if bytes.Equal(rewritten, inner) {
		return append(out, orig...)
	}
	n := len(rewritten)
	if pct {
		n = decodedLen(rewritten)
		out = append(out, `s%3A`...)
		out = append(out, strconv.Itoa(n)...)
		return append(out, `%3A%22`...)
	}
	out = append(out, 's', ':')
	out = append(out, strconv.Itoa(n)...)
	return append(out, ':', '"')
}

// headerAt matches a serialized string header at i, in either spelling, and
// locates its data exactly using the declared length.
func headerAt(b []byte, i int) (span, bool) {
	if i >= len(b) || b[i] != 's' {
		return span{}, false
	}
	// Literal: s:LEN:"DATA";
	if i+1 < len(b) && b[i+1] == ':' {
		j := i + 2
		for j < len(b) && b[j] >= '0' && b[j] <= '9' {
			j++
		}
		if j > i+2 && j+1 < len(b) && b[j] == ':' && b[j+1] == '"' {
			if n, err := strconv.Atoi(string(b[i+2 : j])); err == nil && n >= 0 && n <= len(b) {
				end := j + 2 + n
				// The closer has to be exactly where the length says, *and* what
				// follows has to be a serialized token. A sentence quoting
				// `s:6:"a.test"` is not a span unless it closes there — and a
				// span whose declared length is stale can still land on a `";`
				// inside its own data, which is the commonest two-byte sequence
				// in serialized data holding HTML or CSS.
				//
				// That false boundary is how a valid row was destroyed. The HTML
				// response arm does not repair, so the browser is served a blob
				// whose length is stale; the request arm then trusted that length,
				// found a `";` six bytes early, and wrote a *different* wrong
				// number — turning a round trip that used to self-heal into
				// permanent corruption. Requiring a valid continuation makes a
				// mis-parse decline instead, which restores the self-healing.
				if end < len(b) && b[end] == '"' &&
					(end+1 >= len(b) || b[end+1] == ';') &&
					validContinuation(b, end+2) {
					return span{at: i, dataAt: j + 2, dataEnd: end}, true
				}
			}
		}
		return span{}, false
	}
	// Percent-encoded: s%3ALEN%3A%22DATA%22%3B
	if !pctIs(b, i+1, ':') {
		return span{}, false
	}
	j := i + 4
	start := j
	for j < len(b) && b[j] >= '0' && b[j] <= '9' {
		j++
	}
	if j == start || !pctIs(b, j, ':') || !pctIs(b, j+3, '"') {
		return span{}, false
	}
	// `n <= len(b)` before it is used as an offset. A 19-digit length made
	// `j + 2 + n` overflow to a negative index, the `end < len(b)` bounds check
	// passed because negative is less than len, and the read panicked — a 502
	// from the proxy on any request or response body carrying that byte
	// sequence, which post and comment content can. No declared length larger
	// than the buffer can be honest anyway.
	n, err := strconv.Atoi(string(b[start:j]))
	if err != nil || n < 0 || n > len(b) {
		return span{}, false
	}
	dataAt := j + 6
	end, ok := advanceDecoded(b, dataAt, n)
	if !ok || !pctIs(b, end, '"') || !pctIs(b, end+3, ';') {
		return span{}, false
	}
	return span{at: i, dataAt: dataAt, dataEnd: end, pct: true}, true
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

// validContinuation reports whether b at i begins something that can follow a
// serialized value: another typed token, a container close, or the end.
//
// It is what separates a real span from a stale length that happened to land on
// a `";` inside its own data. Declining there is always safe — the caller falls
// back to rewriting without repair, which is what the code did before repair
// existed and which round-trips.
func validContinuation(b []byte, i int) bool {
	if i >= len(b) {
		return true
	}
	switch b[i] {
	case '}', ')':
		return true
	// A form field separator. A whole option value can be a single span, and
	// then what follows it is the end of that field, not another token.
	case '&':
		return true
	case 'N':
		return i+1 < len(b) && b[i+1] == ';'
	case 's', 'i', 'd', 'b', 'a', 'O', 'C', 'E', 'R':
		return i+1 < len(b) && b[i+1] == ':'
	}
	return false
}

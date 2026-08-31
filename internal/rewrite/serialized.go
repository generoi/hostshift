package rewrite

import "strconv"

// RewriteSerialized rewrites a PHP-serialized payload and re-emits its length
// prefixes, so the result still parses.
//
// `s:LEN:"DATA";` counts DATA in bytes, so any rewrite that changes a byte count
// leaves LEN stale and PHP refuses the whole structure — `false` where an
// options array should be, into a shared database, with no undo and no error.
//
// Skipping such a body instead was tried and was worse. The response direction
// rewrites too, so the browser is *served* a blob whose prefix already counts
// the canonical host while the data holds the variant; passing the POST back
// through untouched then writes that stale prefix *and* the variant hostname
// upstream. Rewriting without repair at least round-trips: the length the
// response direction broke, the request direction restores. Repairing is the
// only option that is correct in both directions.
//
// The lengths are read from the *input*, where they are still right, so the
// spans are found exactly rather than by guessing where a string ends. A body
// that does not parse is not serialized data and is returned untouched, for the
// caller to rewrite however it would have.
func RewriteSerialized(b []byte, rw func([]byte) []byte) ([]byte, bool) {
	out, ok := rewriteSerializedIn(b, rw, 0)
	return out, ok
}

// maxSerializedDepth bounds the recursion for a doubly-serialized payload — a
// serialized string whose data is itself serialized, which WordPress produces
// whenever an option value is an array of serialized values. Without the
// recursion the inner prefix goes stale exactly as the outer one did.
const maxSerializedDepth = 8

func rewriteSerializedIn(b []byte, rw func([]byte) []byte, depth int) ([]byte, bool) {
	if depth > maxSerializedDepth {
		return b, false
	}
	var out []byte
	prev, found := 0, false
	for i := 0; i+3 < len(b); {
		// `s:` then digits then `:"`.
		if b[i] != 's' || b[i+1] != ':' {
			i++
			continue
		}
		j := i + 2
		for j < len(b) && b[j] >= '0' && b[j] <= '9' {
			j++
		}
		if j == i+2 || j+1 >= len(b) || b[j] != ':' || b[j+1] != '"' {
			i++
			continue
		}
		n, err := strconv.Atoi(string(b[i+2 : j]))
		if err != nil || n < 0 {
			i++
			continue
		}
		start := j + 2
		end := start + n
		// The closing `";` has to be exactly where the length says it is. If it
		// is not, this is not a serialized string — a sentence containing
		// `s:12:"` is not one — and nothing here is touched.
		if end+1 >= len(b)+1 || end+1 > len(b) || b[end] != '"' {
			i++
			continue
		}
		if end+1 < len(b) && b[end+1] != ';' {
			i++
			continue
		}
		inner := b[start:end]
		rewritten := rw(inner)
		// A serialized string can hold another serialized payload.
		if nested, ok := rewriteSerializedIn(rewritten, rw, depth+1); ok {
			rewritten = nested
		}
		found = true
		// The gap is rewritten too. Structural bytes hold no origins so this is
		// a no-op on well-formed data — but a buffer that is only *partly*
		// serialized (a sentence quoting `s:11:"hello world"`, or a blob whose
		// trailing span has a stale length) would otherwise have everything
		// after the last good span copied out verbatim and never rewritten.
		out = append(out, rw(b[prev:i])...)
		out = append(out, 's', ':')
		out = append(out, strconv.Itoa(len(rewritten))...)
		out = append(out, ':', '"')
		out = append(out, rewritten...)
		out = append(out, '"')
		prev = end + 1
		i = end + 1
	}
	if !found {
		return b, false
	}
	return append(out, rw(b[prev:])...), true
}

// RewriteSerializedPct is RewriteSerialized for a percent-encoded payload,
// which is how a form actually sends one.
//
// `options.php` and `admin-ajax.php` percent-encode their bodies, so the header
// arrives as `s%3A51%3A%22` and a scanner looking for the literal `s:51:"`
// never sees it — while the percent view rewrites the host inside it perfectly
// happily. That is the settings flow the whole problem is about, and it is the
// one spelling the literal parser misses.
//
// The length counts *decoded* bytes, so the span is found by walking the
// encoded data until that many decoded bytes have passed.
func RewriteSerializedPct(b []byte, rw func([]byte) []byte) ([]byte, bool) {
	var out []byte
	prev, found := 0, false
	for i := 0; i < len(b); {
		j, n, ok := pctStringHeader(b, i)
		if !ok {
			i++
			continue
		}
		end, ok := advanceDecoded(b, j, n)
		if !ok || !pctHasSuffix(b, end) {
			i++
			continue
		}
		inner := b[j:end]
		rewritten := rw(inner)
		found = true
		out = append(out, rw(b[prev:i])...)
		out = append(out, `s%3A`...)
		out = append(out, strconv.Itoa(decodedLen(rewritten))...)
		out = append(out, `%3A%22`...)
		out = append(out, rewritten...)
		prev = end
		i = end
	}
	if !found {
		return b, false
	}
	return append(out, rw(b[prev:])...), true
}

// pctStringHeader matches `s%3A<digits>%3A%22` at i, returning where the data
// starts and the declared decoded length.
func pctStringHeader(b []byte, i int) (int, int, bool) {
	if i+10 > len(b) || b[i] != 's' || !pctIs(b, i+1, ':') {
		return 0, 0, false
	}
	j := i + 4
	start := j
	for j < len(b) && b[j] >= '0' && b[j] <= '9' {
		j++
	}
	if j == start || !pctIs(b, j, ':') || !pctIs(b, j+3, '"') {
		return 0, 0, false
	}
	n, err := strconv.Atoi(string(b[start:j]))
	if err != nil || n < 0 {
		return 0, 0, false
	}
	return j + 6, n, true
}

// pctIs reports whether b at i is the percent escape for c.
func pctIs(b []byte, i int, c byte) bool {
	if i+2 >= len(b) || b[i] != '%' {
		return false
	}
	v, ok := unhex(b[i+1], b[i+2])
	return ok && v == c
}

// pctHasSuffix reports whether the encoded `";` closer sits at i.
func pctHasSuffix(b []byte, i int) bool {
	return pctIs(b, i, '"') && pctIs(b, i+3, ';')
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

// decodedLen is how many bytes b decodes to.
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

// RepairSerialized rewrites b with rw, keeping any PHP-serialized length
// prefixes correct, in either the literal or the percent-encoded spelling.
//
// It returns rw(b) unchanged when b holds nothing serialized, so a caller can
// use it in place of rw everywhere without deciding first.
func RepairSerialized(b []byte, rw func([]byte) []byte) []byte {
	if out, ok := RewriteSerialized(b, rw); ok {
		return out
	}
	if out, ok := RewriteSerializedPct(b, rw); ok {
		return out
	}
	return rw(b)
}

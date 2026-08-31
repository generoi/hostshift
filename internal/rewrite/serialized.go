package rewrite

import (
	"bytes"
	"sort"
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
		end := fieldBreak(b, start)
		rep, ok := repairField(b[start:end], rw)
		out = append(out, rep...)
		found = found || ok
		if end < len(b) {
			out = append(out, '&')
		}
		start = end + 1
	}
	// Over the whole buffer, not the fields joined back together, even though
	// they hold the same bytes when nothing was repaired.
	//
	// The split is on a raw `&`, which in a properly encoded body is always a
	// separator — but a *character reference* is also written with one, so
	// `https:&#47;&#47;host` is cut into three fields and the host is not in any
	// of them. Rewriting the assembled buffer is what catches it, and dropping
	// this to stop double-counting events sent a variant hostname to the
	// upstream in two of the request-body spellings.
	//
	// The cost is that every event and leak counter in a payload-free body is
	// recorded twice, which since round 31 wrapped the query string means every
	// ordinary `?redirect_to=…&x=1`. Served bytes are right; `--explain` and the
	// census are double. That is the cheaper of the two errors — and it is not
	// the only inflation here, since repairAt tries five spellings and each may
	// call rw before one succeeds.
	if !found {
		return rw(b)
	}
	return out
}

// fieldBreak is the offset of the `&` that ends the field beginning at start,
// or len(b).
//
// A `&` that opens a character reference is not one. The split used to take any
// `&`, so `https:&#47;&#47;host` was cut into three fields and the byte matcher
// — which needs `//`, `\/` or `%2F` — found the host in none of them. A
// whole-buffer pass covered that, but only when no field carried a serialized
// value, and `options.php` posts every option on a settings page in one body:
// one serialized option disarmed it for the rest, and a variant hostname went
// into the shared database.
//
// Splitting correctly is better than covering for a bad split. A real separator
// in a urlencoded body cannot be a reference, because a `&` inside a value is
// `%26` — so where these two disagree, the reference is what the sender meant.
func fieldBreak(b []byte, start int) int {
	for i := start; i < len(b); i++ {
		if b[i] == '&' && refRun(b, i) == 0 {
			return i
		}
	}
	return len(b)
}

// repairField repairs one `&`-delimited field.
func repairField(b []byte, rw func([]byte) []byte) ([]byte, bool) {
	var out []byte
	prev, found := 0, false
	for i := 0; i < len(b); {
		if !valueShape(b, i) {
			i++
			continue
		}
		if !valuePosition(b, i) {
			// Glued to the text in front of it — a label with no trailing
			// punctuation, `Åtgärd` or `v2` or `]`. The position gate exists to
			// keep `https://x/s:3:"a"` in a URL path from committing and then
			// declining the buffer, and it cannot tell that apart from a real
			// value behind a label: both are a header after a non-separator.
			//
			// So this arm is strictly additive. It looks, and it accepts only a
			// value that parses completely with nothing but whitespace after it.
			// Anything else is skipped exactly as before — never a decline,
			// never a count — so a page that worked cannot start failing here.
			if rep, end, ok, _ := repairAt(b, i, rw, func(e int) bool {
				return onlySpaceAfter(b, e)
			}); ok {
				out = append(out, rw(b[prev:i])...)
				out = append(out, rep...)
				prev, i = end, end
				found = true
				continue
			}
			i++
			continue
		}
		rep, end, ok, committed := repairAt(b, i, rw, func(e int) bool {
			return occupiesItsField(b, i, e)
		})
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

// onlySpaceAfter reports whether nothing but whitespace follows end.
func onlySpaceAfter(b []byte, end int) bool {
	for i := end; i < len(b); i++ {
		switch b[i] {
		case ' ', '\t', '\r', '\n':
		case '\\':
			if i+1 < len(b) && (b[i+1] == 't' || b[i+1] == 'r' || b[i+1] == 'n') {
				i++
				continue
			}
			return false
		default:
			return false
		}
	}
	return true
}

// repairAt tries each spelling at i. committed reports whether the candidate got
// past its length and opening delimiter — far enough that it really is a
// serialized header and a failure to parse means the buffer is untrustworthy.
//
// Without that distinction every `https:` in a URL was a candidate whose length
// parse failed immediately, and "a failed candidate declines the buffer" then
// declined every body containing a link.
func repairAt(b []byte, i int, rw func([]byte) []byte, fieldOK func(end int) bool) (rep []byte, end int, ok, committed bool) {
	for _, base := range []syntax{literalSyntax, percentSyntax, htmlSyntax, jsonSyntax, jsonHTMLSyntax} {
		r, e, o, c, amb := tryBothPicks(b, i, rw, 0, base, fieldOK)
		if o {
			return r, e, true, c
		}
		if c || amb {
			committed = true
		}
	}
	return nil, 0, false, committed
}

// tryBothPicks parses at i under one spelling, letting the enclosing parse
// decide between readings rather than preferring one.
//
// Where a value's data holds a character reference, more than one reading can
// close — and inside a nested payload that is the normal case, not a corner.
// Choosing by preference would be the guess this file exists to avoid, so both
// choices are run and the answer is kept only when they agree, or when only one
// of them parses the whole value *and* satisfies its field. Where both parse to
// different lengths the value is genuinely ambiguous and it declines, which is
// the outcome every reading of it used to get.
//
// The second walk runs only when the first had a tie to break, which is what
// `chose` reports — so an ordinary value is parsed once.
func tryBothPicks(b []byte, i int, rw func([]byte) []byte, depth int, base syntax,
	fieldOK func(end int) bool) (rep []byte, end int, ok, committed, ambiguous bool) {

	run := func(pick int, chose *bool) ([]byte, int, bool, bool) {
		syn := base
		syn.pick, syn.chose = pick, chose
		r, e, o, c := repairValueC(b, i, rw, depth, syn)
		if o && fieldOK != nil && !fieldOK(e) {
			return nil, 0, false, c
		}
		return r, e, o, c
	}

	chose := false
	r1, e1, o1, c1 := run(pickFirst, &chose)
	if !chose {
		// No tie was broken, so the other policy would walk the same bytes.
		return r1, e1, o1, c1, false
	}
	r2, e2, o2, c2 := run(pickLast, nil)
	switch {
	case o1 && o2 && (e1 != e2 || string(r1) != string(r2)):
		// Two complete parses that disagree. Nothing here ranks them.
		return nil, 0, false, c1 || c2, true
	case o1:
		return r1, e1, true, c1, false
	case o2:
		return r2, e2, true, c2, false
	}
	return nil, 0, false, c1 || c2, false
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
	return valueShape(b, i) && valuePosition(b, i)
}

// valueShape is the header test: a serialized value begins with one of these
// letters followed by its delimiter.
//
// The *shape*, not just the first byte. Matching on the letter alone made
// `a.test` inside an ordinary string look like the start of an array, and made
// the word "see" a candidate — which matters because a candidate that fails to
// parse declines the whole buffer.
func valueShape(b []byte, i int) bool {
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
	return true
}

// valuePosition is the other half: the value must sit where one can begin.
// Without it a URL path holding `s:3:"a"` was a candidate — it commits, fails
// to close, and the detector reported a healthy page as carrying broken data.
//
// It does not apply inside a string, where the bytes in front of a payload are
// the field's own label. `Obs: ` ends in a separator; `v2`, `Åtgärd` and `]` do
// not, and that difference decided whether the value was seen at all.
func valuePosition(b []byte, i int) bool {
	if i == 0 {
		return true
	}
	switch b[i-1] {
	// Whitespace and `>`, because a `<textarea>` WordPress indents and a text
	// node both put a value there. `+` because that is how a form body spells a
	// space, which is what a field label ends with. occupiesItsField and
	// the nested walk still decide whether the value fills its field; this gate
	// only decides whether to look, so being too permissive costs a parse
	// attempt and being too strict costs a repair.
	case '{', ';', '"', '=', '&', ':', ',', '\'', '>', '[', '+', ' ', '\t', '\r', '\n':
		return true
	}
	// The same separators as the escapes a spelling writes them with.
	//
	// Two ways this was blind. JSON writes `\n` as two bytes, so the byte before
	// a payload on its own line is an ordinary `n`. And a percent escape is
	// three bytes wide, so the one that *ends* at i-1 starts at i-3 — asking
	// pctIs about i-1 asks whether the second hex digit is a `%`, which it never
	// is. Every percent opener here was dead from the day the first was written,
	// in the one spelling `options.php` and every urlencoded POST use, and the
	// detector gates on this same function: a body PHP refuses was reported
	// GREEN because neither side of the diff could see into it.
	//
	// The percent set is the raw set, rather than a few of them chosen by hand.
	// Choosing by hand is what left `%3A` out while `%20` was in, so a label
	// ending in a colon was invisible and one ending in a space was not.
	for _, c := range separators {
		if pctIs(b, i-3, c) {
			return true
		}
	}
	return (i >= 6 && string(b[i-6:i]) == "&quot;") ||
		(i >= 2 && b[i-2] == '\\' &&
			(b[i-1] == '"' || b[i-1] == 'n' || b[i-1] == 't' || b[i-1] == 'r'))
}

// separators is the raw set above, for the spellings that escape them.
var separators = []byte{
	'{', ';', '"', '=', '&', ':', ',', '\'', '>', '[', '+', ' ', '\t', '\r', '\n',
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
	// chose is set when pick actually had to break a tie, so a caller can skip
	// the second walk when the two policies cannot differ.
	chose *bool
	// pick chooses between readings that all close, when more than one does:
	// pickFirst takes the earliest, pickLast the latest. The two are run
	// against each other by repairAt, and a value is repaired only when they
	// agree or only one of them parses — so this is never the final word.
	pick int
	// dlen is how many bytes data decodes to, for the spellings where that has
	// one answer. nil where a character reference makes it ambiguous, and a
	// re-emitted length is then taken as a delta instead.
	dlen func(data []byte) int
}

// decodedLen is how many bytes b decodes to in the percent spelling. `+` counts
// as one byte, which is what it decodes to — treating it as an escape would be
// the bug.
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

// emitLen is the length to re-emit for data that was declared n and came back
// as repaired.
//
// A delta in *source* bytes wherever the spelling cannot measure — which is
// only the two that carry character references, and there a rewrite adds and
// removes plain bytes, so a byte added to the source is a byte added to the
// data.
//
// It was the delta everywhere, and in the percent spelling that is false: a
// separator is three source bytes for one decoded one, so a map that changes
// the scheme or drops a port changes the two counts in *opposite directions*.
// `http%3A%2F%2Flocalhost%3A8080` → `https%3A%2F%2Fwww.example.fi` is one source
// byte shorter and one decoded byte longer, and that goes into the shared
// database on an ordinary options.php save, where nothing scores it.
func emitLen(syn syntax, n int, data, repaired []byte) int {
	if syn.dlen != nil {
		return syn.dlen(repaired)
	}
	return n + len(repaired) - len(data)
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
	// The quote spellings are not offered both ways. `&quot;` is this spelling's
	// own delimiter, so a payload nested inside a string is *made of* them —
	// and offering each one two widths gives a span holding k of them up to 2^k
	// readings, nearly all spurious. That is not a theoretical cost: at six
	// array members it declined 34 of 60, in *both* directions, and the
	// detector runs the same walk, so it also reported a page PHP accepts as
	// broken. A check that is RED on healthy pages is one nobody reads, which
	// is the mechanism that hid five rounds of real corruption.
	//
	// So a quote counts one, which is what it is wherever this spelling put it.
	// Data that genuinely held a literal `&quot;` before esc_attr saw it then
	// fails to close and declines — the same cost that reading carried before,
	// and the rarer case by far.
	if w := entityRun(b, i); w > 0 {
		return w, 1, 0
	}
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
	// No quote guard here, unlike htmlUnit. This spelling writes a data quote as
	// `\&quot;`, which jsonHTMLRun already measured above, so a *bare* `&quot;`
	// in the data can only be the six literal bytes — content that held `&quot;`
	// before either escaper saw it. Forcing it to one, as the guard next door
	// does, would be wrong in the corrupting direction, and the explosion that
	// guard exists to prevent cannot happen here: the delimiters are seven bytes
	// wide and unambiguous, so a nested payload adds no readings at all.
	if w := refRun(b, i); w > 0 {
		return w, 1, w
	}
	return 1, 1, 0
}

// maxStringReadings bounds the search: how many *distinct* readings of the same
// span may be alive at once. A value that branches past it is declined rather
// than explored, which is what the code did with every ambiguous value before
// the readings existed at all.
const maxStringReadings = 2048

// advanceReadings returns every offset at which exactly n decoded bytes have
// passed from i. More than one means the source is genuinely ambiguous and the
// caller must not choose.
//
// The readings only ever diverge at an ambiguous unit. Between two of them
// every live reading walks the identical bytes and consumes the identical
// number of decoded ones, so the run between branches needs no state at all: it
// is one walk, and a shared counter shifts every reading down together.
//
// Carrying a state per (offset, remaining) instead made the count grow with the
// *length* of the value rather than with its ambiguity. After a single `&amp;`
// every later offset had two live remainders, so the cap was reached at about a
// kilobyte and a 1 KB option with one ampersand in it declined — and a decline
// is host-independent, so it fired identically on the canonical page and
// cancelled under the corpus diff's baseline subtraction. Silent, again.
func advanceReadings(b []byte, i, n int, unit func([]byte, int) (int, int, int)) []int {
	if n < 0 {
		return nil
	}
	// Remaining decoded bytes for each live reading, as of the last branch, and
	// what has been consumed since. Sorted ascending, so only the smallest can
	// be the next to finish.
	live, spent := []int{n}, 0
	var ends []int
	for {
		for len(live) > 0 && live[0] <= spent {
			if live[0] == spent {
				ends = append(ends, i)
			}
			// A reading below the counter ended inside a unit, which a length
			// counting whole characters cannot do. It is dropped, not recorded.
			live = live[1:]
		}
		if len(live) == 0 || i >= len(b) {
			return ends
		}
		src, dec, alt := unit(b, i)
		if src == 0 {
			return ends
		}
		if alt <= 0 || alt == dec {
			spent += dec
			i += src
			continue
		}
		next := make([]int, 0, 2*len(live))
		for _, r := range live {
			for _, d := range [2]int{dec, alt} {
				if r-spent-d >= 0 {
					next = append(next, r-spent-d)
				}
			}
		}
		sort.Ints(next)
		live = next[:0]
		for k, v := range next {
			if k == 0 || v != next[k-1] {
				live = append(live, v)
			}
		}
		if len(live) > maxStringReadings {
			return nil
		}
		spent, i = 0, i+src
	}
}

// stringEnd finds where the data of a string of n decoded bytes ends, given
// that a `";` must follow it.
//
// The fast path is the spelling's own single reading, and for a span with no
// character reference in it that reading is the only one. Otherwise the closer
// picks: exactly one reading that closes is the answer, and two are an
// ambiguity this must not resolve by preference.
func stringEnd(b []byte, dataAt, n int, syn syntax) (dataEnd, cw, tw int, ok bool) {
	e, ok := spanEnd(b, dataAt, n, syn, func(e int) bool {
		w, ok := syn.match(b, e, '"')
		if !ok {
			return false
		}
		_, ok = syn.match(b, e+w, ';')
		return ok
	})
	if !ok {
		return 0, 0, 0, false
	}
	cw, _ = syn.match(b, e, '"')
	tw, _ = syn.match(b, e+cw, ';')
	return e, cw, tw, true
}

// spanEnd finds where a span of n decoded bytes ends, given a test for what
// must follow it. stringEnd is this with `";`, and a `C:` payload is this with
// `}`.
//
// The `C:` payload used syn.advance alone, and advance knows one reading. So a
// WooCommerce blob like `C:7:"WC_Data":28:{http://a.test/p?q=1&r=2}` under
// esc_attr declined on the ampersand — and a decline still rewrites the host and
// re-emits nothing, which is the whole reason this file stopped treating one as
// safe.
func spanEnd(b []byte, at, n int, syn syntax, closes func(int) bool) (int, bool) {
	if e, ok := syn.advance(b, at, n); ok && closes(e) {
		if syn.unit == nil || bytes.IndexByte(b[at:e], '&') < 0 {
			return e, true
		}
	}
	if syn.unit == nil {
		return 0, false
	}
	var closers []int
	for _, e := range advanceReadings(b, at, n, syn.unit) {
		if closes(e) {
			closers = append(closers, e)
		}
	}
	if len(closers) == 0 {
		return 0, false
	}
	if len(closers) > 1 {
		// More than one reading closes, which happens constantly once a payload
		// is nested inside a string: the closer `&quot;;` stands at every
		// internal boundary, so charging an `&amp;` its literal width lands on
		// one of them. Refusing here declined 111 of 200 realistic ACF and
		// wp_options blobs, and a decline still rewrites the host.
		//
		// So a choice is made and then checked. advanceReadings returns offsets
		// in ascending order; the escaped reading of a reference consumes five
		// source bytes for one, so it reaches the declared count later than the
		// literal reading does. Neither is right on its own — which is why
		// repairAt runs both and keeps the answer only if they agree, or if only
		// one of them lets the whole value parse.
		if syn.chose != nil {
			*syn.chose = true
		}
		if syn.pick == pickLast {
			return closers[len(closers)-1], true
		}
	}
	return closers[0], true
}

const (
	pickFirst = iota
	pickLast
)

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
	dlen: func(b []byte) int {
		n := 0
		for i := 0; i < len(b); {
			if b[i] == '\\' && i+1 < len(b) {
				if b[i+1] == 'u' {
					if src, dec, ok := jsonUnicodeRun(b, i); ok {
						i, n = i+src, n+dec
						continue
					}
				}
				i, n = i+2, n+1
				continue
			}
			i, n = i+1, n+1
		}
		return n
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
// fixed by the string that encloses it, and it keeps that walk's one refusal: a
// header that commits and does not parse declines the field.
//
// It drops the two questions that only make sense at the top level. What
// *precedes* a value there says whether the value is the whole field; inside a
// string it is the field's own label, and `Obs: ` ending in a separator while
// `Åtgärd` does not decided whether the payload was seen at all. What *follows*
// a value there is residue; inside a string PHP has already answered it —
// unserialize stops at the first complete value and ignores the rest, so a blob
// with " (cachad)" after it is ordinary content, and declining on it made this
// walk disagree with the parser it exists to satisfy.

func repairNested(b []byte, rw func([]byte) []byte, depth int, syn syntax) (rep []byte, ok, decline bool) {
	var out []byte
	prev, found := 0, false
	for i := 0; i < len(b); {
		// Shape only. The position half asks what precedes the value, which
		// inside a string is the field's own label — and a label carries no
		// information about whether the payload after it is real.
		//
		// This was measured against a fuzz of *array* payloads once and looked
		// inert, so it was reverted. An array declares an arity, not a byte
		// count: skip its header and its members are still found, still
		// repaired, and the number that was never re-emitted was never wrong.
		// A string declares a byte count and has no child to fall back on, so
		// for `s:` this is the difference between a repair and a stale length.
		if !valueShape(b, i) {
			i++
			continue
		}
		r, end, parsed, committed, amb := tryBothPicks(b, i, rw, depth, syn, nil)
		if amb {
			return nil, false, true
		}
		if !committed {
			// Only a value with a length prefix has anything at stake here. A
			// bare scalar cannot hold a host and has no number to re-emit, so
			// finding one mid-string says nothing about the rest — and treating
			// it as a payload declined `s:12:"N;not really"`, an ordinary
			// twelve-byte string, taking the whole option down with it.
			i++
			continue
		}
		if !parsed {
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
	// See emitLen: measured where the spelling has one reading, and a delta in
	// source bytes where a character reference means it does not.
	var out []byte
	out = append(out, 's')
	out = syn.emit(out, ':')
	out = append(out, strconv.Itoa(emitLen(syn, n, data, repaired))...)
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
	end, cw, tw, ok := stringEnd(b, j+qw, n, syn)
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
	dataEnd, ok := spanEnd(b, dataAt, dataLen, syn, func(e int) bool {
		_, ok := syn.match(b, e, '}')
		return ok
	})
	if !ok {
		return nil, 0, false
	}
	clw, _ := syn.match(b, dataEnd, '}')
	data := b[dataAt:dataEnd]
	repaired := rw(data)
	if string(repaired) == string(data) {
		return b[i : dataEnd+clw], dataEnd + clw, true
	}
	out := append([]byte{}, b[i:nameEnd+cw]...)
	out = syn.emit(out, ':')
	out = append(out, strconv.Itoa(emitLen(syn, dataLen, data, repaired))...)
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
		for _, base := range []syntax{literalSyntax, percentSyntax, htmlSyntax, jsonSyntax, jsonHTMLSyntax} {
			_, e, o, c, amb := tryBothPicks(b, i, id, 0, base, nil)
			if o {
				end, ok = e, true
				break
			}
			// `amb` counts, like a failure to parse. Whether it *should* is
			// arguable — an undecidable reading is not a wrong length, and this
			// walk rewrites nothing — but every value reachable that way also
			// takes the `c` path, so dropping it changed no measurement, and an
			// unmeasurable change is not one to make.
			if c || amb {
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

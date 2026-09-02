package rewrite

import (
	"bytes"
	"sort"
	"strconv"
	"strings"
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

// UnreadSerialized reports whether b carries a serialized header that no
// spelling could read, in a buffer the rewrite changed.
//
// It is a page-level signal — "look at this one" — not a census, and it is
// deliberately that: what a developer needs is the page, and claiming a count
// of spans invites believing an arithmetic the evidence does not support.
//
// # Why this exists
//
// Enumerating the spellings does not terminate. Five real encoders give
// twenty-five ordered pairs and each one added multiplies them, so there will
// always be a composition the walk cannot read — and a value it cannot read
// still has its host rewritten with no length re-emitted, which is a value PHP
// refuses.
//
// `BrokenSerialized` cannot report those, for a structural reason rather than
// an oversight: it asks whether the served bytes parse, and a spelling this
// build cannot read does not parse on the canonical page either, so the corpus
// diff's baseline subtraction cancels it to zero. Four consecutive rounds of
// real corruption were reported GREEN that way. This asks instead whether the
// rewrite touched bytes it could not account for, which the canonical pass
// never does, so it survives the same subtraction.
//
// # Why the gate is readLen and not mayHoldSerialized
//
// The first version of this used mayHoldSerialized, whose own doc says it is
// deliberately wrong in one direction — it must never refuse a value the walk
// would repair, so it accepts anything shaped like a type letter, a colon and a
// digit. `border:1px` is `r`, `:`, `1`. So is `background:0 0`, `order:2`, and
// the `{a:1,b:2}` of any minified script. Every real WordPress page has inline
// CSS and was therefore red, and a check that is red on every page carries no
// information — so the false GREEN this was added to close stayed open.
//
// A gate built to over-accept cannot be the evidence for a red signal. readLen
// is the narrow one: it requires a complete `:<digits>:`, which none of those
// have, and which every serialized header in every spelling does.
//
// # Why it does not split fields
//
// The first version split on `&` with fieldBreak, which is the *form-body*
// splitter. An HTML body is not a form body, and the proxy runs
// RepairSerialized there, which does not split — so the metric measured a
// different buffer from the one the rewriter edited, and an ordinary `&` in a
// serialized string put the header and the host in different halves and hid
// them from each other.
func UnreadSerialized(b []byte, rw func([]byte) []byte) bool {
	// Where the rewrite actually landed, as a range in the original buffer: the
	// common prefix and the common suffix bound it from both ends, which works
	// even though the rewrite changes the length.
	out := rw(b)
	if bytes.Equal(out, b) {
		return false // nothing was touched, so nothing was touched unread
	}
	lo := 0
	for lo < len(b) && lo < len(out) && b[lo] == out[lo] {
		lo++
	}
	tail := 0
	for tail < len(b)-lo && tail < len(out)-lo && b[len(b)-1-tail] == out[len(out)-1-tail] {
		tail++
	}
	hi := len(b) - tail

	// The headers, so each one can be asked about its own bytes.
	//
	// The first version tested `rw(b) != b` over the whole page and then looked
	// for a header anywhere in it. Those are two independent facts, and the note
	// it raised — "a serialized value *here* was rewritten" — claimed they were
	// one: a `<link rel="canonical">` at the top of the page supplied the change
	// and any unreadable blob below it supplied the header, so a value the
	// rewrite never touched reddened the page. Every WordPress page has that
	// link.
	type head struct{ at, reach int }
	var heads []head
	for i := 0; i < len(b); i++ {
		if !valueShape(b, i) {
			continue
		}
		for _, syn := range []syntax{
			literalSyntax, percentSyntax, htmlSyntax, jsonSyntax,
			jsonHTMLSyntax, percentHTMLSyntax, percentJSONSyntax,
			htmlJSONSyntax, jsonDoubleSyntax,
		} {
			n, j, ok := readLen(b, i+1, syn)
			if !ok {
				continue
			}
			// How far the value reaches, under the *tightest* reading: its
			// declared length is in decoded bytes and every spelling here
			// spends at least one source byte on each, so the data covers at
			// least j+n.
			//
			// Deliberately the tight bound rather than the loose one. A loose
			// bound — twelve source bytes per decoded byte, which is what the
			// widest escape actually costs — reaches past a short value into
			// whatever follows, and a `<link rel="canonical">` after the blob
			// then counts as the blob's own change. That is the red-on-every-
			// page failure this signal has now had twice. Under-reaching costs
			// a report on a value whose host sits late in a heavily escaped
			// string; over-reaching costs the whole check.
			reach := j + n
			if reach > len(b) || reach < j {
				reach = len(b)
			}
			heads = append(heads, head{i, reach})
			break
		}
	}
	for _, h := range heads {
		i := h.at
		// If any spelling parses it completely, the walk read it and re-emitted
		// its length; nothing to report.
		if _, _, ok, _ := repairAt(b, i, func(x []byte) []byte { return x }, nil); ok {
			continue
		}
		// Its own bytes, from this header to wherever the next one starts. An
		// unreadable value has no end this can measure — that is what makes it
		// unreadable — so the next header is the boundary, which puts anything
		// else on the page outside it.
		// h.reach alone. Bounding by the next header as well was tried and no
		// measurement could tell the difference: j+n is already tighter than
		// the gap to the next header in every shape I could build, and a
		// second bound nothing can distinguish is a second thing to keep true.
		end := h.reach
		// Against the *whole* buffer's rewrite, compared by position, not by
		// rewriting the fragment. A fragment is not the document it came from —
		// a reference-encoded host in an attribute value decodes there and not
		// in a bare slice of the same bytes — so asking rw about a fragment
		// answers a question about a page that does not exist.
		if lo < end && hi > i {
			return true
		}
	}
	return false
}

// peelFormField takes one layer of form encoding off a field's value, repairs
// and rewrites what is underneath, and puts the layer back.
//
// A form encodes what it is given. When the page already held a percent-encoded
// origin — which the response direction happily rewrites, because
// `https%3A%2F%2Fh` is one of the three spellings §4.4 requires — the browser
// posts it back with the `%` itself encoded, as `https%253A%252F%252Fh`. No
// spelling in the table matches that, so the request direction could not take it
// back: a *variant* hostname reached the app and went into the shared database,
// which is §4.3's whole subject and has no undo. Round 63's daily audit read one
// out of a real database, and `check` and `diff` were both silent — `diff` has
// no request-direction assertion at all.
//
// Chasing spellings is the losing move here: a form can wrap a form. Peeling the
// one layer the content type *declares* is the fix, and the three spellings then
// handle what is inside.
//
// It splices rather than re-serialising, and that is the whole of why it is safe.
//
// Round 63 guarded the peel by requiring the value to re-encode byte-identically,
// first under Go's `url.QueryEscape` and then under the WHATWG *form-submission*
// serializer. Both are the wrong question, because there is no one encoder: a
// `<form>` POST, `URLSearchParams`, and jQuery's `encodeURIComponent` with
// `%20`→`+` — which is what WordPress core posts from `admin-ajax.php` and the
// Customizer — disagree on `!`, `'`, `(`, `)` and `~`. Every disagreement
// declined the peel whole, so the Customizer's `customized` field, which carries
// every setting in one value, leaked its variant hostname into the shared
// database the moment any setting held a paren or an apostrophe. `url()` in CSS
// always holds parens. Read back out of a real database, and served from the
// canonical hostname to everyone afterwards.
//
// So: decode, rewrite, and put back only the bytes that changed, keeping every
// other byte's original spelling exactly as the sender wrote it. No sender's
// encoder has to be guessed, and the replacement is a hostname — unreserved
// bytes that every one of those encoders spells the same way.
func peelFormField(b []byte, rw func([]byte) []byte) ([]byte, bool) {
	eq := bytes.IndexByte(b, '=')
	if eq < 0 || !bytes.ContainsRune(b[eq+1:], '%') {
		return nil, false
	}
	val := string(b[eq+1:])
	dec, spans, ok := formDecodeSpans(val)
	if !ok {
		return nil, false
	}
	rep := string(RepairSerialized([]byte(dec), rw))
	if rep == dec {
		return nil, false
	}
	// The changed range, as one span between the common prefix and suffix.
	// Everything outside it is copied through in the sender's own spelling.
	p := 0
	for p < len(dec) && p < len(rep) && dec[p] == rep[p] {
		p++
	}
	sfx := 0
	for sfx < len(dec)-p && sfx < len(rep)-p &&
		dec[len(dec)-1-sfx] == rep[len(rep)-1-sfx] {
		sfx++
	}
	out := append([]byte(nil), b[:eq+1]...)
	out = append(out, val[:spans[p]]...)
	out = append(out, formEncode(rep[p:len(rep)-sfx])...)
	return append(out, val[spans[len(dec)-sfx]:]...), true
}

// formDecodeSpans decodes an urlencoded value and records where each decoded
// byte came from, so a rewrite can be spliced back without re-encoding the rest.
//
// spans has one entry per decoded byte plus a terminator: spans[i] is the offset
// in val at which decoded byte i begins.
func formDecodeSpans(val string) (dec string, spans []int, ok bool) {
	var sb strings.Builder
	sb.Grow(len(val))
	spans = make([]int, 0, len(val)+1)
	for i := 0; i < len(val); {
		spans = append(spans, i)
		switch c := val[i]; {
		case c == '+':
			sb.WriteByte(' ')
			i++
		case c == '%':
			// An invalid escape is three literal bytes, which is what the
			// WHATWG parser and PHP both do with `50%25 off` written as
			// `50% off`. Declining the whole field instead — the first version
			// of this — meant a value with one stray `%` kept its variant
			// hostname all the way into the shared database, and a stray `%` in
			// a setting is ordinary.
			h, ok1 := hexAt(val, i+1)
			l, ok2 := hexAt(val, i+2)
			if !ok1 || !ok2 {
				sb.WriteByte('%')
				i++
				break
			}
			sb.WriteByte(h<<4 | l)
			i += 3
		default:
			sb.WriteByte(c)
			i++
		}
	}
	spans = append(spans, len(val))
	return sb.String(), spans, true
}

// hexAt reads one hex digit at i, reporting whether there is one there.
func hexAt(s string, i int) (byte, bool) {
	if i >= len(s) {
		return 0, false
	}
	return unhexDigit(s[i])
}

func unhexDigit(c byte) (byte, bool) {
	switch {
	case '0' <= c && c <= '9':
		return c - '0', true
	case 'a' <= c && c <= 'f':
		return c - 'a' + 10, true
	case 'A' <= c && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// formEncode is the WHATWG urlencoded serializer — what the browser that sent
// the body actually used.
//
// `url.QueryEscape` is not it. The two disagree on exactly two bytes and each
// disagreement declined a peel whole, sending the variant hostname upstream into
// the shared database: a browser leaves `*` raw where QueryEscape writes `%2A`,
// and writes `~` as `%7E` where QueryEscape leaves it raw. Both are ordinary in
// the bodies this exists for — they are CSS combinators, and `custom_css` is the
// option round 63's own comment names. Checked against
// `new URLSearchParams([["k","*~"]]).toString()`, which is `k=*%7E`.
func formEncode(s string) string {
	var sb strings.Builder
	sb.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ' ':
			sb.WriteByte('+')
		case c == '*' || c == '-' || c == '.' || c == '_' ||
			('0' <= c && c <= '9') || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z'):
			sb.WriteByte(c)
		default:
			const hex = "0123456789ABCDEF"
			sb.WriteByte('%')
			sb.WriteByte(hex[c>>4])
			sb.WriteByte(hex[c&0x0f])
		}
	}
	return sb.String()
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
		field := b[start:end]
		rep, ok := repairField(field, rw)
		// The peel runs on whatever the spellings left, and is withheld only from
		// a field where one of them *repaired a serialized length*.
		//
		// That is the round 44 hazard and the whole of it: `font-family:"Inter"`
		// inside a percent-encoded `custom_css` decodes to a value whose embedded
		// quotes are the delimiters' own byte, and re-walking it re-emits a length
		// six bytes short. Withholding on any change at all was wider than the
		// hazard and wrong: `?u=https%3A%2F%2Fh%2Fx` inside a URL that is itself
		// form-encoded — a share link, a redirect target, an `?ref=` — carries
		// one origin the spellings rewrite and a second one only the peel can
		// reach, and rewriting the first was what withheld the second. Measured
		// through a real wp-admin save: the variant hostname reached the shared
		// database. §4.3, no undo.
		if !ok {
			if prep, pok := peelFormField(rep, rw); pok {
				rep, ok = prep, true
			}
		}
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
//
// It asks parseURLRef, not refRun. They answer different questions and the
// difference is a leak: refRun asks what `esc_attr` writes — at most twelve
// bytes, semicolon required — because that is what a serialized *length* counts.
// This has to ask what a decoder will read a host through, and that decoder
// takes a named reference up to thirty-four bytes (the `&ZeroWidthSpace;`
// family) and a numeric one whose semicolon is optional, because browsers
// accept it. Splitting on a `&` the decoder would have consumed puts the break
// inside the hostname, and then no field contains it.
func fieldBreak(b []byte, start int) int {
	for i := start; i < len(b); i++ {
		if b[i] != '&' {
			continue
		}
		if _, w := parseURLRef(b[i:]); w > 0 {
			continue
		}
		return i
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
//
// In every spelling the transport may have applied, not only the literal one.
// A serialized value posted as a urlencoded field arrives with its trailing
// newline as `%0A`, which read as "something other than whitespace follows" — so
// the repair declined, the generic rewriter replaced the host anyway, and the
// field went into the shared database with a length describing the string it
// used to be. PHP returns false for that, or truncates it. A trailing byte the
// developer never typed decided which.
func onlySpaceAfter(b []byte, end int) bool {
	for i := end; i < len(b); i++ {
		switch b[i] {
		case ' ', '\t', '\r', '\n':
		// `+` is how form encoding spells a space, and `%XX` how it spells the
		// rest. Widening what counts as trailing whitespace cannot make a repair
		// wrong: repairAt has already parsed the value, so the length re-emitted
		// describes it either way, and anything after `end` is rewritten by the
		// generic path exactly as before.
		case '+':
		case '%':
			if i+2 < len(b) && pctWhitespace(b[i+1], b[i+2]) {
				i += 2
				continue
			}
			return false
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

// pctWhitespace reports whether %XY spells a whitespace byte: tab, LF, CR or
// space, in either hex case.
func pctWhitespace(x, y byte) bool {
	hex := func(c byte) int {
		switch {
		case c >= '0' && c <= '9':
			return int(c - '0')
		case c >= 'a' && c <= 'f':
			return int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			return int(c-'A') + 10
		}
		return -1
	}
	hi, lo := hex(x), hex(y)
	if hi < 0 || lo < 0 {
		return false
	}
	switch byte(hi<<4 | lo) {
	case '\t', '\n', '\r', ' ':
		return true
	}
	return false
}

// repairAt tries each spelling at i. committed reports whether the candidate got
// past its length and opening delimiter — far enough that it really is a
// serialized header and a failure to parse means the buffer is untrustworthy.
//
// Without that distinction every `https:` in a URL was a candidate whose length
// parse failed immediately, and "a failed candidate declines the buffer" then
// declined every body containing a link.
func repairAt(b []byte, i int, rw func([]byte) []byte, fieldOK func(end int) bool) (rep []byte, end int, ok, committed bool) {
	for _, base := range []syntax{literalSyntax, percentSyntax, htmlSyntax, jsonSyntax, jsonHTMLSyntax, percentHTMLSyntax, percentJSONSyntax, htmlJSONSyntax, jsonDoubleSyntax} {
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
	// re-emitted length is then a delta measured through `unit` instead.
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
// unitLen counts b through a spelling's own unit reader, charging each
// ambiguous run its escaped reading. See emitLen for why that is exact as a
// difference even though it is wrong as a total.
func unitLen(b []byte, unit func([]byte, int) (int, int, int)) int {
	n := 0
	for i := 0; i < len(b); {
		src, dec, _ := unit(b, i)
		if src == 0 {
			// Not measurable here; count the byte and move on rather than stop,
			// since this is only ever used as a difference.
			i, n = i+1, n+1
			continue
		}
		i, n = i+src, n+dec
	}
	return n
}

func emitLen(syn syntax, n int, data, repaired []byte) int {
	if syn.dlen != nil {
		return syn.dlen(repaired)
	}
	// A delta in *decoded* bytes, not source bytes.
	//
	// The source delta was right only where the spelling has no escape whose
	// width differs from what it decodes to. It is wrong the moment one does:
	// a percent escape is three source bytes for one decoded byte, so a map
	// that adds a port adds an escape and the source grows while the data
	// shrinks. That is what `dlen` was for, and the spellings without one —
	// the two that carry character references — took the source delta instead
	// and inherited the bug one spelling over.
	//
	// They have no `dlen` because a reference's width is genuinely ambiguous.
	// It does not need to be resolved here: a rewrite substitutes hostnames,
	// which contain no references, so both sides of the delta hold the same
	// ones. Counting each as one byte is wrong by the same amount on both
	// sides and cancels exactly, while every unambiguous layer is decoded
	// properly.
	return n + unitLen(repaired, syn.unit) - unitLen(data, syn.unit)
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
			if w := entityRun(b, i); w > 0 {
				return w, true
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
// jsonHexWidth is the width of a `\uXXXX` escape at i that decodes to c, or 0.
//
// JSON has two spellings for every character, and a delimiter written the long
// way is still that delimiter. WordPress core takes the long way on purpose:
// `wp_interactivity_data_wp_context()` encodes with
// JSON_HEX_TAG|JSON_HEX_APOS|JSON_HEX_QUOT|JSON_HEX_AMP, which is what lets a
// `data-wp-context` attribute be single-quoted with no second escaping pass —
// so every Interactivity API block writes `\u0022` where the walk looked only
// for `\"`. Reading one and not the other is a hole in the escaping axis, and a
// value nothing can read is one the host is rewritten in with no length
// re-emitted.
func jsonHexWidth(b []byte, i int, c byte) int {
	if i+6 > len(b) || b[i] != '\\' || b[i+1] != 'u' {
		return 0
	}
	v := 0
	for _, h := range b[i+2 : i+6] {
		d, ok := hexDigit(h)
		if !ok {
			return 0
		}
		v = v<<4 | d
	}
	if v != int(c) {
		return 0
	}
	return 6
}

var jsonSyntax = syntax{
	match: func(b []byte, i int, c byte) (int, bool) {
		if c == '"' && i+1 < len(b) && b[i] == '\\' && b[i+1] == '"' {
			return 2, true
		}
		if w := jsonHexWidth(b, i, c); w > 0 {
			return w, true
		}
		if c != '"' && i < len(b) && b[i] == c {
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
		if c == '"' && i+7 <= len(b) && b[i] == '\\' && string(b[i+1:i+7]) == "&quot;" {
			return 7, true
		}
		// The hex spelling passes through esc_attr untouched — it has no byte
		// esc_attr escapes — so it reaches this spelling as itself.
		if w := jsonHexWidth(b, i, c); w > 0 {
			return w, true
		}
		if c != '"' && i < len(b) && b[i] == c {
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

// pctEncodedWidth is the width at i of the percent-encoded spelling of s, where
// each byte may be literal or `%XX`. Zero when it is not there.
//
// Encoders disagree about what to escape — `rawurlencode` takes `&`, `#` and
// `;` and leaves the letters — so this accepts either form per byte rather than
// enumerating the products.
func pctEncodedWidth(b []byte, i int, s string) int {
	j := i
	for k := 0; k < len(s); k++ {
		switch {
		case j < len(b) && b[j] == s[k]:
			j++
		case pctIs(b, j, s[k]):
			j += 3
		default:
			return 0
		}
	}
	return j - i
}

// pctByte decodes one byte at i, literal or `%XX`, with its source width.
func pctByte(b []byte, i int) (byte, int) {
	if i >= len(b) {
		return 0, 0
	}
	if b[i] == '%' && i+2 < len(b) {
		if v, ok := unhex(b[i+1], b[i+2]); ok {
			return v, 3
		}
		return 0, 0
	}
	return b[i], 1
}

// percentJSONSyntax is a serialized value that was JSON-escaped and then
// percent-encoded: `s%3A5%3A%5C%22`.
//
// `JSON.parse(decodeURIComponent("…"))` is the shape the decoder view stack was
// built for — its own comment records fourteen canonical origins on one live
// /cart/ and eighteen in wp-admin — so the transport was known and the pairing
// with a serialized payload was not. percentSyntax wants `%22` where the JSON
// layer put `%5C%22`, and charges the `%5C` of every `\/` a byte `serialize`
// never counted; neither reads it, so the value was skipped while the percent
// view rewrote the host inside it.
//
// The JSON layer is below the percent layer, so a `\/` is one byte to the
// serializer and one byte here.
var percentJSONSyntax = syntax{
	match: func(b []byte, i int, c byte) (int, bool) {
		if c == '"' {
			if w := pctEncodedWidth(b, i, `\"`); w > 0 {
				return w, true
			}
			return 0, false
		}
		if v, w := pctByte(b, i); w > 0 && v == c {
			return w, true
		}
		return 0, false
	},
	emit: func(dst []byte, c byte) []byte {
		const hex = "0123456789ABCDEF"
		if c == '"' {
			return append(dst, "%5C%22"...)
		}
		return append(dst, '%', hex[c>>4], hex[c&0xf])
	},
	advance: func(b []byte, i, n int) (int, bool) {
		if n < 0 {
			return 0, false
		}
		for n > 0 {
			if i >= len(b) {
				return 0, false
			}
			src, dec, _ := percentJSONUnit(b, i)
			if src == 0 || dec > n {
				return 0, false
			}
			i, n = i+src, n-dec
		}
		return i, true
	},
	unit: percentJSONUnit,
}

// percentJSONUnit reads one decoded byte of the percent-over-JSON spelling.
func percentJSONUnit(b []byte, i int) (src, dec, alt int) {
	if w := pctEncodedWidth(b, i, `\u`); w > 0 {
		j, val := i+w, 0
		for k := 0; k < 4; k++ {
			c, cw := pctByte(b, j)
			d, ok := hexDigit(c)
			if cw == 0 || !ok {
				return 0, 0, 0
			}
			val, j = val<<4|d, j+cw
		}
		switch {
		case val >= 0xD800 && val <= 0xDBFF:
			// A surrogate pair: the low half follows in the same spelling.
			w2 := pctEncodedWidth(b, j, `\u`)
			if w2 == 0 {
				return 0, 0, 0
			}
			k2, lo := j+w2, 0
			for k := 0; k < 4; k++ {
				c, cw := pctByte(b, k2)
				d, ok := hexDigit(c)
				if cw == 0 || !ok {
					return 0, 0, 0
				}
				lo, k2 = lo<<4|d, k2+cw
			}
			if lo < 0xDC00 || lo > 0xDFFF {
				return 0, 0, 0
			}
			return k2 - i, 4, 0
		case val >= 0xDC00 && val <= 0xDFFF:
			return 0, 0, 0
		}
		if wl := utf8.RuneLen(rune(val)); wl > 0 {
			return j - i, wl, 0
		}
		return 0, 0, 0
	}
	if w := pctEncodedWidth(b, i, `\`); w > 0 {
		if _, cw := pctByte(b, i+w); cw > 0 {
			return w + cw, 1, 0
		}
		return 0, 0, 0
	}
	if _, w := pctByte(b, i); w > 0 {
		return w, 1, 0
	}
	return 0, 0, 0
}

// jsonDoubleSyntax is a serialized value JSON-encoded twice: a JSON document
// carried as a *string* inside another one, which is how a Gutenberg block
// attribute holds nested JSON.
//
// The second pass escapes the first pass's escapes, so every `\"` becomes
// `\\\"` and every `\/` becomes `\\\/` — four source bytes for one byte the
// serializer counted. It is the same encoder twice rather than two different
// ones, which is why it was missed: the product was enumerated over *kinds* of
// escaping and this cell is a kind crossed with itself.
var jsonDoubleSyntax = syntax{
	match: func(b []byte, i int, c byte) (int, bool) {
		if c == '"' {
			if i+4 <= len(b) && string(b[i:i+4]) == `\\\"` {
				return 4, true
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
			return append(dst, `\\\"`...)
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
			src, dec, _ := jsonDoubleUnit(b, i)
			if src == 0 || dec > n {
				return 0, false
			}
			i, n = i+src, n-dec
		}
		return i, true
	},
	unit: jsonDoubleUnit,
}

// jsonDoubleUnit reads one decoded byte of the twice-JSON-encoded spelling.
func jsonDoubleUnit(b []byte, i int) (src, dec, alt int) {
	if i+1 < len(b) && b[i] == '\\' && b[i+1] == '\\' {
		// `\\` then the inner pass's escape: `\\\"`, `\\\/`, or `\\u00e4`.
		if i+2 < len(b) && b[i+2] == '\\' {
			if i+3 < len(b) {
				return 4, 1, 0
			}
			return 0, 0, 0
		}
		if i+2 < len(b) && b[i+2] == 'u' {
			// The inner `\uXXXX` with its backslash doubled: measure the rune
			// from the same digits jsonUnicodeRun would read.
			if s, d, ok := jsonUnicodeRun(b[1:], i); ok {
				return s + 1, d, 0
			}
			return 0, 0, 0
		}
		return 2, 1, 0
	}
	if i < len(b) {
		return 1, 1, 0
	}
	return 0, 0, 0
}

// htmlJSONSyntax is a serialized value that was HTML-escaped and then
// JSON-encoded: `esc_attr` first, so the quotes are `&quot;`, and `json_encode`
// second, so every `/` is `\/`.
//
// It is the mirror of jsonHTMLSyntax, which is the same two encoders the other
// way round — and the order decides everything, because whichever ran last owns
// the quotes. A plugin that escapes on save and JSON-encodes on read produces
// this one; nothing read it, and a value nothing reads has its host rewritten
// with no length re-emitted.
var htmlJSONSyntax = syntax{
	match: func(b []byte, i int, c byte) (int, bool) {
		if c == '"' {
			if w := entityRun(b, i); w > 0 {
				return w, true
			}
			return 0, false
		}
		if w := jsonHexWidth(b, i, c); w > 0 {
			return w, true
		}
		if i < len(b) && b[i] == c {
			return 1, true
		}
		return 0, false
	},
	emit: func(dst []byte, c byte) []byte {
		if c == '"' {
			return append(dst, "&quot;"...)
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
			src, dec, _ := htmlJSONUnit(b, i)
			if src == 0 || dec > n {
				return 0, false
			}
			i, n = i+src, n-dec
		}
		return i, true
	},
	unit: htmlJSONUnit,
}

// htmlJSONUnit reads one decoded byte of the entity-then-JSON spelling: the
// JSON layer is on the outside, so its escapes are undone first.
func htmlJSONUnit(b []byte, i int) (src, dec, alt int) {
	if i < len(b) && b[i] == '\\' {
		if i+1 < len(b) && b[i+1] == 'u' {
			if s, d, ok := jsonUnicodeRun(b, i); ok {
				return s, d, 0
			}
			return 0, 0, 0
		}
		if i+1 < len(b) {
			return 2, 1, 0
		}
		return 0, 0, 0
	}
	// The quote spelling is unambiguous, for the reason htmlUnit gives: it is
	// the delimiter of the layer underneath, so a nested payload is made of
	// them and offering two readings each explodes the search.
	if w := entityRun(b, i); w > 0 {
		return w, 1, 0
	}
	if w := refRun(b, i); w > 0 {
		return w, 1, w
	}
	if i < len(b) {
		return 1, 1, 0
	}
	return 0, 0, 0
}

// percentHTMLSyntax is a serialized value that was HTML-escaped and then
// percent-encoded: `s%3A5%3A%26%2334%3B`.
//
// The other four spellings compose two layers at most, and this is the pair
// nothing covered. percentSyntax matches `%3A` for the colon and wants `%22`
// for the quote; htmlSyntax matches `&#34;` for the quote and wants a literal
// colon. Neither parses it, so the value was skipped — and a skip is not
// neutral: the fallback rewrites the host and re-emits no length. Confirmed on
// five surfaces through the add-on, in both directions, and `BrokenSerialized`
// walks the same list so it reported those pages GREEN.
//
// The escaping order is what fixes the counting: the entity layer is *below*
// the percent layer, so `serialize` counted the quote it wrote as one byte and
// this must too.
var percentHTMLSyntax = syntax{
	match: func(b []byte, i int, c byte) (int, bool) {
		if c == '"' {
			for _, e := range quoteEntities {
				if w := pctEncodedWidth(b, i, e); w > 0 {
					return w, true
				}
			}
			return 0, false
		}
		if pctIs(b, i, c) {
			return 3, true
		}
		return 0, false
	},
	emit: func(dst []byte, c byte) []byte {
		const hex = "0123456789ABCDEF"
		if c == '"' {
			return append(dst, "%26quot%3B"...)
		}
		return append(dst, '%', hex[c>>4], hex[c&0xf])
	},
	advance: func(b []byte, i, n int) (int, bool) {
		if n < 0 {
			return 0, false
		}
		for n > 0 {
			if i >= len(b) {
				return 0, false
			}
			src, dec, _ := percentHTMLUnit(b, i)
			if src == 0 || dec > n {
				return 0, false
			}
			i, n = i+src, n-dec
		}
		return i, true
	},
	unit: percentHTMLUnit,
}

// percentHTMLUnit reads one decoded byte of the percent-over-entity spelling.
func percentHTMLUnit(b []byte, i int) (src, dec, alt int) {
	// A reference first, in either spelling: it is one byte to the serializer
	// that wrote it, or its own literal bytes if the data already held it —
	// the same ambiguity htmlUnit carries, one layer down.
	for _, e := range quoteEntities {
		if w := pctEncodedWidth(b, i, e); w > 0 {
			return w, 1, 0
		}
	}
	// Any other reference, and it is percent-encoded too: `&amp;` reaches this
	// spelling as `%26amp%3B`. Asking refRun, which wants a literal `&`, read
	// those nine bytes as five separate ones — so every payload holding an
	// ampersand, an angle bracket or an apostrophe mis-measured, which is most
	// real WordPress content. Round 34 fixed this spelling for the quote and
	// left every other reference behind.
	if src, chars := pctRefRun(b, i); src > 0 {
		return src, 1, chars
	}
	if c, w := pctByte(b, i); w > 0 {
		_ = c
		return w, 1, 0
	}
	return 0, 0, 0
}

// pctRefRun measures a character reference whose own bytes are percent-encoded,
// returning its source width and how many characters it spells — which is the
// count if the data already held the reference rather than the character.
func pctRefRun(b []byte, i int) (src, chars int) {
	c, w := pctByte(b, i)
	if w == 0 || c != '&' {
		return 0, 0
	}
	j, n := i+w, 1
	for n <= 12 {
		c, w := pctByte(b, j)
		if w == 0 {
			return 0, 0
		}
		j, n = j+w, n+1
		if c == ';' {
			if n == 2 {
				return 0, 0
			}
			return j - i, n
		}
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' ||
			c >= '0' && c <= '9' || c == '#') {
			return 0, 0
		}
	}
	return 0, 0
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

// signWidth is the width of a `+` or `-` at j, in whatever spelling wrote it,
// or 0.
//
// Through syn.match, because `+` is the one byte of the scalar grammar a
// urlencoder touches. PHP writes any float from 1e17 up — and every integer
// past PHP_INT_MAX — as `d:1.0E+17;`, and `urlencode` makes that `+` into
// `%2B`. Read raw, the exponent's sign was invisible in the percent spelling:
// the scalar failed, its array failed with it, the field declined, and the
// generic rewrite then replaced the host and re-emitted no length. A settings
// page with one large number on it lost the option.
func signWidth(b []byte, j int, syn syntax) int {
	// Raw first: `-` is not in any urlencoder's escape set, so `d:1.0E-5;`
	// arrives with it literal even in the percent spelling. Only `+` is
	// escaped, and only because a form body already spends it on a space.
	if j < len(b) && (b[j] == '-' || b[j] == '+') {
		return 1
	}
	for _, c := range []byte{'-', '+'} {
		if w, ok := syn.match(b, j, c); ok {
			return w
		}
	}
	return 0
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
		j += signWidth(b, j, syn)
		for j < len(b) && b[j] >= '0' && b[j] <= '9' {
			j++
		}
	case 'd':
		// Digits, sign, decimal point, exponent — plus PHP's INF/-INF/NAN.
		if j+2 < len(b) && (string(b[j:j+3]) == "INF" || string(b[j:j+3]) == "NAN") {
			j += 3
		} else {
			j += signWidth(b, j, syn)
			if j+3 < len(b) && string(b[j:j+3]) == "INF" {
				j += 3
			} else {
				for j < len(b) {
					if c := b[j]; c >= '0' && c <= '9' || c == '.' || c == 'e' || c == 'E' {
						j++
						continue
					}
					if w := signWidth(b, j, syn); w > 0 {
						j += w
						continue
					}
					break
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
			// Declined, and this is a real cost, measured: a *legacy* nested
			// payload whose own length an earlier naive search-replace broke is
			// a value PHP accepts, because it only ever reads the outer length —
			// and declining leaves the outer stale too, so PHP then refuses the
			// whole option. An opaque broken string becomes a lost row.
			//
			// Skipping it instead was tried and is worse. It turns a decline
			// into repairing the *pieces* of a structure this walk does not
			// understand, which is what the file header describes destroying a
			// valid row: the inner spans still parse on their own, and their
			// numbers were already right for the original. It also makes the
			// depth limit unenforceable, since past it every value fails to
			// parse and would be skipped one level at a time.
			//
			// So the cost stands. It round-trips — the other direction declines
			// the same way — and `hostshift diff` reports it, because the outer
			// length is stale against bytes that grew.
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
	// Every delimiter comes back exactly as it arrived, and only the digits are
	// replaced. syn.emit writes one canonical spelling of each, which changed
	// bytes it had no reason to: a value delimited with `&#34;` came back
	// `&quot;`, one byte wider per quote with the same decoded content — and the
	// length here is a *source* delta in this spelling, so it counted every one
	// of those bytes and overshot by one per quote. Normalising `%3a` to `%3A`
	// in the percent spelling was the same thing, one surface over.
	c1w, _ := syn.match(b, i+1, ':')
	dstart := i + 1 + c1w
	dend := dstart
	for dend < len(b) && b[dend] >= '0' && b[dend] <= '9' {
		dend++
	}
	var out []byte
	out = append(out, b[i:dstart]...) // the type letter and its colon
	out = append(out, strconv.Itoa(emitLen(syn, n, data, repaired))...)
	out = append(out, b[dend:j+qw]...) // the second colon and the opening quote
	out = append(out, repaired...)
	out = append(out, b[dataEnd:end]...) // the closing quote and the terminator
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
// reason, so the two directions stay consistent.
//
// **The round trip is exact only when the substitution is byte-symmetric**, and
// one ordinary case is not. The matcher deliberately matches *both* schemes for
// every canonical host and replaces with the variant's declared origin — M0
// measured one fleet host appearing 165 times over http and zero over https —
// so `http://canonical` goes out as `https://variant` and comes back as
// `https://canonical`, one byte longer than it left. A declined value then
// returns to the database under a length that no longer describes it, and PHP
// refuses it. The served page is RED in the detector, but the request direction
// is scored by nothing.
//
// Removing the decline fixes that case and reintroduces the one this check
// exists for: with it off, `TestAnOrdinaryCustomCSSOptionSurvivesARoundTrip`
// and `TestTheResidueRulesBlindSpotComesHome` both fail — the destroyed
// `wp_options` row of §4.3. Measured, not assumed: every PHP corpus sweep
// scores identically either way, and those two tests are the whole difference.
// So the cost stands, and it is stated here rather than promised away.
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
		pctQuote
	)
	open := ownField
	for start > 0 {
		c := b[start-1]
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
			start--
			continue
		}
		// The same whitespace in the spelling a form sends, which is the mirror
		// of the trailing scan below.
		//
		// Only the raw bytes were skipped here, and no browser sends those: a
		// `application/x-www-form-urlencoded` body spells a leading newline
		// `%0D%0A` and a leading space `+` or `%20`. So an option whose value
		// begins with a newline — one edited in a `<textarea>`, which this file
		// already notes is how they get there — failed the "occupies its field"
		// gate on the way back in, the repair declined, and the generic rewriter
		// replaced the host and left the old length. `s:35:` over 30 bytes,
		// written to the shared database, and PHP returns false for the row.
		//
		// The trailing side of this same function was widened for exactly these
		// spellings two rounds ago; the leading side was not, and the two have to
		// agree about what whitespace is.
		if c == '+' {
			start--
			continue
		}
		if start >= 3 && b[start-3] == '%' && pctWhitespace(b[start-2], b[start-1]) {
			start -= 3
			continue
		}
		switch {
		// The percent-encoded delimiters, before the raw ones, because the byte
		// at start-1 is then a hex digit and every raw case below would miss it.
		// A value that arrived percent-encoded sits inside a field whose quotes
		// are `%22`, and comparing raw bytes declined every one of them — which
		// is not neutral, so the whole percent-over-JSON shape was served with
		// the host rewritten and no length re-emitted.
		case pctIs(b, start-3, '"'):
			open = pctQuote
			start -= 2
		case pctIs(b, start-3, '\''):
			open = pctQuote
			start -= 2
		case pctIs(b, start-3, '&'), pctIs(b, start-3, '='):
			start -= 2
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
		// The same whitespace in the spelling its transport uses. A serialized
		// value posted as a urlencoded field carries its trailing newline as
		// `%0A`, and reading that as "something else follows" made this return
		// false — so the repair declined, the generic rewriter replaced the host
		// anyway, and the field went to the database with a length describing
		// the string it used to be.
		//
		// `ownField` only, and the exclusion of `pctQuote` is the whole point.
		// Inside a `%22`-quoted field the value's own quotes are `%22` too, and
		// the residue of a parse that stopped short inside a string is the tail
		// of the true string — for `custom_css` that tail is `%0A%0A%0A%22%3B%7D`.
		// Skipping those newlines as whitespace walks the scan onto the `%22`
		// and lets the `pctQuote` arm accept it as the field's closing quote,
		// which is §4.3's custom_css truncation exactly: `s:89:` describing 89
		// bytes of a 95-byte option, correct by its own length and so invisible
		// to `broken`, with the Customizer re-serialising the loss back into the
		// shared database. The defect this skip exists for was `ownField` alone.
		if open == ownField {
			if b[i] == '+' {
				continue
			}
			if b[i] == '%' && i+2 < len(b) && pctWhitespace(b[i+1], b[i+2]) {
				i += 2
				continue
			}
		}
		switch open {
		case ownField:
			// Its own field: only the next separator may follow, so a stray
			// quote is residue rather than a close.
			return b[i] == '&'
		case pctQuote:
			return pctIs(b, i, '"') || pctIs(b, i, '\'')
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
		for _, base := range []syntax{literalSyntax, percentSyntax, htmlSyntax, jsonSyntax, jsonHTMLSyntax, percentHTMLSyntax, percentJSONSyntax, htmlJSONSyntax, jsonDoubleSyntax} {
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
			// Past this header, so the same header is not counted twice. An
			// *enclosing* header still fails and still counts, which is why the
			// doc comment above calls this a detector rather than a census.
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
	for _, e := range quoteEntities {
		if i+len(e) <= len(b) && string(b[i:i+len(e)]) == e {
			return len(e)
		}
	}
	return 0
}

// quoteEntities is every spelling of `"` a serialiser or a template engine
// writes. The hex forms were missing, so a value delimited with `&#x22;` was
// not a value at all to this walk: it declined, and a decline rewrites the host
// and re-emits nothing. WordPress core writes `&quot;`, but a theme or a
// JS-side escaper reaching for `&#x22;` is not exotic.
var quoteEntities = []string{"&quot;", "&#34;", "&#034;", "&#x22;", "&#X22;", "&#x022;"}

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

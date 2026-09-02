package rewrite

import (
	"bytes"
	"sort"
	"strings"
	"sync"

	"github.com/generoi/hostshift/internal/origin"
)

// The second gap between the matcher's model and the browser's: the matcher
// models *bytes*, and the browser runs a URL parser over them first.
//
// §5.3's three encodings all assume the origin is a contiguous run reading
// `scheme` `://` `host`. The WHATWG URL parser requires none of that, and every
// shape below resolves to https://www.example.fi/x in a browser — verified
// against ada, the parser Chrome ships — while matching no pattern the scan can
// see. Worse, it matched nothing the *census* could see either: `--json`
// reported zero candidates and zero skips, and the straggler sweep runs through
// the same matcher, so nothing anywhere said the page was worth a second look.
//
//	https://www.example<TAB>.fi/x     tab, LF, CR anywhere, raw or as a reference
//	https:\\www.example.fi/x          and /\, \/, ///, ////, //\ …
//	http:www.example.fi/x             a *different* scheme needs no slashes at all
//	<SPACE>https:\\www.example.fi/x   leading C0 and spaces are stripped first
//	https://user@www.example.fi/x     userinfo pushes the host off the separator
//	https://www.ex%61mple.fi/x        the host is percent-decoded before lookup
//
// The first version of this file enumerated shapes and normalised them. That is
// the wrong shape of solution — it closed the five cases it knew and left five
// more, because a rule of the form "a run of two or more" is a guess at where
// the authority begins. This one *locates* the authority the way the parser
// locates it, then replaces the host and nothing else.
//
// Replacing only the host byte range matters. The value is not re-serialised, so
// a query string, a fragment, an unusual separator and any whitespace outside
// the host all survive exactly as written; the only bytes that change are the
// ones naming the origin, which is the same contract as §5.2's ordinary splice.

// hostReplacer maps a canonical host to the variant host that replaces it.
//
// Built from the matcher's pairs rather than from its patterns: this pass needs
// a lookup keyed by the *parsed* host, which is a different question from the
// anchored byte scan the patterns answer.
type hostReplacer struct {
	// collect, when non-nil, turns every pass into a scan: hosts are recorded
	// and nothing is rewritten. See locateHostIn.
	//
	// Keyed by host and then by the *source* offset the host starts at, because
	// a dozen views run over one buffer and several of them find the same URL.
	// Counting each hit incremented every host once per view that fired, so one
	// `wp_json_encode`-escaped URL anywhere in the page — which is every
	// WordPress page — multiplied the whole census. Six SVG namespace
	// declarations were reported as "24 links to www.w3.org", and a threshold
	// meaning five links fired at two.
	collect map[string]map[int]struct{}

	to map[string]origin.Origin
	// schemes is the set of schemes the variants are served on. The document's
	// own scheme decides whether a reference with a scheme and no slashes is an
	// authority or a path, and the document is served at a variant origin.
	schemes map[string]bool
	// schemeList is schemes in a fixed order, so nothing here depends on Go's
	// randomised map iteration.
	schemeList []string
}

// tableKey is how a parsed host is looked up: unbracketed, because that is what
// hostReplacer.key produces and what origins store — HostPort() brackets an IPv6
// literal for *rendering*, which is the opposite of what a lookup wants.
func tableKey(o origin.Origin) string {
	if o.Port == "" {
		return o.Host
	}
	return o.Host + ":" + o.Port
}

func newHostReplacer(m *origin.Matcher) *hostReplacer {
	h := &hostReplacer{to: map[string]origin.Origin{}, schemes: map[string]bool{}}
	for _, p := range m.Pairs() {
		if p.Identity() {
			continue
		}
		h.to[tableKey(p.Canonical)] = p.Variant
		h.schemes[p.Variant.Scheme] = true
	}
	for s := range h.schemes {
		h.schemeList = append(h.schemeList, s)
	}
	sort.Strings(h.schemeList)
	return h
}

// key normalises a parsed host to the form the table is keyed on: lowercase,
// no root dot, and the browser's domain-to-ASCII.
//
// origin.HostFold, not a bare punycode: the browser runs UTS46 mapping first, so
// a soft hyphen in the host, fullwidth letters, U+3002 as a label separator or
// an NFD spelling all name the canonical host to a browser while sharing no
// bytes with it. §5.5 calls IDN real for .fi client domains, and NFD is what a
// macOS filesystem or a paste produces without anyone trying.
// active reports whether a pass has anything to do: a map to rewrite with, or a
// collection to fill. Every view guards on this, so a scan reaches all of them.
func (h *hostReplacer) active() bool {
	return h != nil && (len(h.to) > 0 || h.collect != nil)
}

func (h *hostReplacer) key(b []byte) string {
	s := strings.ToLower(string(b))
	// Origins store an IPv6 literal unbracketed — url.Hostname() strips them —
	// so the lookup key has to match that.
	if len(s) > 1 && s[0] == '[' && s[len(s)-1] == ']' {
		return s[1 : len(s)-1]
	}
	if a, err := origin.HostFold(s); err == nil {
		s = a
	}
	// After the fold, not before. UTS46 *produces* an ASCII root dot from
	// U+3002, U+FF0E and U+FF61, so trimming first left `www。example。fi。`
	// folding to `www.example.fi.` against a table keyed `www.example.fi` — a
	// miss, and a dereferenceable production origin in a plain `<a href>` with
	// no userinfo, no odd slashes and no encoding trick, on every surface and
	// every content type.
	return strings.TrimSuffix(s, ".")
}

func isURLStripped(c byte) bool { return c == '\t' || c == '\n' || c == '\r' }

func isSlashish(c byte) bool { return c == '/' || c == '\\' }

// isAuthorityByte reports whether c can appear inside an authority — the host,
// its userinfo, or its port. Deliberately generous: anything non-ASCII is a
// possible IDN label, and `%` a possible escape. What it excludes is what ends
// an authority in every context the rewriter sees one: a quote, a bracket,
// whitespace, a comma, a semicolon.
func isAuthorityByte(c byte) bool {
	// The host-byte half comes from origin, not from a second copy of it. The
	// two tables answer different questions — this one also has to admit the
	// authority's own structure — but they must not disagree about what a *host*
	// byte is, and they did: round 54 taught delimAt that `&` continues a host
	// and left this stopping there, so `<p>https://www.example.fi&period;x</p>`
	// was declined by the matcher and rewritten by the locator one pass later.
	// A browser reads that as www.example.fi.x, and what went out was a variant
	// hostname glued to `&period;x` that the request direction cannot read back.
	if origin.IsHostByte(c) {
		return true
	}
	switch c {
	// Authority structure. `~` and `+` predate both tables and are wider than
	// origin's host set; they are kept because narrowing the scan is a separate
	// change with its own leak risk, and named here so the difference is a
	// decision rather than a drift.
	//
	// `&` is deliberately absent, and belongs to hostRange's end scan instead:
	// it continues a host that has already started and does not begin one.
	// Admitting it here removed the left anchor, so `&https:\\www.example.fi/x`
	// stopped being a candidate at all and the origin survived — a boundary this
	// file's own TestBoundariesAreNotAnAllowlist exists to hold.
	case '~', '%', '+', '@', ':', '[', ']':
		return true
	}
	return false
}

// normalised is v with the bytes the URL parser removes taken out, and a map
// back to where each surviving byte came from.
type normalised struct {
	b   []byte
	pos []int // pos[i] is where b[i] came from in the original
	end []int // end[i] is one past the *end* of what b[i] came from
}

// stripForURL removes what the parser removes before it parses: leading and
// trailing C0 controls and spaces, then tab, LF and CR wherever they appear.
//
// The character-reference spellings of those three go too. They are handled here
// rather than in decodeURLRefs because that decoder must never *emit* a control
// character — doing so was one of the XSS holes this file sits next to. Removing
// one is not emitting one.
func stripForURL(v []byte) normalised {
	lo, hi := 0, len(v)
	for lo < hi && v[lo] <= 0x20 {
		lo++
	}
	for hi > lo && v[hi-1] <= 0x20 {
		hi--
	}
	n := normalised{b: make([]byte, 0, hi-lo), pos: make([]int, 0, hi-lo), end: make([]int, 0, hi-lo)}
	for i := lo; i < hi; {
		if isURLStripped(v[i]) {
			i++
			continue
		}
		if v[i] == '&' {
			if k := removableRef(v[i:]); k > 0 {
				i += k
				continue
			}
		}
		n.b = append(n.b, v[i])
		n.pos = append(n.pos, i)
		n.end = append(n.end, i+1)
		i++
	}
	return n
}

// stripForRefs is stripForURL with character references decoded into the view.
//
// Decoding belongs where the *consuming* parser decodes, not where HTML does.
// decodeURLRefs runs on HTML attribute values only, and its comment justifies
// that with "inside <script> and <style> the browser does not decode references"
// — true of HTML, false of XML. In XHTML the XML parser decodes them inside
// script and style (which is why XHTML scripts need CDATA), and in the XML
// family — SVG especially — it decodes them everywhere. Both were confirmed
// dereferenced by a real browser.
//
// Nothing is re-serialised: the decoded bytes exist only in the view, and the
// splice replaces the host's original byte range, so `&#47;&#47;` survives
// untouched beside a rewritten host and an XML document keeps its `&amp;`.
func stripForRefs(v []byte) normalised {
	if bytes.IndexByte(v, '&') < 0 {
		return stripForURL(v)
	}
	dec := make([]byte, 0, len(v))
	pos := make([]int, 0, len(v))
	end := make([]int, 0, len(v))
	for i := 0; i < len(v); {
		if v[i] == '&' {
			if c, n := parseURLRef(v[i:]); n > 0 {
				for k := 0; k < len(c); k++ {
					dec = append(dec, c[k])
					pos = append(pos, i)
					end = append(end, i+n)
				}
				i += n
				continue
			}
		}
		dec = append(dec, v[i])
		pos = append(pos, i)
		end = append(end, i+1)
		i++
	}
	n := stripRemovals(dec, pos, end)
	return n
}

// stripForJSONEsc is stripForURL with JSON string escapes decoded into the view.
//
// The producer that makes this necessary is WordPress core, and what it escapes
// is not the slash. Gutenberg's block serializer — `serialize_block_attributes()`
// in wp-includes/blocks.php, and `serializeAttributes()` in @wordpress/blocks —
// puts every block's attributes through
//
//	.replace(/--/g, '\\u002d\\u002d')   // "Don't break HTML comments."
//
// before writing them into the `<!-- wp:… -->` delimiter. **Every variant
// hostname this tool can generate contains `--` by construction**, because
// DefaultVariantPattern is `{slug}--{leftmost-label}`. So the editor re-saves a
// block whose URL we rewrote, emits `wt-a\u002d\u002dacme.ddev.site`, and the
// reverse pass cannot read its own hostname back: the variant goes upstream into
// the shared production database, where `parse_blocks` decodes it and production
// serves a `.ddev.site` URL to real visitors. §4.3's failure, on ordinary use of
// the block editor, with no undo.
//
// The forward direction has the same shape whenever the canonical is an IDN,
// because punycode's ACE prefix is literally `xn--`: `xn\u002d\u002dhmeen-gra.fi`
// went out byte-identical, which is test 28.
//
// Only escapes landing in printable ASCII are decoded. The rule stripForURL's
// comment sets — a decoder must never *emit* a control character — applies here
// exactly, and it also keeps the view from inventing a byte the authority
// scanner would read as a separator. Surrogate pairs and everything above 0x7E
// are left as written, because no authority byte lives there.
func stripForJSONEsc(v []byte) normalised { return stripJSONEsc(v, false) }

// stripForJSONEscCtl is stripForJSONEsc for a surface whose escapes are real: it
// also removes `\t`, `\n`, `\r` and their `\uXXXX` spellings, because the string
// decoder turns them into the characters the URL parser then deletes.
// `<script>fetch("https://www.example\t.fi/p")</script>` is the host
// www.example.fi to a browser, and refusing the escape left a live production
// origin in the preview.
//
// Two views rather than one rule, because whether the escape is an escape is the
// surface's answer and nothing below this line can see it. In an `href` a
// backslash is a path separator: `https://www.example\u0009.fi/x` is the host
// www.example with the path `/u0009.fi/x`, and folding it removed six bytes from
// a value nothing should have touched. See origin.SurfaceDecodesEscapes.
func stripForJSONEscCtl(v []byte) normalised { return stripJSONEsc(v, true) }

// escView picks between them.
func escView(surface string) func([]byte) normalised {
	if origin.SurfaceDecodesEscapes(surface) {
		return stripForJSONEscCtl
	}
	return stripForJSONEsc
}

// hasEsc is hasJSONEsc widened for the ctl view, whose two-byte spellings
// (`\t`) carry no `\u` for the cheap gate to find.
func hasEsc(v []byte, surface string) bool {
	if hasJSONEsc(v) {
		return true
	}
	return origin.SurfaceDecodesEscapes(surface) && bytes.IndexByte(v, '\\') >= 0
}

func stripJSONEsc(v []byte, ctl bool) normalised {
	// The ctl view also decodes two-byte spellings, which carry no `\u` for
	// hasJSONEsc's cheap gate to find.
	if !hasJSONEsc(v) && !(ctl && bytes.IndexByte(v, '\\') >= 0) {
		return stripForURL(v)
	}
	dec := make([]byte, 0, len(v))
	pos := make([]int, 0, len(v))
	end := make([]int, 0, len(v))
	for i := 0; i < len(v); {
		if v[i] == '\\' && i+1 < len(v) {
			// The letter spellings of the three the URL parser removes. Same
			// rule as the `\u0009` form below: removed from the view, so the
			// host reads across them exactly as a browser reads it.
			if ctl {
				switch v[i+1] {
				case 't', 'n', 'r':
					i += 2
					continue
				}
			}
			// `\/` is already handled by the JSON surface, but it reaches here
			// too on the composed views, and decoding it costs nothing.
			if v[i+1] == '/' {
				dec = append(dec, '/')
				pos = append(pos, i)
				end = append(end, i+2)
				i += 2
				continue
			}
			// `\xNN` and `\u{...}` are the other two spellings of the same byte
			// that a *JavaScript* string may carry, and an inline `<script>` is
			// Tier 1. `\xe4` in particular is what a minifier run with
			// `ascii_only` writes for every byte above 0x7E — the same IDN
			// authority the `\u00e4` case was widened for, one member over.
			if r, w, ok := jsEscAt(v[i:]); ok && ctl {
				for k := 0; k < len(r); k++ {
					dec = append(dec, r[k])
					pos = append(pos, i)
					end = append(end, i+w)
				}
				i += w
				continue
			}
			if v[i+1] == 'u' && i+6 <= len(v) {
				if r, ok := jsonEscRune(v[i+2:i+6], ctl); ok {
					// Every byte of the rune points at the whole escape, the way
					// stripForRefs already maps a multi-byte named reference, so
					// a splice that lands anywhere inside it replaces all six
					// source bytes.
					for k := 0; k < len(r); k++ {
						dec = append(dec, r[k])
						pos = append(pos, i)
						end = append(end, i+6)
					}
					i += 6
					continue
				}
			}
		}
		dec = append(dec, v[i])
		pos = append(pos, i)
		end = append(end, i+1)
		i++
	}
	return stripRemovals(dec, pos, end)
}

// hasJSONEsc reports whether the buffer holds an escape this view decodes.
//
// A two-byte needle, not "contains a backslash". Every view in this family
// builds three slices the length of the body, and TestAllocationStaysBounded
// measures the peak for one 8 MiB request — gating on the backslash alone made
// this fire on every CSS escape in the document and took the composite from 200x
// the body to 304x. `\\/` is left to the CSS view, which decodes it already.
func hasJSONEsc(v []byte) bool {
	if bytes.Contains(v, []byte(`\u`)) || bytes.Contains(v, []byte(`\x`)) {
		return true
	}
	return false
}

// hasRefJSONEsc reports whether the buffer could hold a JSON escape once
// character references are decoded — the backslash spelled either way, followed
// by the `u`.
//
// Asked of the decoder, not of a list of spellings. The first version named six
// fixed strings, and `parseURLRef` accepts more ways to write that backslash than
// six: `&#092;` with a leading zero, `&bsol;` (in this package's own table,
// annotated as the JSON separator's byte), and `&#92` with no terminating
// semicolon, which browsers accept and so does the decoder. Each one the gate
// refused is a spelling the view behind it would have read, and on the request
// path that is the variant hostname going upstream into the shared database.
//
// A gate narrower than the thing it guards is the same defect as a needle list
// narrower than a family, one level up. It allocates nothing, so the
// ampersand-only allocation case stays where it is rather than paying the 185x
// a bare `&` gate costs.
func hasRefJSONEsc(v []byte) bool {
	if hasJSONEsc(v) {
		return true
	}
	for i := 0; i < len(v); i++ {
		if v[i] != '&' {
			continue
		}
		c, n := parseURLRef(v[i:])
		if n == 0 || len(c) != 1 || c[0] != '\\' {
			continue
		}
		if i+n >= len(v) {
			continue
		}
		if v[i+n] == 'u' {
			return true
		}
		if v[i+n] == '&' {
			if c2, n2 := parseURLRef(v[i+n:]); n2 > 0 && len(c2) == 1 && c2[0] == 'u' {
				return true
			}
		}
	}
	return false
}

// jsEscAt decodes `\xNN` or `\u{...}` at the start of b, returning the bytes and
// the width consumed.
//
// Two of JavaScript's four string escapes, and deliberately not the other two.
//
// Legacy octal and the line continuation were added and then taken out again,
// for two reasons that turned out to be the same reason. Both are `\` followed
// by something ordinary — a digit, a newline — so neither has a two-byte needle
// to gate on, and a scan for them armed this whole view on every CSS escape in
// the document: `\3a` is a colon, `\2014` a dash, and a page with one measured
// 287x the body against a 128x fixture that does not look at this shape.
//
// And the line continuation is wrong outside a JS string. These views run on
// every surface, and in an HTML attribute a backslash is a `/` to the URL
// parser, so removing `\<LF>` from inside `www.example.f\<LF>i` invents a host
// the browser never resolves and rewrites bytes that were not an origin.
//
// `\xNN` and `\u{...}` have neither problem: each decodes to a byte in place,
// and each has a two-byte needle. `\xNN` also has a producer — a minifier run
// with `ascii_only` writes every byte above 0x7E that way, which is the same IDN
// authority `\u00e4` was widened for.
func jsEscAt(b []byte) ([]byte, int, bool) {
	if len(b) < 4 || b[0] != '\\' {
		return nil, 0, false
	}
	if b[1] == 'x' {
		r, ok := jsonEscRune(append([]byte("00"), b[2:4]...), true)
		if !ok {
			return nil, 0, false
		}
		return r, 4, true
	}
	// Legacy octal, `\NNN`, one to three digits 0-7 — decoded when the view is
	// already being built, never gated on. `hasJSONEsc` deliberately does not
	// arm for this: `\` before a digit is a CSS escape far more often than a JS
	// one, and scanning for it took an ordinary themed page from 118x the body
	// to 287x. But a body that already contains `\u` or `\x` — which is most
	// WordPress inline script — has the view built anyway, and reading `\056`
	// there costs nothing. A purely-octal body stays unread, which is the price
	// of not arming.
	if b[1] >= '0' && b[1] <= '7' {
		w, val := 1, 0
		for w < 4 && w < len(b) && b[w] >= '0' && b[w] <= '7' {
			val = val<<3 | int(b[w]-'0')
			w++
		}
		if val < 0x20 || val > 0x7E {
			return nil, 0, false
		}
		return []byte{byte(val)}, w, true
	}
	if b[1] != 'u' || b[2] != '{' {
		return nil, 0, false
	}
	// `\u{...}`: up to six hex digits, then a closing brace.
	for w := 4; w < len(b) && w <= 10; w++ {
		if b[w] != '}' {
			continue
		}
		h := b[3:w]
		if len(h) == 0 || len(h) > 6 {
			return nil, 0, false
		}
		padded := append(make([]byte, 0, 4), h...)
		for len(padded) < 4 {
			padded = append([]byte{'0'}, padded...)
		}
		if len(padded) > 4 {
			return nil, 0, false
		}
		r, ok := jsonEscRune(padded, true)
		if !ok {
			return nil, 0, false
		}
		return r, w + 1, true
	}
	return nil, 0, false
}

// jsonEscRune decodes the four hex digits of a `\uXXXX` escape into the bytes
// it stands for, or reports false.
//
// The lower bound is the rule stripForURL's comment sets: a decoder must never
// *emit* a control character, and here it would also invent a byte the authority
// scanner reads as a separator, so the scan would locate a host across a break
// that is not in the document.
//
// There is no upper bound, and the one that was here — `> 0x7E`, "no authority
// byte lives there" — was simply wrong. An IDN authority is made of exactly
// those bytes, `wp_json_encode` writes them as `\u00e4`, and §M4 lists that
// spelling among the three it calls a dereferenceable production origin reaching
// the browser. The blob it lives in is `wp_localize_script`'s, which is on
// essentially every WordPress page, and the JS parser decodes it before the
// browser dereferences it — so the developer's own session posts to the client's
// live admin-ajax.
//
// Surrogates stay refused: a lone one is not a character, and a pair needs the
// second escape, which this function does not see. A canonical host reaches this
// package already folded to punycode, and the fold happens on the decoded bytes,
// so the UTF-8 spelling is what has to arrive here.
func jsonEscRune(h []byte, ctl bool) ([]byte, bool) {
	if len(h) != 4 {
		return nil, false
	}
	n := 0
	for _, c := range h {
		switch {
		case c >= '0' && c <= '9':
			n = n<<4 | int(c-'0')
		case c >= 'a' && c <= 'f':
			n = n<<4 | int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			n = n<<4 | int(c-'A') + 10
		default:
			return nil, false
		}
	}
	switch n {
	case 0x09, 0x0A, 0x0D:
		if !ctl {
			// Not an escape on this surface: a backslash in an `href` or a
			// header is a path separator, so these six bytes are ordinary path
			// characters. Removing them there deleted bytes from a value that
			// names a different host entirely.
			return nil, false
		}
		// Removed by the URL parser, so removed from the view: the host reads
		// across them. `https://www.example\t.fi/p` is the host `www.example.fi`
		// to a browser, and refusing the escape left it unrewritten — a live
		// production origin in the preview. Removing is not emitting, which is
		// the rule stripForURL states for the literal spellings.
		return []byte{}, true
	}
	if n < 0x20 || n == 0x7F {
		return nil, false
	}
	if n >= 0xD800 && n <= 0xDFFF {
		return nil, false
	}
	return []byte(string(rune(n))), true
}

// stripForPercent is stripForURL with percent-escapes decoded into the view.
//
// The engine models three encodings and handles each in isolation; it did not
// handle them *composed*. WooCommerce emits
// `JSON.parse(decodeURIComponent("…"))` blobs inline, and percent-encoding a
// JSON-escaped URL gives `https%3A%5C%2F%5C%2Fhost` — one `%5C%2F` per `\/`.
// Measured on a live store, a logged-in /cart/ carried fourteen canonical
// origins that way and wp-admin eighteen, with `--json` reporting zero
// candidates and zero skips, and `diff` printing GREEN.
//
// Decoding into the view is enough: `\/\/` is a run of four slash-ish bytes, so
// the locator finds the authority once the escapes are gone, and the splice
// replaces only the host's original byte range — the percent-encoding around it
// survives exactly as written.
func stripForPercent(v []byte) normalised {
	if bytes.IndexByte(v, '%') < 0 {
		return stripForURL(v)
	}
	dec := make([]byte, 0, len(v))
	pos := make([]int, 0, len(v))
	end := make([]int, 0, len(v))
	for i := 0; i < len(v); {
		if v[i] == '%' && i+2 < len(v) {
			hi, ok1 := digitVal(v[i+1], 16)
			lo, ok2 := digitVal(v[i+2], 16)
			if ok1 && ok2 {
				dec = append(dec, byte(hi*16+lo))
				pos = append(pos, i)
				end = append(end, i+3)
				i += 3
				continue
			}
		}
		dec = append(dec, v[i])
		pos = append(pos, i)
		end = append(end, i+1)
		i++
	}
	n := stripRemovals(dec, pos, end)
	return n
}

// stripForCSS is stripForURL with CSS escapes decoded first.
//
// `https\3a\2f\2fwww.example.fi/x` is a CSS-level spelling of an absolute URL:
// the CSS tokenizer unescapes it *before* anything sees a URL, so the locator —
// which models the URL parser and nothing else — cannot reach it by
// construction, and the byte matcher sees no `://` at all. Measured in Chrome,
// both `cssText` and `getComputedStyle().backgroundImage` come back as
// `url("https://www.example.fi/…")`, a live production fetch.
//
// One escape is a backslash, one to six hex digits, and an optional single
// trailing whitespace which is part of the escape rather than of the value.
func stripForCSS(v []byte) normalised {
	if bytes.IndexByte(v, '\\') < 0 {
		return stripForURL(v)
	}
	dec := make([]byte, 0, len(v))
	pos := make([]int, 0, len(v))
	end := make([]int, 0, len(v))
	for i := 0; i < len(v); {
		if v[i] != '\\' || i+1 >= len(v) {
			dec = append(dec, v[i])
			pos = append(pos, i)
			end = append(end, i+1)
			i++
			continue
		}
		j, val, digits := i+1, 0, 0
		for j < len(v) && digits < 6 {
			d, ok := digitVal(v[j], 16)
			if !ok {
				break
			}
			val = val*16 + d
			j++
			digits++
		}
		if digits == 0 {
			// An escaped literal: the next character stands for itself.
			dec = append(dec, v[i+1])
			pos = append(pos, i)
			end = append(end, i+2)
			i += 2
			continue
		}
		if j < len(v) && (v[j] == ' ' || isURLStripped(v[j])) {
			j++ // the one whitespace that terminates an escape
		}
		if val == 0 || val > 0x10FFFF {
			val = 0xFFFD
		}
		for _, c := range []byte(string(rune(val))) {
			dec = append(dec, c)
			pos = append(pos, i)
			end = append(end, j)
		}
		i = j
	}
	// Now the URL parser's own removals, over the decoded bytes, carrying the
	// map through.
	n := stripRemovals(dec, pos, end)
	return n
}

// stripRemovals is the URL parser's own removal pass over already-decoded bytes:
// tab, LF and CR, and the character references that spell them.
//
// Each decoder used to run this inline and test isURLStripped alone, so a
// reference-spelled control survived every decode that actually fired. The
// removal only happened in stripForURL — which the three decoders reach only as
// a *fall-through*, when their own trigger byte is absent. So one unrelated
// backslash anywhere in the buffer sent stripForCSS down its real decode path
// and re-opened the leak that composing the views was supposed to close, for
// every origin in that buffer: a Windows path in an `<desc>`, a `\201c` in a
// stylesheet, a regex in a sitemap. The trigger is buffer-wide, and for the XML
// entry points the buffer is the whole body.
func stripRemovals(dec []byte, pos, end []int) normalised {
	n := normalised{b: make([]byte, 0, len(dec)), pos: make([]int, 0, len(dec)), end: make([]int, 0, len(dec))}
	for i := 0; i < len(dec); {
		if isURLStripped(dec[i]) {
			i++
			continue
		}
		if dec[i] == '&' {
			if k := removableRef(dec[i:]); k > 0 {
				i += k
				continue
			}
		}
		n.b = append(n.b, dec[i])
		n.pos = append(n.pos, pos[i])
		n.end = append(n.end, end[i])
		i++
	}
	return n
}

// removableRef reports the length of a character reference at b that spells a
// character the URL parser removes, or 0.
func removableRef(b []byte) int {
	if len(b) < 4 || b[0] != '&' {
		return 0
	}
	if b[1] != '#' {
		lim := min(len(b), 10)
		end := bytes.IndexByte(b[1:lim], ';')
		if end < 0 {
			return 0
		}
		switch string(b[1 : 1+end]) {
		case "Tab", "NewLine":
			return end + 2
		}
		return 0
	}
	j, base := 2, 10
	if j < len(b) && (b[j] == 'x' || b[j] == 'X') {
		base, j = 16, j+1
	}
	start, val := j, 0
	for j < len(b) {
		d, ok := digitVal(b[j], base)
		if !ok {
			break
		}
		val = val*base + d
		if val > 0x10FFFF {
			return 0
		}
		j++
	}
	if j == start {
		return 0
	}
	if j < len(b) && b[j] == ';' {
		j++
	}
	if val == '\t' || val == '\n' || val == '\r' {
		return j
	}
	return 0
}

// schemeLen returns the length of a leading "http:" or "https:", and which of
// the two it was. Case-insensitive: the parser lowercases the scheme.
func schemeLen(b []byte) (int, string) {
	for _, s := range []string{"https:", "http:"} {
		if len(b) >= len(s) && hasFoldPrefixASCII(b[:len(s)], s) {
			return len(s), strings.TrimSuffix(s, ":")
		}
	}
	return 0, ""
}

func hasFoldPrefixASCII(b []byte, want string) bool {
	for i := 0; i < len(want); i++ {
		c := b[i]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != want[i] {
			return false
		}
	}
	return true
}

// authorityStart returns where the authority begins in b, or -1.
//
// Two entries, straight out of the parser's state machine:
//
//   - A scheme. If it differs from the document's own scheme the parser goes to
//     special-authority-slashes and then special-authority-ignore-slashes, which
//     skips a run of '/' and '\' of *any* length, zero included — so
//     `http:www.example.fi/x` on an https page is an authority. If it matches the
//     document's scheme the parser goes to special-relative-or-authority, which
//     needs two, and `https:www.example.fi/x` is then a path.
//   - No scheme, and a run of two or more '/' and '\'.
//
// schemeAt names the scheme governing the authority at b[at], looking forwards
// for one written there and backwards for one the caller entered past. With
// neither, the reference is scheme-relative and resolves against the document,
// which is served at a variant.
func (h *hostReplacer) schemeAt(b []byte, at int) string {
	if _, s := schemeLen(b[at:]); s != "" {
		return s
	}
	// Back past the slash run, not just one byte. A candidate sits at the start
	// of a run, and `https:///host`'s authority is three slashes from its
	// scheme — looking only at the immediately preceding byte saw none, so the
	// reference was resolved under the *variant's* scheme rather than its own
	// written one. With an http variant that made `:80` a default port, and a
	// URL naming a different origin was rewritten. Candidates are emitted only
	// at run starts, so this terminates immediately.
	j := at
	for j > 0 && isSlashish(b[j-1]) {
		j--
	}
	for _, s := range []string{"https:", "http:"} {
		if j >= len(s) && hasFoldPrefixASCII(b[j-len(s):j], s) {
			return strings.TrimSuffix(s, ":")
		}
	}
	// Sorted, not a map range: `for s := range h.schemes` picks by Go's
	// randomised iteration order, so the same input in the same process produced
	// two different outputs — 176 one way and 24 the other over 200 runs. A
	// rewriter whose output depends on hash seeding undermines every
	// byte-identity and corpus-diff claim in the project.
	if len(h.schemeList) > 0 {
		return h.schemeList[0]
	}
	return "https"
}

// schemeWrittenAt reports whether the document actually wrote a scheme at this
// candidate, as opposed to schemeAt falling back to the map's.
//
// The two halves of schemeAt that read the buffer, and neither of the two that
// guess. A guess is fine for choosing which target to prefer and wrong for
// deciding which port is that scheme's default: on a mixed-scheme map the guess
// is the alphabetically first *variant* scheme anywhere in the map, so one
// site's `//host:443` was judged under another site's configuration.
func schemeWrittenAt(b []byte, at int) bool {
	if _, s := schemeLen(b[at:]); s != "" {
		return true
	}
	j := at
	for j > 0 && isSlashish(b[j-1]) {
		j--
	}
	for _, s := range []string{"https:", "http:"} {
		if j >= len(s) && hasFoldPrefixASCII(b[j-len(s):j], s) {
			return true
		}
	}
	return false
}

func (h *hostReplacer) authorityStart(b []byte) (int, bool) {
	if n, _ := schemeLen(b); n > 0 {
		i := n
		for i < len(b) && isSlashish(b[i]) {
			i++
		}
		if i-n >= 2 {
			return i, false
		}
		// Fewer than two slashes: an authority only if this scheme differs from
		// the document's. Whether it does cannot be known until the host is
		// looked up, because the document is served at *this host's* variant —
		// so the caller is told to confirm it, rather than a whole-map guess
		// being made here. Guessing from the map was wrong on a mixed-scheme
		// map: `https:www.example.fi/x`, a path to a browser on an https page,
		// was rewritten because some *other* site's variant happened to be http.
		return i, true
	}
	i := 0
	for i < len(b) && isSlashish(b[i]) {
		i++
	}
	if i >= 2 {
		return i, false
	}
	return -1, false
}

// hostRange returns the byte range of the host in b, starting at the authority.
//
// Userinfo is skipped: everything up to the *last* '@' before the authority
// ends belongs to the credentials, so `https://user@host` names host and not
// user. The authority ends at the first '/', '\', '?' or '#'; the host itself
// also ends at ':', which begins the port.
// maxHost bounds the host. A DNS name is at most 253 octets, and
// percent-encoding can only inflate that threefold, so nothing longer is a host
// this map could ever contain.
//
// Without the bound the scan ran to the end of the buffer whenever the region
// held no delimiter — and `urlTokenStarts` offers a candidate at every `http:`,
// which `authorityStart` accepts with zero slashes because the scheme differs
// from the document's. A body of `"http: "` repeated was therefore k candidates
// times O(n) each: 7 seconds at 192 KB, extrapolating to about four hours of
// pinned CPU for one 8 MiB JSON request body, with no timeout on that path.
// Third instance of the bug class scan.go documents; the other two callers were
// fixed and this one was not.
// An IPv6 literal is at most `[` plus 45 characters plus `]`; the slack is for a
// zone id. It bounds the `]` search, which used to run to the end of the buffer.
const maxIPv6 = 64

const maxHost = 253 * 3

func hostRange(b []byte, at int) (start, hostEnd, end int, port string) {
	end = len(b)
	// A bracket terminates the scan unless the authority *starts* with one.
	//
	// `[` and `]` are authority bytes, for an IPv6 literal, and tokenBoundary
	// also treats them as boundaries — so `[` both began a candidate and failed
	// to end one, and `[http:` repeated made every candidate scan to the end of
	// the buffer. 400 KB took 58 seconds, 8 MiB extrapolates to about seven
	// hours, on the response path, the request-body path and the JSON path
	// alike. maxHost is checked after the scan, so it bounded the result and not
	// the work.
	//
	// This is the fourth instance of the class, and the comment claiming "a
	// candidate only starts after a non-authority byte, so the sum over
	// candidates stays linear" was exactly the false premise: brackets are the
	// counterexample, and they were made boundaries deliberately.
	// The bracketed literal is settled first, and within a fixed window.
	//
	// `[` and `]` are authority bytes, for an IPv6 literal, and tokenBoundary
	// also treats them as boundaries — so `[` both began a candidate and failed
	// to end one. `[http:` repeated put a candidate every six bytes and an
	// authority start on every `[`, and both the forward scan and the `]` search
	// then ran to the end of the buffer: 400 KB took 58 seconds, and 8 MiB —
	// DefaultMaxBody — extrapolates to about seven hours of pinned CPU for one
	// request, on the response path, the request-body path and the JSON path
	// alike. maxHost is checked after the scan, so it bounded the result and not
	// the work.
	//
	// This is the fourth instance of the class in this file, and the comment
	// below claiming "a candidate only starts after a non-authority byte, so the
	// sum over candidates stays linear" was exactly the false premise: brackets
	// are the counterexample, and they were made boundaries deliberately.
	//
	// Both halves are bounded by what the grammar allows rather than by a guess.
	// A literal is at most `[` + 45 characters + `]`, plus a zone id; nothing
	// longer is one, so a `]` that is not inside the window means this is not an
	// authority rather than a literal we should keep looking for. And there is no
	// userinfo to search for, because userinfo would precede the bracket.
	// Past the userinfo before asking whether this is a bracketed literal.
	//
	// `at` is the authority start, and an authority may open with `user@`. The
	// test below was made on `b[at]`, so `http://u@[::1]/` never entered the
	// bracket branch — the general branch ran instead, and that branch treats
	// `[` as a boundary, so the scan stopped at the bracket and the host came
	// out empty. This function's own comment says the bracket branch needs no
	// userinfo search "because userinfo would precede the bracket": true, and
	// the reason it has to be skipped rather than assumed absent.
	if k := bytes.IndexByte(b[at:min(at+maxHost, len(b))], '@'); k >= 0 {
		if j := at + k + 1; j < len(b) && b[j] == '[' {
			at = j
		}
	}
	if at < len(b) && b[at] == '[' {
		lim := at + maxIPv6
		if lim > len(b) {
			lim = len(b)
		}
		k := bytes.IndexByte(b[at:lim], ']')
		if k < 0 {
			return at, at, at, ""
		}
		hostEnd = at + k + 1
		end = hostEnd
		for i := hostEnd; i < len(b); i++ {
			if !isAuthorityByte(b[i]) || b[i] == '[' || b[i] == ']' {
				break
			}
			end = i + 1
		}
		if hostEnd < end && b[hostEnd] == ':' {
			port = string(b[hostEnd+1 : end])
		}
		return at, hostEnd, end, port
	}
	// Stop at anything that cannot be in an authority, not just at `/ \ ? #`.
	//
	// In an attribute the value *is* the URL, so the end of the buffer is the end
	// of the authority. Everywhere else the URL is embedded and something follows
	// it — `fetch("…")`, `url(…)`, prose — and taking those bytes into the host
	// made the fold fail and the whole shape leak on exactly the surfaces §5.2
	// calls Tier 1. The byte matcher never had this problem: delimAt knows a
	// quote ends a host.
	for i := at; i < end; i++ {
		// `&` continues a host here although it cannot start one, which is why
		// it is not in isAuthorityByte. A host ends at `/ \ ? #`, so
		// `https://www.acme.fi&x` names `www.acme.fi&x` — and as a character
		// reference it is whatever it decodes to, which is more host either way.
		// delimAt has said so since round 54; this is the copy that still
		// stopped, so the matcher declined the match and the locator rewrote it
		// one pass later.
		if !isAuthorityByte(b[i]) && b[i] != '&' {
			end = i
			break
		}
		if b[i] == '[' || b[i] == ']' {
			end = i
			break
		}
	}
	start = at
	if k := bytes.LastIndexByte(b[at:end], '@'); k >= 0 {
		start = at + k + 1
	}
	// The cap belongs on the *host*, after userinfo is out of the way. Capping
	// the whole authority put the userinfo search inside the window, so pushing
	// the `@` past 759 bytes made the host vanish and the origin leak — silently,
	// since the byte matcher needs `//` immediately before a host and could not
	// see it either. A DNS name is 253 octets and percent-encoding inflates that
	// threefold; nothing longer is a host this map contains. The scan itself is
	// bounded by isAuthorityByte, and a candidate only starts after a
	// non-authority byte, so the sum over candidates stays linear without it.
	if end-start > maxHost {
		return start, start, start, ""
	}
	hostEnd = end
	if k := bytes.IndexByte(b[start:end], ':'); k >= 0 {
		port = string(b[start+k+1 : end])
		hostEnd = start + k
	}
	return start, hostEnd, end, port
}

// percentDecode decodes %XX in a host. The parser percent-decodes before
// domain-to-ASCII, so `www.ex%61mple.fi` is `www.example.fi` — and delimAt
// already reasons about a '%' on the right edge of a host without anything
// having applied the same reasoning inside it.
func percentDecode(b []byte) []byte {
	if bytes.IndexByte(b, '%') < 0 {
		return b
	}
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		if b[i] == '%' && i+2 < len(b) {
			hi, ok1 := digitVal(b[i+1], 16)
			lo, ok2 := digitVal(b[i+2], 16)
			if ok1 && ok2 {
				out = append(out, byte(hi*16+lo))
				i += 2
				continue
			}
		}
		out = append(out, b[i])
	}
	return out
}

// urlTokenStarts returns every offset in v where a URL could begin.
//
// A value is not always one URL. srcset and imagesrcset are comma-separated
// lists whose entries carry a descriptor, ping is a space-separated list, a meta
// refresh spells it `0;url=…`, and a style attribute wraps it in `url(…)`. The
// anchored matcher finds plain origins in all of those without knowing any of
// their grammars — it just scans — and the same is true here as long as the
// locator is offered each token rather than only the head of the value.
//
// Only token boundaries, so a `//` inside a path or a query cannot be mistaken
// for an authority.
// tokenBoundary reports whether a URL could begin at v[i] — the start of the
// value, or just after a byte that separates one token from the next.
// tokenBoundary reports whether a URL could begin at v[i].
//
// Defined by what cannot precede one, not by a list of what can. The list was
// space, comma, paren, equals, quote and semicolon — which is the same mistake
// isHostByte's comment says it exists to avoid, "an allowlist of terminators …
// guarantees a long tail of misses", made on the other side of the host. `>` was
// not in it, and XML element content always begins right after `>`: the whole
// obfuscated-URL family was invisible in a sitemap or a feed, on the very arm
// that was added for them, while the same bytes one space later rewrote fine.
func tokenBoundary(v []byte, i int) bool {
	if i == 0 {
		return true
	}
	c := v[i-1]
	// Brackets are authority bytes because an IPv6 literal is written in them,
	// but nothing continues *through* one — `]https://h` and `[https://h` both
	// begin a URL — so they are boundaries here even though they are host bytes
	// there.
	return c == '[' || c == ']' || !isAuthorityByte(c)
}

func urlTokenStarts(v []byte) []int {
	var out []int
	for i := 0; i < len(v); i++ {
		if !tokenBoundary(v, i) {
			continue
		}
		if n, _ := schemeLen(v[i:]); n > 0 {
			out = append(out, i)
			continue
		}
		if isSlashish(v[i]) {
			out = append(out, i)
			// Past the whole run. Every byte inside one used to be its own
			// candidate, which is redundant — authorityStart walks the run from
			// wherever it is asked, so they all reach the same authority — and it
			// made schemeAt's backwards walk quadratic in the run's length: 4x
			// the input cost 18x the time, caught by the scaling guard.
			for i+1 < len(v) && isSlashish(v[i+1]) {
				i++
			}
		}
	}
	return out
}

// locateHostIn finds the host the URL parser would read starting at n.b[at],
// and the origin it maps to. from/until are indices into the *original* value.
//
// It takes an already-stripped buffer rather than stripping one itself. Stripping
// per candidate made the pass quadratic: stripForURL allocates a []byte and a
// []int over the whole remainder, so a long value with many token starts cost
// O(k·n) — measured at 55 seconds for a 320 KB attribute value, which
// extrapolates to hours at the shipped 4 MiB token cap. That is the same bug
// class scan.go documents having already fixed once.
func (h *hostReplacer) locateHostIn(v []byte, n normalised, at int, value bool, surface string) (from, until int, repl string, ok bool) {
	// The scheme decides which port is the default, so it has to be found
	// wherever the caller entered. foldedHostLeak enters at the slash *run*, so
	// looking only forwards saw no scheme and fell back to https — and
	// `http://h:443`, whose 443 is not http's default and so is a different
	// origin, was rewritten.
	scheme := h.schemeAt(n.b, at)
	schemeWritten := schemeWrittenAt(n.b, at)
	rel, needsDifferingScheme := h.authorityStart(n.b[at:])
	if rel < 0 {
		return 0, 0, "", false
	}
	start := at + rel
	if start >= len(n.b) {
		return 0, 0, "", false
	}
	hs, he, end, port := hostRange(n.b, start)
	// A trailing dot is the host's root label inside a URL and a full stop in
	// prose, and only the caller knows which surface it is on — the same
	// distinction Matcher.RewriteText exists for. Absorbing it in a text node
	// would eat the sentence's punctuation.
	//
	// Only when the dot ends the authority, though: a full stop is never
	// followed by `:80`, so in `http:www.example.fi.:80` the dot is the root
	// label whatever the surface. Dropping it there split the host from its port
	// and the splice replaced neither, so the forward pass emitted a *variant*
	// with a root dot and a port that the request direction could not read back
	// — the round trip §4.3 exists to hold. authorityEnd re-widening to the
	// whole authority had been hiding it.
	if !value && he == end && he > hs && n.b[he-1] == '.' &&
		!(end < len(n.b) && isURLDelim(n.b[end])) {
		he--
	}
	if hs >= he {
		return 0, 0, "", false
	}
	// A string escape after the host can continue the host.
	//
	// `stripForURL` reads a backslash as a slash, so `www.acme.fi\u00a0x` ends
	// the host at the backslash and matched — but `wp_json_encode` writes every
	// non-ASCII rune that way, and the browser decodes it, so the reference
	// resolves to something that is not this origin.
	//
	// Whether it is an escape at all is the surface's answer, not the byte's. In
	// an attribute the URL parser reads a backslash as a `/`: it is structure,
	// and `<a href="https://www.acme.fi\u002d/p">` is a live origin with the path
	// `/u002d/p`. In prose `\p` is two characters. Only a buffer still carrying
	// its source escapes — a script, a style, the straggler's raw bytes — has an
	// alphabet. Round 53 applied one reading everywhere and got both ends wrong.
	// See origin.escapeAlphabetFor, which is now the only copy of that table.
	//
	// The byte matcher answers the same question at the same position, and round
	// 53 gave it a second implementation that then drifted; both halves call
	// origin.EscapeContinuesHost now.
	if he < len(n.pos) && n.pos[he] < len(v) && v[n.pos[he]] == '\\' &&
		origin.EscapeContinuesHost(v, n.pos[he], surface) {
		return 0, 0, "", false
	}
	// A port written behind an escaped colon.
	//
	// `\x3a8443` is `:8443` to a JavaScript string, and §5.4 says that is a
	// *different* origin from the bare host. The escape view decodes it and
	// declines correctly; the plain view folds the backslash to `/`, ends the
	// host there, sees no port and matches the canonical — so whichever pass
	// runs first wins, and the plain one does. Reading the port here makes both
	// passes agree, and it has to be read rather than refused: `\x3a443` is
	// https's default, which a browser drops, so that one *is* the canonical.
	if port == "" && he < len(n.pos) && n.pos[he] < len(v) {
		if w := origin.EscColonLen(v, n.pos[he], surface); w > 0 {
			d := n.pos[he] + w
			e := d
			for e < len(v) && v[e] >= '0' && v[e] <= '9' {
				e++
			}
			if e > d {
				port = string(v[d:e])
			} else if e < len(v) && !isURLDelim(v[e]) {
				// `\072x` — a colon with neither digits nor a delimiter after
				// it is a parse error, and no browser resolves it.
				return 0, 0, "", false
			}
		}
	}
	host := h.key(percentDecode(n.b[hs:he]))
	// Collecting rather than rewriting.
	//
	// `check` needs the list of absolute-URL hosts a page carries, so it can
	// subtract what the deployment names and report the rest. Round 53 wrote
	// that scan as a `//host` grep in shell — and the comment forty lines above
	// the block it sits in says why that could not work: "one spelling of an
	// origin out of the dozen this project knows a browser resolves … Writing
	// the decoder views a second time in shell was never going to work; the
	// binary already has them." A JSON-escaped `https:\/\/shop.acme.fi`, which
	// is how wp_json_encode writes every URL, was invisible to it.
	//
	// So the answer comes from here, where every view already runs. Recording
	// and declining leaves the buffer untouched, which is what a scan wants.
	if h.collect != nil {
		at := n.pos[hs]
		if h.collect[host] == nil {
			h.collect[host] = map[int]struct{}{}
		}
		h.collect[host][at] = struct{}{}
		return 0, 0, "", false
	}
	// host:port first, and the bare host only when the port is the scheme's
	// default. §5.4 matches on exact origin equality, so `https://h:80` is a
	// different origin from `https://h` and rewriting it was a false positive —
	// one the byte matcher, which disambiguates by port, never made.
	var to origin.Origin
	if port != "" {
		to, ok = h.to[host+":"+port]
		if !ok && !schemeWritten {
			// The target for this host names the scheme the document is served
			// on, which is what a scheme-relative reference resolves under.
			if cand, have := h.to[host]; have &&
				origin.NormalisePort(cand.Scheme, port) == "" {
				to, ok = cand, true
			}
		}
		if !ok && schemeWritten && origin.NormalisePort(scheme, port) == "" {
			to, ok = h.to[host]
		}
	} else {
		to, ok = h.to[host]
	}
	if !ok {
		return 0, 0, "", false
	}
	// The confirmation authorityStart asked for: with fewer than two slashes this
	// is an authority only when the reference's scheme differs from the one the
	// document is served on, which is this host's own variant scheme.
	if needsDifferingScheme && to.Scheme == scheme {
		return 0, 0, "", false
	}
	// Whatever the original spelled the host with — a tab, a reference, a
	// percent escape — the replaced range covers all of it, because pos maps
	// every surviving byte back and the removed ones lie between them.
	from, until = n.pos[hs], n.end[he-1]

	// The port, and the scheme, when the variant's differ from what is written.
	//
	// The splice used to emit to.Host alone, which is right only when the
	// variant shares the canonical's scheme and has no port — the ddev case, and
	// the only one anything tested. Anywhere else every obfuscated spelling came
	// out on the wrong port and the wrong scheme while the plain spelling was
	// correct, and the round trip then could not reverse it: the reverse table is
	// keyed host:port, the request parsed a host with no port, and the *variant*
	// hostname went upstream into the shared database.
	//
	// Widening the range to cover the scheme takes the separator with it, so the
	// separator has to be written back in the encoding it was found in.
	//
	// A raw `://` over a `%3A%2F%2F` is not "one fewer obfuscated URL", which is
	// what this comment used to claim: inside a path segment the `%2F` is what
	// keeps it one segment, so `/go/http%3A%2F%2Fwww.acme.fi%2Fbar` became
	// `/go/https://wt-a--acme.ddev.site%2Fbar` — three path separators where
	// there was one, and a URL that no longer routes. Through HostLeaksBack that
	// is a save writing a dead path into the shared database.
	//
	// The byte matcher has known this since it was written: `encoding.schemeSep`
	// exists so its replacements carry `%3A%2F%2F` and the JSON spelling. The
	// locator had no notion of it, so the two spellings of one URL disagreed.
	needScheme := to.Scheme != scheme
	hasPort := to.Port != "" || portOf(n.b, he, end) != ""
	switch {
	case needScheme:
		// The scheme word, then everything up to the host copied out of the
		// source unchanged, then the host.
		//
		// Four rounds fixed one facet of this arm each: punycode, then the third
		// arm, then a literal separator written over an encoded one, then a
		// source width mistaken for a spelling. Each fix enumerated the
		// encodings it knew, and each time the next encoding was the next
		// defect. Enumeration does not terminate here, for the reason PLAN 5.2
		// records it not terminating for the serialized spellings.
		//
		// So stop enumerating. Copying the span verbatim is right for every
		// encoding at once, including ones nobody has thought of yet - and it
		// keeps the userinfo, which lives inside that span and which this arm
		// used to delete. That was its own defect: the other two arms begin past
		// the at-sign and preserve it, so one URL got two answers depending only
		// on whether the schemes agreed, against a contract this file states
		// outright a hundred lines down.
		if sep, ok := verbatimSep(v, n, at, hs); ok {
			return n.pos[at], authorityEnd(n, he, end),
				to.Scheme + sep + to.DisplayHostPort(), true
		}
		// Zero or one slash: there is no separator run to copy, so the target's
		// own separator belongs — but the userinfo still does not disappear.
		return n.pos[at], authorityEnd(n, he, end),
			to.Scheme + schemeSepAt(v, n, at, hs) + userinfoAt(v, n, at, hs) +
				to.DisplayHostPort(), true
	case hasPort:
		return from, authorityEnd(n, he, end), to.DisplayHostPort(), true
	}
	// to.HostPort() rather than to.Host: it brackets an IPv6 literal, and the
	// bare host produced `https://2001:db8::1/x`, which the parser rejects. The
	// other two arms already went through String()/HostPort(); this one did not.
	return from, until, to.DisplayHostPort(), true
}

// authorityEnd is one past the port, or one past the host when there is none.
//
// The `:` check is the one portOf already makes, and leaving it out re-widened
// the range to the whole authority — silently undoing the trailing-dot carve-out
// locateHostIn had just applied for a text node, where a dot is a full stop and
// not the host's root label. `See http:www.example.fi. Thanks` came out as
// `See https://v.ddev.site Thanks`. Prose corruption rather than a leak, but it
// is the `value` distinction this file is careful about everywhere else.
// userinfoAt returns the source bytes between the separator and the host — the
// userinfo and its at-sign, exactly as written, or empty when there is none.
//
// `http:user:pw@host` has no separator run for verbatimSep to copy, and the
// credentials still have to survive: this arm replaces from the scheme through
// the port, so anything in that range it does not re-emit is deleted.
func userinfoAt(v []byte, n normalised, at, hs int) string {
	for i := at; i < hs && i < len(n.b); i++ {
		if n.b[i] != ':' {
			continue
		}
		j := i + 1
		for j < hs && j < len(n.b) && isSlashish(n.b[j]) {
			j++
		}
		if j >= hs || hs >= len(n.pos) || n.pos[j] >= n.pos[hs] {
			return ""
		}
		return string(v[n.pos[j]:n.pos[hs]])
	}
	return ""
}

// verbatimSep returns the source bytes between the scheme's colon and the host,
// when there is a separator run there to copy.
//
// Two or more slash-ish bytes in the view means the source wrote a real
// separator in some spelling, and copying it is exact by construction - no
// encoding table, so no next encoding to miss. Fewer than two is `http:host`,
// where there is nothing to copy and the target's own separator belongs.
func verbatimSep(v []byte, n normalised, at, hs int) (string, bool) {
	for i := at; i < hs && i < len(n.b); i++ {
		if n.b[i] != ':' {
			continue
		}
		slashes := 0
		for j := i + 1; j < hs && j < len(n.b); j++ {
			if !isSlashish(n.b[j]) {
				break
			}
			slashes++
		}
		if slashes < 2 || hs >= len(n.pos) {
			return "", false
		}
		if n.pos[i] >= n.pos[hs] || n.pos[hs] > len(v) {
			return "", false
		}
		return string(v[n.pos[i]:n.pos[hs]]), true
	}
	return "", false
}

// schemeSepAt reports the `://` between a scheme at view index at and a host at
// view index hs, in the encoding the source wrote it in.
//
// Read from the source *bytes*, not from the source width. Width says only how
// many bytes one view byte came from, and three of them is `%3A` — but it is
// also `\3a`, the CSS escape this file keeps an entire view for, and five is
// `&#58;` and six is `\u003a`. Answering `%3A%2F%2F` to all of them did to those
// spellings exactly what the round before had stopped it doing to percent: an
// inline `style="background:url(http\3a\2f\2fwww.acme.fi/l.png)"` came out as
// `url(https%3A%2F%2Fwt-a--acme.ddev.site/l.png)`, which the CSS tokenizer
// unescapes to nothing useful — a single relative path segment, a 404 where
// production served an image, and through HostLeaksBack the same unresolvable
// spelling written back into the shared database.
//
// Raw is the right fallback for everything else: it is what an HTML attribute,
// a CSS value and XML text all resolve, and it is what this arm emitted for
// those shapes before any of this.
func schemeSepAt(v []byte, n normalised, at, hs int) string {
	for i := at; i < hs && i < len(n.b); i++ {
		if n.b[i] != ':' {
			continue
		}
		if len(v[n.pos[i]:n.end[i]]) == 3 {
			if src := v[n.pos[i]:n.end[i]]; src[0] == '%' && src[1] == '3' &&
				(src[2] == 'A' || src[2] == 'a') {
				return "%3A%2F%2F"
			}
		}
		// Everything the source wrote between the colon and the host. A
		// backslash anywhere in it is JSON's `\/` — the view cannot show this,
		// because stripForURL reads a backslash as a slash and the escape
		// disappears into the authority run.
		if hs < len(n.pos) && n.end[i] <= n.pos[hs] {
			if bytes.IndexByte(v[n.end[i]:n.pos[hs]], '\\') >= 0 {
				return ":" + `\/` + `\/`
			}
		}
		return "://"
	}
	return "://"
}

// isURLDelim reports whether c ends the authority: the start of a path, query
// or fragment.
func isURLDelim(c byte) bool {
	return c == '/' || c == '\\' || c == '?' || c == '#'
}

func authorityEnd(n normalised, he, end int) int {
	if end > he && n.b[he] == ':' {
		return n.end[end-1]
	}
	return n.end[he-1]
}

// portOf reports the port text between the host end and the authority end.
func portOf(b []byte, he, end int) string {
	if end > he && b[he] == ':' {
		return string(b[he+1 : end])
	}
	return ""
}

// foldedHostLeak catches a host that only *folds* onto a canonical one.
//
// The byte matcher compares bytes, so a host spelled with a soft hyphen, with
// fullwidth letters, with U+3002 for the dots, or in NFD shares nothing with the
// pattern it names — and unlike the shapes above, that is true on every surface,
// not just in a URL attribute. A production origin in a text node, in an inline
// script, in a stylesheet or in a comment is still a production origin the
// browser will resolve when something reads it.
//
// So this runs over the whole value on every surface, and it is cheap because it
// cannot fire without a non-ASCII byte: a host that is pure ASCII either matches
// the pattern already or is not the canonical host at all. That one test skips
// the entire pass on most documents, and on the rest the work is bounded by the
// number of `//` runs.
func (w *HTML) foldedHostLeak(surface string, base int, v []byte, value bool) []byte {
	if !w.hosts.active() {
		return v
	}
	nonASCII := false
	for _, c := range v {
		if c >= 0x80 {
			nonASCII = true
			break
		}
	}
	if !nonASCII {
		return v
	}

	n := stripForURL(v)
	var out []byte
	prev := 0
	for i := 0; i+1 < len(n.b); i++ {
		if !isSlashish(n.b[i]) {
			continue
		}
		// Jump to the end of the run rather than trying every offset inside it.
		// authorityStart walks the run from wherever it is asked, so starting at
		// each of its L bytes was L²/2 work — 20 seconds for a 400,000-byte run,
		// extrapolating to about 38 minutes at the 4 MiB token cap. That is the
		// bug locateHostIn's own comment says was already fixed once, live again
		// in its other caller. One non-ASCII byte anywhere in the value is the
		// only other trigger.
		run := i
		for run < len(n.b) && isSlashish(n.b[run]) {
			run++
		}
		// The same left anchor urlTokenStarts applies. Without it this walked
		// every `//` in the buffer, so `https://cdn.other/p//ｗｗｗ.example.fi/q`
		// — where the run is a path separator, not an authority — had its path
		// segment rewritten, while the plain ASCII spelling of the same URL was
		// correctly left alone. The two spellings disagreeing is the model error;
		// the oracle's second half calls it a false positive.
		if run-i < 2 || n.pos[i] < prev || !tokenBoundary(n.b, i) {
			i = run - 1
			continue
		}
		from, until, repl, ok := w.hosts.locateHostIn(v, n, i, value, surface)
		if !ok {
			i = run - 1
			continue
		}
		// Nothing to do when the bytes already say the variant, and nothing to
		// do when the host is plain ASCII — that is the byte matcher's job, and
		// it has already run.
		if !bytes.Equal(v[from:until], []byte(repl)) && hasNonASCII(v[from:until]) {
			out = append(out, v[prev:from]...)
			out = append(out, repl...)
			prev = until
			w.stats.Record(surface, base, []origin.Event{{
				// Not base+from: Stats.Record adds base itself. d3ad6a7 fixed
				// that for the views going through w.record and said "all five
				// newer views" — these two build their events inline and were
				// missed, so the surfaces whose own comments say "a non-zero
				// count is worth looking at" were the ones reporting an offset
				// past the end of the document.
				Offset:  from,
				Surface: surface,
				Action:  origin.ActionRewrote,
				Text:    string(v[from:until]),
			}})
		}
	}
	if out == nil {
		return v
	}
	return append(out, v[prev:]...)
}

func hasNonASCII(b []byte) bool {
	for _, c := range b {
		if c >= 0x80 {
			return true
		}
	}
	return false
}

// normaliseURLLeak replaces every host in v that the URL parser would read and
// this map rewrites, and returns v untouched when there are none.
//
// Only the hosts' byte ranges change. Everything else — the scheme as written,
// the separator however it was spelled, userinfo, port, path, query, fragment,
// and every byte between the entries of a list — is copied through, so this
// cannot damage a value it does not need to fix.
func (w *HTML) normaliseURLLeak(surface string, base int, v []byte, value bool) []byte {
	if !w.hosts.active() {
		return v
	}
	n := stripForURL(v)
	var out []byte
	prev := 0
	for _, off := range urlTokenStarts(n.b) {
		if off < len(n.pos) && n.pos[off] < prev {
			continue // inside a host already replaced
		}
		from, until, repl, ok := w.hosts.locateHostIn(v, n, off, value, surface)
		if !ok {
			continue
		}
		out = append(out, v[prev:from]...)
		out = append(out, repl...)
		prev = until
		w.stats.Record(surface, base, []origin.Event{{
			// Not base+from — see the note on the other inline builder above.
			Offset:  from,
			Surface: surface,
			Action:  origin.ActionRewrote,
			Text:    string(v[from:until]),
		}})
	}
	if out == nil {
		return v
	}
	return append(out, v[prev:]...)
}

// hostsFor gives a matcher its host table, built once and cached on the matcher
// so the JSON path does not rebuild it per string.
var hostsCache sync.Map // *origin.Matcher -> *hostReplacer

func hostsFor(m *origin.Matcher) *hostReplacer {
	if h, ok := hostsCache.Load(m); ok {
		return h.(*hostReplacer)
	}
	h := newHostReplacer(m)
	hostsCache.Store(m, h)
	return h
}

// rewriteAll applies both catchers to a standalone buffer — the JSON path, which
// has no HTML tokenizer around it. Counters are the caller's business; the
// events this would emit duplicate the ones RewriteJSON already records.
func (h *hostReplacer) rewriteAll(v []byte, value bool, surface string, ev *[]origin.Event) []byte {
	if !h.active() {
		return v
	}
	// One pass, not two.
	//
	// The second pass over slashRunStarts existed because that scan found
	// scheme-relative authorities urlTokenStarts did not. Anchoring
	// slashRunStarts on a token boundary — which it needed, to stop rewriting
	// path segments — made it a strict subset of urlTokenStarts, so the pass
	// could no longer find anything the pass above it had not already found.
	// TestSlashRunStartsAreASubsetOfTokenStarts pins that, because it is what
	// makes removing this safe.
	//
	// It was not free: the gate is one non-ASCII byte anywhere in the buffer, so
	// every Finnish page, feed, sitemap and request body paid for it. On an
	// 8 MiB body a single `ä` took transient allocation from 156 MB to 292 MB.
	v = h.spliceHosts(v, urlTokenStarts, value, surface, ev)
	// The CSS view too, because the *forward* direction emits it.
	//
	// cssEscapeLeak splices the host into the escaped spelling, so a style
	// attribute goes to the browser as `url(https\3a \2f \2f <variant>/x)` —
	// and the editor posts that back. Nothing on the way in could read it: the
	// byte matcher's prefilter needs `//`, `\/` or `%2F` and that string has
	// none, and stripForCSS was reachable only from the forward pass. So the
	// variant hostname went upstream and into the database §4.3 says stays
	// byte-identical to production.
	//
	// The rule this is an instance of: every spelling the forward direction can
	// *emit*, the reverse direction must be able to *read*.
	if bytes.IndexByte(v, '\\') >= 0 {
		v = h.spliceHostsIn(stripForCSS(v), v, urlTokenStarts, value, surface, ev)
	}
	// And the percent view, for an encoding composed with another one.
	if bytes.IndexByte(v, '%') >= 0 {
		v = h.spliceHostsIn(stripForPercent(v), v, urlTokenStarts, value, surface, ev)
	}
	// And percent-then-JSON, which is `post.php` sending a block delimiter as a
	// urlencoded field: the backslash Gutenberg wrote is `%5C`, so the escape
	// reads `%5Cu002d%5Cu002d` and no literal backslash is there for the plain
	// view to find.
	//
	// Here, not on the reference path. Moving it there was reasoned as "a
	// urlencoded body is a request" — but `rewriteAll` is not the request path.
	// It is what the proxy runs over every Tier 1 response header and over a
	// non-XML `text/plain` body, in the *forward* direction, so the move left
	// the two directions disagreeing about one encoding: `HostLeaksBack` read
	// the spelling and `HostLeaks` no longer did. One copy, because two costs
	// 455x the body and blows the allocation ceiling.
	if bytes.Contains(v, []byte("%5Cu")) || bytes.Contains(v, []byte("%5cu")) {
		v = h.spliceHostsIn(composeView(stripForPercent(v), escView(surface)), v,
			urlTokenStarts, value, surface, ev)
	}
	// And JSON's own escape, which is the same rule again and the sharpest case
	// of it: Gutenberg escapes `--` to `\u002d\u002d` in every block delimiter,
	// and every variant hostname contains `--` by construction. Here rather than
	// in a forward-only surface precisely so the reverse direction gets it —
	// this spelling is emitted by *WordPress*, not by us, and the failure it
	// causes is the variant hostname landing in the shared database.
	if hasEsc(v, surface) {
		v = h.spliceHostsIn(escView(surface)(v), v, urlTokenStarts, value, surface, ev)
	}
	return v
}

// rewriteAllRefs is rewriteAll for a consumer that decodes character references
// — the XML family, XHTML's script and style, and every request body.
func (h *hostReplacer) rewriteAllRefs(v []byte, value bool, surface string, ev *[]origin.Event) []byte {
	v = h.refsOnly(h.rewriteAll(v, value, surface, ev), value, surface, ev)
	// And references spelling CSS escapes, which needs both decodes composed.
	if h.active() && bytes.IndexByte(v, '&') >= 0 {
		if n, ok := refsThenCSS(v); ok {
			v = h.spliceHostsIn(n, v, urlTokenStarts, value, surface, ev)
		}
		// And references spelling a JSON escape, which is the same composition
		// on the axis the escape view was added without. `stripForCSS` is
		// reachable through a reference decode and `stripForJSONEsc` was not, so
		// `wt-a&#92;u002d&#92;u002dacme.ddev.site` was read in its literal
		// spelling and not in its reference-encoded one — on every surface whose
		// parser decodes references, which includes every request body. A
		// spelling is a family; this is the member the family was missing.
		if hasRefJSONEsc(v) {
			v = h.spliceHostsIn(composeView(stripForRefs(v), escView(surface)), v,
				urlTokenStarts, value, surface, ev)
		}
	}
	return v
}

// refsOnly is the reference view alone, for callers where the other views would
// be wrong — an HTML attribute, where the browser decodes references but not CSS
// escapes.
func (h *hostReplacer) refsOnly(v []byte, value bool, surface string, ev *[]origin.Event) []byte {
	if !h.active() || bytes.IndexByte(v, '&') < 0 {
		return v
	}
	return h.spliceHostsIn(stripForRefs(v), v, urlTokenStarts, value, surface, ev)
}

// slashRunStarts yields the first byte of each run of two or more slashes, which
// is where a scheme-relative authority can begin.
func slashRunStarts(b []byte) []int {
	var out []int
	for i := 0; i < len(b); i++ {
		if !isSlashish(b[i]) {
			continue
		}
		run := i
		for run < len(b) && isSlashish(b[run]) {
			run++
		}
		// Anchored, the way foldedHostLeak anchors — its comment cites this
		// exact input. `https://cdn.other/p//www.example.fi/q` is a path
		// segment, not an authority, and the standalone path rewrote it while
		// the HTML path correctly left it alone. The two spellings disagreeing
		// is the model error the oracle's second half calls a false positive,
		// and in the request direction it edits a path on its way into the
		// shared database. The gate is one non-ASCII byte anywhere in the
		// buffer, so one `ä` in a Finnish feed armed it for every candidate.
		if run-i >= 2 && tokenBoundary(b, i) {
			out = append(out, i)
		}
		i = run - 1
	}
	return out
}

func (h *hostReplacer) spliceHosts(v []byte, starts func([]byte) []int, value bool, surface string, ev *[]origin.Event) []byte {
	return h.spliceHostsIn(stripForURL(v), v, starts, value, surface, ev)
}

// spliceHostsIn splices, and appends what it did to ev when the caller has a
// census to report to.
//
// It discarded events unconditionally, and the justification — "the events this
// would emit duplicate the ones RewriteJSON already records" — was true of the
// JSON path and of no other caller. So every standalone entry point rewrote
// silently: the request line, the query, Referer/Origin, every request body,
// every response header, and every text/plain and XML response body. A sitemap
// with five CSS-escaped origins came out rewritten with `--json` reporting
// none, and `--dry-run` — which §5.8 makes the tool you point at a canonical
// checkout to decide whether a site needs hostshift — answered "nothing to do"
// on the very shapes these views exist for. Third recurrence of the
// instrument-lies class, at the entry points the earlier fix did not enumerate.
func (h *hostReplacer) spliceHostsIn(n normalised, v []byte, starts func([]byte) []int, value bool, surface string, ev *[]origin.Event) []byte {
	out, events := h.spliceHostsLog(n, v, starts, value, surface)
	if ev != nil {
		*ev = append(*ev, events...)
	}
	return out
}

// spliceHostsLog is spliceHostsIn with the events it performed, for the callers
// that have a census to report to.
//
// Every view but the reference one went through the discarding wrapper, so
// `--dry-run` on a page whose origins are all percent- or CSS-encoded printed
// zero rewrites and zero candidates — and §5.8 makes that mode the thing you
// point at a canonical checkout to decide whether a site needs hostshift at all.
// It answered "nothing to do" on the very WooCommerce blob stripForPercent was
// written for. That is the instrument-reporting-health-it-did-not-measure
// failure the PLAN already records twice, reintroduced by the fix for it.
func (h *hostReplacer) spliceHostsLog(n normalised, v []byte, starts func([]byte) []int, value bool, surface string) ([]byte, []origin.Event) {
	var out []byte
	var events []origin.Event
	prev := 0
	for _, off := range starts(n.b) {
		if off < len(n.pos) && n.pos[off] < prev {
			continue
		}
		from, until, repl, ok := h.locateHostIn(v, n, off, value, surface)
		if !ok {
			continue
		}
		if from < prev {
			continue
		}
		out = append(out, v[prev:from]...)
		out = append(out, repl...)
		events = append(events, origin.Event{
			Offset: from,
			Action: origin.ActionRewrote,
			Text:   string(v[from:until]),
		})
		prev = until
	}
	if out == nil {
		return v, nil
	}
	return append(out, v[prev:]...), events
}

// composeView applies a second decoder to an already-decoded view, mapping the
// result's positions all the way back to the original bytes.
//
// The views were siblings, never a stack: each one decoded the raw value its own
// way and none of them ever saw another's output. So a spelling that needs two
// decodes to become a URL went out byte-identical, and the census called the page
// clean.
func composeView(outer normalised, f func([]byte) normalised) normalised {
	inner := f(outer.b)
	n := normalised{
		b:   inner.b,
		pos: make([]int, len(inner.b)),
		end: make([]int, len(inner.b)),
	}
	for i := range inner.b {
		from, until := inner.pos[i], inner.end[i]-1
		if from < 0 || from >= len(outer.pos) {
			// An empty view, not `inner`. Returning inner was labelled "decline
			// rather than corrupt" and did the opposite: inner's positions index
			// the *intermediate* buffer, so spliceHostsLog would splice at
			// offsets into a buffer that is not the one being written. Nothing
			// reaches this today — all three decoders emit pos[i] in [0,len(in))
			// and end[i] in (pos[i],len(in)], verified by arming this branch with
			// a panic and running the suite plus eleven million fuzz executions —
			// but a fourth decoder that does not is the trap this was left as.
			return normalised{}
		}
		if until < from {
			until = from
		}
		if until >= len(outer.end) {
			until = len(outer.end) - 1
		}
		n.pos[i], n.end[i] = outer.pos[from], outer.end[until]
	}
	return n
}

// stripForRefsCSS is the CSS tokenizer's view of a value that the HTML parser
// decodes references in first — a `style` attribute, or a `<style>` element
// inside `<svg>`.
//
// `style="background:url(https&#92;3a &#92;2f &#92;2f h/x.png)"` is one Chrome
// fetches: getAttribute returns exactly the bytes the plain `\3a ` spelling
// gives, because HTML decodes `&#92;` to a backslash before the CSS tokenizer
// ever runs. Neither view alone can see it — stripForCSS's `\` guard is false on
// the still-encoded bytes, and stripForRefs decodes to a `\3a ` that no
// reference view then unescapes. Any sanitiser or editor that entity-encodes a
// backslash in an inline style produces it, and inline styles are where a page
// builder's background images live.
func stripForRefsCSS(v []byte) normalised {
	return composeView(stripForRefs(v), stripForCSS)
}

// refsThenCSS is stripForRefsCSS for the two callers that skip on no match.
//
// It used to skip the composed view when the reference-decoded buffer held no
// backslash, on the reasoning that the CSS layer only unescapes backslashes.
// That reasoning was wrong and the skip was a leak. stripForCSS *falls through
// to stripForURL* when there is no backslash, and stripForURL is what removes
// tab, LF and CR — including their character-reference spellings, which
// stripForRefs deliberately leaves alone because parseURLRef must never emit a
// control character. So this composition was quietly the engine's only
// refs-then-URL-strip view, and `https:&#47;&#10;&#47;host` went out
// byte-identical on every standalone XML and SVG body, on `<style>` and text
// inside foreign content, and in XHTML — with the census reporting a clean
// page. Chrome preserves a reference to LF through XML attribute-value
// normalisation and ada then strips it, so that is a live production fetch.
//
// The allocation this bought back (118x to 85x on ampersand-only shapes) is not
// worth a leak, and the remaining cost is tracked by TestAllocationStaysBounded
// rather than traded against correctness.
func refsThenCSS(v []byte) (normalised, bool) {
	return composeView(stripForRefs(v), stripForCSS), true
}

// percentLeak is the percent-decoded view, for an encoding composed with
// another one — `https%3A%5C%2F%5C%2Fhost`, which is what percent-encoding a
// JSON-escaped URL produces and what WooCommerce hands to decodeURIComponent.
func (w *HTML) percentLeak(surface string, base int, v []byte, value bool) []byte {
	if !w.hosts.active() || bytes.IndexByte(v, '%') < 0 {
		return v
	}
	out, events := w.hosts.spliceHostsLog(stripForPercent(v), v, urlTokenStarts, value, surface)
	w.record(surface, base, events)
	return out
}

// jsonEscLeak is the JSON-escape view as a recorded HTML-surface catcher, for
// the spelling WordPress's own block serializer emits.
func (w *HTML) jsonEscLeak(surface string, base int, v []byte, value bool) []byte {
	if !w.hosts.active() || !hasEsc(v, surface) {
		return v
	}
	out, events := w.hosts.spliceHostsLog(escView(surface)(v), v, urlTokenStarts, value, surface)
	w.record(surface, base, events)
	return out
}

// record stamps a view's events with its surface and the value's offset in the
// document, which spliceHostsLog does not know, and hands them to the census.
func (w *HTML) record(surface string, base int, events []origin.Event) {
	for i := range events {
		events[i].Surface = surface
	}
	// Stats.Record adds `base` itself. Adding it here too reported every event
	// from these five views at twice the value's offset — while the byte
	// matcher, which goes through Record alone, stayed correct. A page then
	// showed a mixture of right and wrong offsets, which is worse than
	// uniformly wrong because it looks credible, and §5.8 makes --explain the
	// thing that points a developer at the byte that leaked.
	w.stats.Record(surface, base, events)
}

// refsLeak is the reference view as a recorded HTML-surface catcher.
//
// The foreign-content and XHTML gate reached it through rewriteAllRefs, which
// records nothing, so a reference-encoded origin inside `<svg><style>` was
// rewritten with the census reporting a clean page.
func (w *HTML) refsLeak(surface string, base int, v []byte, value bool) []byte {
	if !w.hosts.active() || bytes.IndexByte(v, '&') < 0 {
		return v
	}
	out, events := w.hosts.spliceHostsLog(stripForRefs(v), v, urlTokenStarts, value, surface)
	w.record(surface, base, events)
	return out
}

// refsCSSLeak is stripForRefsCSS as a recorded HTML-surface catcher, for a style
// surface whose references the parser decodes before the CSS tokenizer runs.
func (w *HTML) refsCSSLeak(surface string, base int, v []byte) []byte {
	if !w.hosts.active() || bytes.IndexByte(v, '&') < 0 {
		return v
	}
	n, ok := refsThenCSS(v)
	if !ok {
		return v
	}
	out, events := w.hosts.spliceHostsLog(n, v, urlTokenStarts, true, surface)
	w.record(surface, base, events)
	return out
}

// cssEscapeLeak is the CSS-tokenizer view of a style surface.
func (w *HTML) cssEscapeLeak(surface string, base int, v []byte) []byte {
	if !w.hosts.active() || bytes.IndexByte(v, '\\') < 0 {
		return v
	}
	out, events := w.hosts.spliceHostsLog(stripForCSS(v), v, urlTokenStarts, true, surface)
	w.record(surface, base, events)
	return out
}

// bareSurface names what a caller holds when it holds a buffer no string
// decoder will run over again: a complete URL-bearing value, or a run of prose.
// Neither carries source escapes, so in neither is a backslash anything but a
// path separator — which is the whole reason the surface has to travel this far
// down. RepairSerialized uses it for values it has already unquoted, where the
// JSON surface name would claim escapes that are no longer there.
func bareSurface(value bool) string {
	if value {
		return SurfaceHTMLAttr
	}
	return SurfaceText
}

// HostLeaks applies the URL-parser locator and the IDNA fold to a standalone
// buffer, for the proxy's request line, query, headers and non-HTML bodies.
//
// Those surfaces had the byte matcher alone, and the response side manufactures
// exactly what the byte matcher cannot see: it splices only the matched host, so
// `https:\\www.example.fi/a` goes to the browser as `https:\\wt-a--x.ddev.site/a`
// — an obfuscated *variant*. The byte matcher's prefilter needs `//`, `\/` or
// `%2F`, and that string has none, so a form post carrying it back went upstream
// unreversed and the variant hostname was written into the shared database. That
// is worse than a leak: §4.3's case for the whole design is that the database
// stays byte-identical to production, shared by canonical, every worktree and CI.
func HostLeaks(m *origin.Matcher, b []byte, value bool) []byte {
	if m == nil || len(b) == 0 {
		return b
	}
	return hostsFor(m).rewriteAll(b, value, bareSurface(value), nil)
}

// HostLeaksXML is HostLeaks for a body whose parser decodes character
// references: the XML family, where `href="https:&#47;&#47;host"` is a live
// reference in an SVG, a feed or a sitemap.
// HostLeaksBack is HostLeaks for the request direction, which must be able to
// read every spelling the response direction can emit.
//
// The two directions are not symmetric surfaces. Nothing decodes a character
// reference in a text/plain *response*, so decoding one there would corrupt the
// text — but the forward pass splices hosts into reference-encoded and
// CSS-escaped spellings inside a page, and the editor or form that posts that
// content back sends it as text/plain, urlencoded or JSON. Reading only the
// plain spelling on the way in meant a *variant* hostname went upstream and into
// the shared database, which is the one failure §4.3 says the whole design
// exists to prevent.
//
// The rule, stated once: every spelling the forward direction can emit, the
// reverse direction must be able to read.
func HostLeaksBack(m *origin.Matcher, b []byte) []byte {
	if m == nil || len(b) == 0 {
		return b
	}
	return hostsFor(m).rewriteAllRefs(b, true, SurfaceHTMLAttr, nil)
}

func HostLeaksXML(m *origin.Matcher, b []byte, value bool) []byte {
	if m == nil || len(b) == 0 {
		return b
	}
	return hostsFor(m).rewriteAllRefs(b, value, bareSurface(value), nil)
}

// Counted returns HostLeaks, HostLeaksBack and HostLeaksXML with the census
// wired up, which is what the proxy uses.
//
// The plain forms above rewrite silently, and every one of the proxy's eleven
// call sites used them — so a body the engine rewrote forty origins in reported
// zero, and `--dry-run` said a site needed no hostshift. Anything with a Stats
// to report to should call these. The plain forms now have no non-test callers —
// the filter became a Counted caller in the same change — and are kept because
// the internal chain still needs a nil-accumulator path.
func HostLeaksCounted(m *origin.Matcher, b []byte, value bool, st *Stats, surface string, base int) []byte {
	if m == nil || len(b) == 0 {
		return b
	}
	return counted(st, surface, base, func(ev *[]origin.Event) []byte {
		return hostsFor(m).rewriteAll(b, value, surface, ev)
	})
}

// HostLeaksBackCounted is HostLeaksBack with the census wired up.
func HostLeaksBackCounted(m *origin.Matcher, b []byte, st *Stats, surface string, base int) []byte {
	if m == nil || len(b) == 0 {
		return b
	}
	return counted(st, surface, base, func(ev *[]origin.Event) []byte {
		return hostsFor(m).rewriteAllRefs(b, true, surface, ev)
	})
}

// HostLeaksXMLCounted is HostLeaksXML with the census wired up.
func HostLeaksXMLCounted(m *origin.Matcher, b []byte, value bool, st *Stats, surface string, base int) []byte {
	if m == nil || len(b) == 0 {
		return b
	}
	return counted(st, surface, base, func(ev *[]origin.Event) []byte {
		return hostsFor(m).rewriteAllRefs(b, value, surface, ev)
	})
}

// counted runs f with an accumulator and reports what it did.
func counted(st *Stats, surface string, base int, f func(*[]origin.Event) []byte) []byte {
	var ev []origin.Event
	out := f(&ev)
	for i := range ev {
		ev[i].Surface = surface
	}
	st.Record(surface, base, ev)
	return out
}

// HostsIn reports every absolute-URL host the body carries, with a count, using
// the same views the rewriter uses — every escape spelling, every composed
// encoding, on every surface.
//
// This exists so `ddev hostshift check` can ask "what hostnames does this page
// actually link to" without a map. Round 53 asked that question with a `//host`
// grep in shell, which sees exactly one spelling out of the dozen this project
// knows a browser resolves: a JSON-escaped `https:\/\/shop.acme.fi` — how
// wp_json_encode writes every URL — was invisible, and so were the percent and
// character-reference spellings. The engine already decodes all of them, and the
// comment above the canonical-direction scan in that same shell function had
// already said so in as many words.
func HostsIn(b []byte) map[string]int {
	if len(b) == 0 {
		return nil
	}
	h := &hostReplacer{collect: map[string]map[int]struct{}{}}
	// The reference-decoding pass, which runs every other view inside it. `value`
	// is false: a page is prose as often as it is markup, and a scan wants the
	// conservative reading of a trailing dot.
	//
	// The straggler's surface, because the buffer is a whole served page with its
	// script escapes intact, and what the scan reports is what the browser would
	// dereference: a canonical followed by `\xe4` resolves somewhere else and is
	// not an origin this page carries.
	//
	// Not SurfaceRawText, which names the markup inside <noscript> and <title>
	// and has no string decoder over it. One name cannot mean both, and this is
	// the caller that made it try.
	h.rewriteAllRefs(append([]byte(nil), b...), false, SurfaceStraggler, nil)
	out := make(map[string]int, len(h.collect))
	for host, at := range h.collect {
		out[host] = len(at)
	}
	return out
}

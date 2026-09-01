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
	switch {
	case c >= 0x80:
		return true
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	switch c {
	case '-', '.', '_', '~', '%', '+', '@', ':', '[', ']':
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
func stripForJSONEsc(v []byte) normalised {
	if !hasJSONEsc(v) {
		return stripForURL(v)
	}
	dec := make([]byte, 0, len(v))
	pos := make([]int, 0, len(v))
	end := make([]int, 0, len(v))
	for i := 0; i < len(v); {
		if v[i] == '\\' && i+1 < len(v) {
			// `\/` is already handled by the JSON surface, but it reaches here
			// too on the composed views, and decoding it costs nothing.
			if v[i+1] == '/' {
				dec = append(dec, '/')
				pos = append(pos, i)
				end = append(end, i+2)
				i += 2
				continue
			}
			if v[i+1] == 'u' && i+6 <= len(v) {
				if r, ok := jsonEscRune(v[i+2 : i+6]); ok {
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
	return bytes.Contains(v, []byte(`\u`))
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
		if i+n < len(v) && v[i+n] == 'u' {
			return true
		}
	}
	return false
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
func jsonEscRune(h []byte) ([]byte, bool) {
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
		if !isAuthorityByte(b[i]) {
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
func (h *hostReplacer) locateHostIn(n normalised, at int, value bool) (from, until int, repl string, ok bool) {
	// The scheme decides which port is the default, so it has to be found
	// wherever the caller entered. foldedHostLeak enters at the slash *run*, so
	// looking only forwards saw no scheme and fell back to https — and
	// `http://h:443`, whose 443 is not http's default and so is a different
	// origin, was rewritten.
	scheme := h.schemeAt(n.b, at)
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
	if !value && he == end && he > hs && n.b[he-1] == '.' {
		he--
	}
	if hs >= he {
		return 0, 0, "", false
	}
	host := h.key(percentDecode(n.b[hs:he]))
	// host:port first, and the bare host only when the port is the scheme's
	// default. §5.4 matches on exact origin equality, so `https://h:80` is a
	// different origin from `https://h` and rewriting it was a false positive —
	// one the byte matcher, which disambiguates by port, never made.
	var to origin.Origin
	if port != "" {
		to, ok = h.to[host+":"+port]
		if !ok && origin.NormalisePort(scheme, port) == "" {
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
	// Widening the range to cover the scheme drops the obfuscated separator with
	// it. That is a fair trade: what replaces it resolves to the same origin, and
	// one fewer obfuscated URL on the page is not a loss.
	needScheme := to.Scheme != scheme
	hasPort := to.Port != "" || portOf(n.b, he, end) != ""
	switch {
	case needScheme:
		// From the scheme through the port: the whole origin, written plainly.
		return n.pos[at], authorityEnd(n, he, end), to.String(), true
	case hasPort:
		return from, authorityEnd(n, he, end), to.HostPort(), true
	}
	// to.HostPort() rather than to.Host: it brackets an IPv6 literal, and the
	// bare host produced `https://2001:db8::1/x`, which the parser rejects. The
	// other two arms already went through String()/HostPort(); this one did not.
	return from, until, to.HostPort(), true
}

// authorityEnd is one past the port, or one past the host when there is none.
//
// The `:` check is the one portOf already makes, and leaving it out re-widened
// the range to the whole authority — silently undoing the trailing-dot carve-out
// locateHostIn had just applied for a text node, where a dot is a full stop and
// not the host's root label. `See http:www.example.fi. Thanks` came out as
// `See https://v.ddev.site Thanks`. Prose corruption rather than a leak, but it
// is the `value` distinction this file is careful about everywhere else.
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
	if w.hosts == nil || len(w.hosts.to) == 0 {
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
		from, until, repl, ok := w.hosts.locateHostIn(n, i, value)
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
	if w.hosts == nil || len(w.hosts.to) == 0 {
		return v
	}
	n := stripForURL(v)
	var out []byte
	prev := 0
	for _, off := range urlTokenStarts(n.b) {
		if off < len(n.pos) && n.pos[off] < prev {
			continue // inside a host already replaced
		}
		from, until, repl, ok := w.hosts.locateHostIn(n, off, value)
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
func (h *hostReplacer) rewriteAll(v []byte, value bool, ev *[]origin.Event) []byte {
	if h == nil || len(h.to) == 0 {
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
	v = h.spliceHosts(v, urlTokenStarts, value, ev)
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
		v = h.spliceHostsIn(stripForCSS(v), v, urlTokenStarts, value, ev)
	}
	// And the percent view, for an encoding composed with another one.
	if bytes.IndexByte(v, '%') >= 0 {
		v = h.spliceHostsIn(stripForPercent(v), v, urlTokenStarts, value, ev)
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
		v = h.spliceHostsIn(composeView(stripForPercent(v), stripForJSONEsc), v,
			urlTokenStarts, value, ev)
	}
	// And JSON's own escape, which is the same rule again and the sharpest case
	// of it: Gutenberg escapes `--` to `\u002d\u002d` in every block delimiter,
	// and every variant hostname contains `--` by construction. Here rather than
	// in a forward-only surface precisely so the reverse direction gets it —
	// this spelling is emitted by *WordPress*, not by us, and the failure it
	// causes is the variant hostname landing in the shared database.
	if hasJSONEsc(v) {
		v = h.spliceHostsIn(stripForJSONEsc(v), v, urlTokenStarts, value, ev)
	}
	return v
}

// rewriteAllRefs is rewriteAll for a consumer that decodes character references
// — the XML family, XHTML's script and style, and every request body.
func (h *hostReplacer) rewriteAllRefs(v []byte, value bool, ev *[]origin.Event) []byte {
	v = h.refsOnly(h.rewriteAll(v, value, ev), value, ev)
	// And references spelling CSS escapes, which needs both decodes composed.
	if h != nil && len(h.to) > 0 && bytes.IndexByte(v, '&') >= 0 {
		if n, ok := refsThenCSS(v); ok {
			v = h.spliceHostsIn(n, v, urlTokenStarts, value, ev)
		}
		// And references spelling a JSON escape, which is the same composition
		// on the axis the escape view was added without. `stripForCSS` is
		// reachable through a reference decode and `stripForJSONEsc` was not, so
		// `wt-a&#92;u002d&#92;u002dacme.ddev.site` was read in its literal
		// spelling and not in its reference-encoded one — on every surface whose
		// parser decodes references, which includes every request body. A
		// spelling is a family; this is the member the family was missing.
		if hasRefJSONEsc(v) {
			v = h.spliceHostsIn(composeView(stripForRefs(v), stripForJSONEsc), v,
				urlTokenStarts, value, ev)
		}
	}
	return v
}

// refsOnly is the reference view alone, for callers where the other views would
// be wrong — an HTML attribute, where the browser decodes references but not CSS
// escapes.
func (h *hostReplacer) refsOnly(v []byte, value bool, ev *[]origin.Event) []byte {
	if h == nil || len(h.to) == 0 || bytes.IndexByte(v, '&') < 0 {
		return v
	}
	return h.spliceHostsIn(stripForRefs(v), v, urlTokenStarts, value, ev)
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

func (h *hostReplacer) spliceHosts(v []byte, starts func([]byte) []int, value bool, ev *[]origin.Event) []byte {
	return h.spliceHostsIn(stripForURL(v), v, starts, value, ev)
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
func (h *hostReplacer) spliceHostsIn(n normalised, v []byte, starts func([]byte) []int, value bool, ev *[]origin.Event) []byte {
	out, events := h.spliceHostsLog(n, v, starts, value)
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
func (h *hostReplacer) spliceHostsLog(n normalised, v []byte, starts func([]byte) []int, value bool) ([]byte, []origin.Event) {
	var out []byte
	var events []origin.Event
	prev := 0
	for _, off := range starts(n.b) {
		if off < len(n.pos) && n.pos[off] < prev {
			continue
		}
		from, until, repl, ok := h.locateHostIn(n, off, value)
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
	if w.hosts == nil || len(w.hosts.to) == 0 || bytes.IndexByte(v, '%') < 0 {
		return v
	}
	out, events := w.hosts.spliceHostsLog(stripForPercent(v), v, urlTokenStarts, value)
	w.record(surface, base, events)
	return out
}

// jsonEscLeak is the JSON-escape view as a recorded HTML-surface catcher, for
// the spelling WordPress's own block serializer emits.
func (w *HTML) jsonEscLeak(surface string, base int, v []byte, value bool) []byte {
	if w.hosts == nil || len(w.hosts.to) == 0 || !hasJSONEsc(v) {
		return v
	}
	out, events := w.hosts.spliceHostsLog(stripForJSONEsc(v), v, urlTokenStarts, value)
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
	if w.hosts == nil || len(w.hosts.to) == 0 || bytes.IndexByte(v, '&') < 0 {
		return v
	}
	out, events := w.hosts.spliceHostsLog(stripForRefs(v), v, urlTokenStarts, value)
	w.record(surface, base, events)
	return out
}

// refsCSSLeak is stripForRefsCSS as a recorded HTML-surface catcher, for a style
// surface whose references the parser decodes before the CSS tokenizer runs.
func (w *HTML) refsCSSLeak(surface string, base int, v []byte) []byte {
	if w.hosts == nil || len(w.hosts.to) == 0 || bytes.IndexByte(v, '&') < 0 {
		return v
	}
	n, ok := refsThenCSS(v)
	if !ok {
		return v
	}
	out, events := w.hosts.spliceHostsLog(n, v, urlTokenStarts, true)
	w.record(surface, base, events)
	return out
}

// cssEscapeLeak is the CSS-tokenizer view of a style surface.
func (w *HTML) cssEscapeLeak(surface string, base int, v []byte) []byte {
	if w.hosts == nil || len(w.hosts.to) == 0 || bytes.IndexByte(v, '\\') < 0 {
		return v
	}
	out, events := w.hosts.spliceHostsLog(stripForCSS(v), v, urlTokenStarts, true)
	w.record(surface, base, events)
	return out
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
	return hostsFor(m).rewriteAll(b, value, nil)
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
	return hostsFor(m).rewriteAllRefs(b, true, nil)
}

func HostLeaksXML(m *origin.Matcher, b []byte, value bool) []byte {
	if m == nil || len(b) == 0 {
		return b
	}
	return hostsFor(m).rewriteAllRefs(b, value, nil)
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
		return hostsFor(m).rewriteAll(b, value, ev)
	})
}

// HostLeaksBackCounted is HostLeaksBack with the census wired up.
func HostLeaksBackCounted(m *origin.Matcher, b []byte, st *Stats, surface string, base int) []byte {
	if m == nil || len(b) == 0 {
		return b
	}
	return counted(st, surface, base, func(ev *[]origin.Event) []byte {
		return hostsFor(m).rewriteAllRefs(b, true, ev)
	})
}

// HostLeaksXMLCounted is HostLeaksXML with the census wired up.
func HostLeaksXMLCounted(m *origin.Matcher, b []byte, value bool, st *Stats, surface string, base int) []byte {
	if m == nil || len(b) == 0 {
		return b
	}
	return counted(st, surface, base, func(ev *[]origin.Event) []byte {
		return hostsFor(m).rewriteAllRefs(b, value, ev)
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

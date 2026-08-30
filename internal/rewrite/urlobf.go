package rewrite

import (
	"bytes"
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
}

func newHostReplacer(m *origin.Matcher) *hostReplacer {
	h := &hostReplacer{to: map[string]origin.Origin{}, schemes: map[string]bool{}}
	for _, p := range m.Pairs() {
		if p.Identity() {
			continue
		}
		h.to[p.Canonical.HostPort()] = p.Variant
		h.schemes[p.Variant.Scheme] = true
	}
	return h
}

// sameSchemeAsDocument reports whether a reference written with this scheme is
// resolved against a base of the same scheme, which is what decides whether
// `https:host` is an authority or a relative path.
//
// When the map spans both schemes the answer is unknowable from here, and the
// safe reading is "not the same" — that treats more references as authorities,
// which can only ever rewrite an origin that is already in the map.
func (h *hostReplacer) sameSchemeAsDocument(scheme string) bool {
	return len(h.schemes) == 1 && h.schemes[scheme]
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
	s := strings.TrimSuffix(strings.ToLower(string(b)), ".")
	if a, err := origin.HostFold(s); err == nil {
		return a
	}
	return s
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
	case '-', '.', '_', '~', '%', '+', '@', ':':
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
	n := normalised{b: make([]byte, 0, len(dec)), pos: make([]int, 0, len(dec)), end: make([]int, 0, len(dec))}
	for i := 0; i < len(dec); i++ {
		if isURLStripped(dec[i]) {
			continue
		}
		n.b = append(n.b, dec[i])
		n.pos = append(n.pos, pos[i])
		n.end = append(n.end, end[i])
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
	for _, s := range []string{"https:", "http:"} {
		if at >= len(s) && hasFoldPrefixASCII(b[at-len(s):at], s) {
			return strings.TrimSuffix(s, ":")
		}
	}
	for s := range h.schemes {
		return s
	}
	return "https"
}

func (h *hostReplacer) authorityStart(b []byte) int {
	if n, scheme := schemeLen(b); n > 0 {
		i := n
		for i < len(b) && isSlashish(b[i]) {
			i++
		}
		if i-n >= 2 || !h.sameSchemeAsDocument(scheme) {
			return i
		}
		return -1
	}
	i := 0
	for i < len(b) && isSlashish(b[i]) {
		i++
	}
	if i >= 2 {
		return i
	}
	return -1
}

// hostRange returns the byte range of the host in b, starting at the authority.
//
// Userinfo is skipped: everything up to the *last* '@' before the authority
// ends belongs to the credentials, so `https://user@host` names host and not
// user. The authority ends at the first '/', '\', '?' or '#'; the host itself
// also ends at ':', which begins the port.
// maxHost bounds the authority scan. A DNS name is at most 253 octets, and
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
const maxHost = 253 * 3

func hostRange(b []byte, at int) (start, end int, port string) {
	end = len(b)
	if lim := at + maxHost; lim < end {
		end = lim
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
	}
	start = at
	if k := bytes.LastIndexByte(b[at:end], '@'); k >= 0 {
		start = at + k + 1
	}
	if k := bytes.IndexByte(b[start:end], ':'); k >= 0 {
		port = string(b[start+k+1 : end])
		end = start + k
	}
	return start, end, port
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
func urlTokenStarts(v []byte) []int {
	var out []int
	for i := 0; i < len(v); i++ {
		if i > 0 {
			switch c := v[i-1]; {
			case c <= 0x20, c == ',', c == '(', c == '=', c == '"', c == '\'', c == ';':
			default:
				continue
			}
		}
		if n, _ := schemeLen(v[i:]); n > 0 {
			out = append(out, i)
			continue
		}
		if isSlashish(v[i]) {
			out = append(out, i)
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
func (h *hostReplacer) locateHostIn(n normalised, at int, value bool) (from, until int, to origin.Origin, ok bool) {
	// The scheme decides which port is the default, so it has to be found
	// wherever the caller entered. foldedHostLeak enters at the slash *run*, so
	// looking only forwards saw no scheme and fell back to https — and
	// `http://h:443`, whose 443 is not http's default and so is a different
	// origin, was rewritten.
	scheme := h.schemeAt(n.b, at)
	rel := h.authorityStart(n.b[at:])
	if rel < 0 {
		return 0, 0, to, false
	}
	start := at + rel
	if start >= len(n.b) {
		return 0, 0, to, false
	}
	hs, he, port := hostRange(n.b, start)
	// A trailing dot is the host's root label inside a URL and a full stop in
	// prose, and only the caller knows which surface it is on — the same
	// distinction Matcher.RewriteText exists for. Absorbing it in a text node
	// would eat the sentence's punctuation.
	if !value && he > hs && n.b[he-1] == '.' {
		he--
	}
	if hs >= he {
		return 0, 0, to, false
	}
	host := h.key(percentDecode(n.b[hs:he]))
	// host:port first. Backwards, the bare-host pair won and an explicit
	// :8080 origin was rewritten to the wrong variant — while the byte matcher,
	// which disambiguates by port, got the same input right, so the two halves
	// of the engine disagreed. §5.4 says matching is on exact origin equality,
	// and :8080 is a different origin.
	// host:port first, and the bare host only when the port is the scheme's
	// default. §5.4 matches on exact origin equality, so `https://h:80` is a
	// different origin from `https://h` and rewriting it was a false positive —
	// one the byte matcher, which disambiguates by port, never made.
	if port != "" {
		to, ok = h.to[host+":"+port]
		if !ok && origin.NormalisePort(scheme, port) == "" {
			to, ok = h.to[host]
		}
	} else {
		to, ok = h.to[host]
	}
	if !ok {
		return 0, 0, to, false
	}
	// Whatever the original spelled the host with — a tab, a reference, a
	// percent escape — the replaced range covers all of it, because pos maps
	// every surviving byte back and the removed ones lie between them.
	return n.pos[hs], n.end[he-1], to, true
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
		if run-i < 2 || n.pos[i] < prev {
			i = run - 1
			continue
		}
		from, until, to, ok := w.hosts.locateHostIn(n, i, value)
		if !ok {
			i = run - 1
			continue
		}
		// Nothing to do when the bytes already say the variant, and nothing to
		// do when the host is plain ASCII — that is the byte matcher's job, and
		// it has already run.
		if !bytes.Equal(v[from:until], []byte(to.Host)) && hasNonASCII(v[from:until]) {
			out = append(out, v[prev:from]...)
			out = append(out, to.Host...)
			prev = until
			w.stats.Record(surface, base, []origin.Event{{
				Offset:  base + from,
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
		from, until, to, ok := w.hosts.locateHostIn(n, off, value)
		if !ok {
			continue
		}
		out = append(out, v[prev:from]...)
		out = append(out, to.Host...)
		prev = until
		w.stats.Record(surface, base, []origin.Event{{
			Offset:  base + from,
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
func (h *hostReplacer) rewriteAll(v []byte, value bool) []byte {
	if h == nil || len(h.to) == 0 {
		return v
	}
	v = h.spliceHosts(v, urlTokenStarts, value)
	if hasNonASCII(v) {
		v = h.spliceHosts(v, slashRunStarts, value)
	}
	return v
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
		if run-i >= 2 {
			out = append(out, i)
		}
		i = run - 1
	}
	return out
}

func (h *hostReplacer) spliceHosts(v []byte, starts func([]byte) []int, value bool) []byte {
	return h.spliceHostsIn(stripForURL(v), v, starts, value)
}

func (h *hostReplacer) spliceHostsIn(n normalised, v []byte, starts func([]byte) []int, value bool) []byte {
	var out []byte
	prev := 0
	for _, off := range starts(n.b) {
		if off < len(n.pos) && n.pos[off] < prev {
			continue
		}
		from, until, to, ok := h.locateHostIn(n, off, value)
		if !ok {
			continue
		}
		if from < prev {
			continue
		}
		out = append(out, v[prev:from]...)
		out = append(out, to.Host...)
		prev = until
	}
	if out == nil {
		return v
	}
	return append(out, v[prev:]...)
}

// cssEscapeLeak is the CSS-tokenizer view of a style surface.
func (w *HTML) cssEscapeLeak(v []byte) []byte {
	if w.hosts == nil || len(w.hosts.to) == 0 || bytes.IndexByte(v, '\\') < 0 {
		return v
	}
	return w.hosts.spliceHostsIn(stripForCSS(v), v, urlTokenStarts, true)
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
	return hostsFor(m).rewriteAll(b, value)
}

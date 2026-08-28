package origin

import (
	"fmt"
	"sort"
	"strings"

	aho "github.com/petar-dambovaliev/aho-corasick"
	"golang.org/x/net/idna"
)

// Pair is one canonical→variant mapping. Under an identity map the two are equal
// and the matcher never splices — which is what makes test 24 hold by
// construction rather than by luck.
type Pair struct {
	Canonical Origin
	Variant   Origin
	// Name is the site/blog label, for diagnostics only.
	Name string
}

// Identity reports whether this pair rewrites to itself.
func (p Pair) Identity() bool { return p.Canonical.Equal(p.Variant) }

// encoding is how the "://" or "//" separator is spelled at the match site.
// PLAN §4.4 requires all three: raw, JSON-escaped, and percent-encoded.
type encoding int

const (
	encRaw     encoding = iota // https://h      //h
	encJSON                    // https:\/\/h    \/\/h
	encPercent                 // https%3A%2F%2Fh  %2F%2Fh
)

func (e encoding) schemeSep() string {
	switch e {
	case encJSON:
		return `:\/\/`
	case encPercent:
		return "%3A%2F%2F"
	}
	return "://"
}

func (e encoding) relSep() string {
	switch e {
	case encJSON:
		return `\/\/`
	case encPercent:
		return "%2F%2F"
	}
	return "//"
}

// hostPort renders host[:port] for this encoding. Only the port colon needs
// encoding; hosts are ASCII after punycode.
func (e encoding) hostPort(o Origin) string {
	if o.Port == "" {
		return o.Host
	}
	if e == encPercent {
		return o.Host + "%3A" + o.Port
	}
	return o.Host + ":" + o.Port
}

// pattern is one automaton entry: a left-anchored origin prefix plus a host.
type pattern struct {
	enc      encoding
	relative bool   // true for "//h", where the scheme is genuinely unknown
	scheme   string // "" when relative
	host     string
	// pairs holds every map entry sharing this host. Port disambiguates between
	// them at match time, so a map may legitimately carry https://h and
	// https://h:8080 as separate origins.
	pairs []int
}

// Matcher finds canonical origins in a bounded byte slice and splices variants
// over them.
//
// Matching is on anchored *origins*, never bare hosts (PLAN §4.4). That is what
// makes the leftmost-label variant scheme safe: "acmecorp.ddev.site" is
// canonical and "wt-a--acmecorp.ddev.site" contains it, but "//acmecorp.ddev.site"
// does not occur inside "//wt-a--acmecorp.ddev.site", so a second pass is a
// fixed point. The unanchored bytes.ReplaceAll in spike/ is the double-port bug
// in a new costume; this replaces it.
type Matcher struct {
	pairs  []Pair
	pats   []pattern
	ac     aho.AhoCorasick
	ident  bool // every pair maps to itself
	maxPat int
}

// MaxMatchLen is the longest run of bytes a single match can span: the longest
// pattern, plus room for a trailing root dot, ":port" and the delimiter that
// terminates it. The streaming straggler sweep uses it to size its carry-over
// window, so that no match can straddle a chunk boundary.
func (m *Matcher) MaxMatchLen() int { return m.maxPat + 16 }

// NewMatcher builds the automaton for a set of canonical→variant pairs.
func NewMatcher(pairs []Pair) (*Matcher, error) {
	if len(pairs) == 0 {
		return nil, fmt.Errorf("origin: empty map")
	}
	m := &Matcher{pairs: pairs, ident: true}
	for _, p := range pairs {
		if !p.Identity() {
			m.ident = false
			break
		}
	}

	// One pattern per (encoding, form, host); pairs sharing a host are collected
	// so that the port can pick between them at match time.
	byKey := map[string]int{}
	hosts := map[string][]int{}
	for i, p := range pairs {
		hosts[p.Canonical.Host] = append(hosts[p.Canonical.Host], i)
	}
	hostNames := make([]string, 0, len(hosts))
	for h := range hosts {
		hostNames = append(hostNames, h)
	}
	sort.Strings(hostNames) // deterministic pattern ids

	var texts []string
	add := func(text string, p pattern) {
		if _, seen := byKey[text]; seen {
			return
		}
		byKey[text] = len(m.pats)
		m.pats = append(m.pats, p)
		texts = append(texts, text)
		m.maxPat = max(m.maxPat, len(text))
	}
	for _, h := range hostNames {
		idx := hosts[h]
		// Both labels. Origin.Parse normalises to the A-label, but WordPress
		// does not punycode siteurl — a site declared as https://hämeen.fi has
		// the U-label in its database and in every rendered page, so building
		// patterns from the A-label alone matched nothing at all and the whole
		// page leaked. §5.5 calls IDN "real for .fi client domains".
		forms := []string{h}
		if u, err := idna.Punycode.ToUnicode(h); err == nil && u != h {
			// The automaton folds ASCII case only, so a non-ASCII letter has to
			// be carried in both cases explicitly. That covers "hämeen.fi" and
			// "HÄMEEN.FI"; the ASCII folding covers every mix of the ASCII
			// letters around them. A host with only *some* of its non-ASCII
			// letters capitalised is not matched, which no real siteurl is.
			forms = append(forms, u)
			if up := strings.ToUpper(u); up != u {
				forms = append(forms, up)
			}
		}
		for _, hf := range forms {
			for _, enc := range []encoding{encRaw, encJSON, encPercent} {
				// Both schemes for every canonical host: the fleet's databases carry
				// http:// forms of hosts declared as https (M0 measured
				// nat.acmecorp.ddev.site appearing 165 times over http and zero over
				// https). A host-keyed map would be wrong; matching both schemes and
				// replacing with the *variant's* declared origin is right.
				for _, scheme := range []string{"https", "http"} {
					add(scheme+enc.schemeSep()+hf, pattern{enc: enc, scheme: scheme, host: hf, pairs: idx})
				}
				add(enc.relSep()+hf, pattern{enc: enc, relative: true, host: hf, pairs: idx})
			}
		}
	}

	b := aho.NewAhoCorasickBuilder(aho.Opts{
		AsciiCaseInsensitive: true, // PLAN §5.5: hosts and schemes compare case-insensitively
		MatchKind:            aho.LeftMostLongestMatch,
		DFA:                  true,
	})
	m.ac = b.Build(texts)
	return m, nil
}

// Identity reports whether every pair maps to itself, so no rewrite can occur.
func (m *Matcher) Identity() bool { return m.ident }

// Validate asserts PLAN §5.4's startup invariants. The important one is not a
// substring ban: it is that running this automaton over each *variant* origin
// yields zero matches. That is what permits a variant host to contain a
// canonical host, which the leftmost-label prefix scheme requires.
func (m *Matcher) Validate() error {
	seen := map[Origin]string{}
	rev := map[Origin]string{}
	for _, p := range m.pairs {
		if prev, dup := seen[p.Canonical]; dup {
			return fmt.Errorf("origin: canonical %s declared by both %q and %q", p.Canonical, prev, p.Name)
		}
		seen[p.Canonical] = p.Name
		// Injectivity is asserted between *sites*, not between pairs. A site
		// legitimately maps several canonical origins to one variant — §4.2's
		// {production_i, staging_i, ddev_i} → variant_i — so that residual URLs
		// left by an imperfect db:pull are corrected too. Two different sites
		// sharing a variant is the error.
		if prev, dup := rev[p.Variant]; dup && prev != p.Name {
			return fmt.Errorf("origin: variant %s produced by both %q and %q — map is not injective", p.Variant, prev, p.Name)
		}
		rev[p.Variant] = p.Name
	}
	if m.ident {
		return nil // an identity map is trivially anchored against itself
	}
	for _, p := range m.pairs {
		for _, enc := range []encoding{encRaw, encJSON, encPercent} {
			probe := p.Variant.Scheme + enc.schemeSep() + enc.hostPort(p.Variant)
			out, _, ev := m.rewrite([]byte(probe), len(probe), NoPrev, "validate", false)
			if string(out) != probe {
				return fmt.Errorf("origin: variant %s is matched by a canonical origin — rewriting it yields %q; "+
					"the map is not a fixed point and would double-rewrite (PLAN §5.4)", p.Variant, out)
			}
			for _, e := range ev {
				if e.Action == ActionRewrote {
					return fmt.Errorf("origin: variant %s matches canonical %s (PLAN §5.4)", p.Variant, e.Text)
				}
			}
		}
	}
	return nil
}

// Pairs returns the map, for diagnostics (`hostshift map`).
func (m *Matcher) Pairs() []Pair { return m.pairs }

// Action and Reason values for --explain (PLAN §5.8).
const (
	ActionRewrote = "rewrote"
	ActionSkipped = "skipped"

	ReasonNotAURL       = "not-a-url"       // no delimiter after the host: it is a longer host, or prose
	ReasonHostNotInMap  = "host-not-in-map" // anchored origin, but its port makes it a different origin
	ReasonIdentityMap   = "identity-map"    // canonical == variant, so there is nothing to change
	ReasonUnanchored    = "unanchored"      // protocol-relative match that is a path segment, not an origin
	ReasonSelfRedirect  = "self-redirect"   // PLAN §4.4 / test 32, used by the proxy
	ReasonSizeCap       = "size-cap-exceeded"
	ReasonNotDecodable  = "encoding-not-decodable"
	ReasonDepthExceeded = "depth-limit"
)

// Event is one --explain trace entry. Offset is a cumulative *input*-stream
// offset, so it stays stable across a length-changing rewrite (PLAN §4.4).
type Event struct {
	Offset  int    `json:"offset"`
	Surface string `json:"surface"`
	Text    string `json:"text"`
	Action  string `json:"action"`
	Reason  string `json:"reason,omitempty"`
}

// isHostByte reports whether a byte can appear in a hostname. Everything else
// terminates one.
//
// This replaces the enumerated delimiter list §4.4 sketches. An allowlist of
// terminators is the same mistake as an allowlist of attributes: it guarantees a
// long tail of misses, and the tail was real — ';' is how a CSP source list ends
// a directive, so `default-src 'self' https://canonical; script-src …` was left
// untouched and the delivered policy named only the canonical host, blocking
// every resource on the variant. ')' closes `@import url(…)`. ']' and '|' and
// '=' were missing too. Asking what a hostname *can* contain has no tail.
func isHostByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	case c == '-' || c == '.' || c == '_':
		return true
	}
	return false
}

// delimAt implements PLAN §4.4's right-hand anchor: a match is only an origin if
// what follows the host terminates it. End of input terminates.
//
// '%' needs care rather than a blanket yes. It is not a host byte, but in the
// percent encoding it introduces one: "%2E" is a dot and "%2D" a hyphen, so
//
//	https://www.example.com%2Eattacker.test/p
//
// is a URL whose real host is www.example.com.attacker.test — a different
// registrable domain, which this code correctly refuses when the dot is
// written literally. Treating '%' as an unconditional terminator rewrote it.
// So the escape is decoded and the same question asked of the byte it denotes.
func delimAt(b []byte, i int) bool {
	if i >= len(b) {
		return true
	}
	if b[i] == '%' {
		if c, ok := unhex(b, i+1); ok {
			return !isHostByte(c)
		}
		return true // a stray '%' is not a host byte either
	}
	return !isHostByte(b[i])
}

// unhex decodes the two hex digits at i.
func unhex(b []byte, i int) (byte, bool) {
	if i+1 >= len(b) {
		return 0, false
	}
	hi, ok1 := hexVal(b[i])
	lo, ok2 := hexVal(b[i+1])
	if !ok1 || !ok2 {
		return 0, false
	}
	return hi<<4 | lo, true
}

func hexVal(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// okBeforeRelative guards the left side of a protocol-relative match. "//host"
// is only an origin at the start of a value or after a separator; inside
// "https://other.example//host" or "path//host" it is a path segment.
//
// Explicit-scheme matches need no such guard: "https://" is its own anchor.
func okBeforeRelative(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return false
	case c == '/' || c == '.' || c == '-' || c == '_' || c == '%' || c == '\\':
		return false
	}
	return true
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// hostTerminated reports whether the host ends at end, allowing one root dot
// to sit between it and the delimiter.
//
// Which is the whole reason the dot is not simply consumed. In prose —
// "Read more at https://www.example.fi. Thanks" — the dot is a full stop, and
// swallowing it into the span deletes a character from the rendered page,
// because the variant is written in its root-less form. Rejecting the match
// instead is worse: the origin then goes out unrewritten, dereferenceable, and
// M0 counted five of those in acmecorp' database. So the dot terminates the
// host and stays where it is, and only the endsHost case above — where real
// URL structure follows, so the dot is genuinely the root label — absorbs it.
func hostTerminated(b []byte, end int) bool {
	if delimAt(b, end) {
		return true
	}
	return b[end] == '.' && delimAt(b, end+1)
}

// endsHost reports whether position i is where a URL's host component ends —
// end of buffer, or one of the characters that can follow an authority. It is
// deliberately narrower than delimAt, which answers the different question of
// whether a host has been cut short by any non-host byte.
func endsHost(b []byte, i int) bool {
	if i >= len(b) {
		return true
	}
	switch b[i] {
	case '/', ':', '?', '#':
		return true
	case '%':
		if c, ok := unhex(b, i+1); ok {
			switch c {
			case '/', ':', '?', '#':
				return true
			}
		}
	}
	return false
}

// Rewrite replaces every canonical origin in b with its variant, treating b as
// a complete value.
//
// It returns b itself when nothing changed, so an unmodified value is not merely
// equal to the input but the same bytes.
func (m *Matcher) Rewrite(b []byte, surface string, explain bool) (out []byte, events []Event) {
	out, _, events = m.rewrite(b, len(b), NoPrev, surface, explain)
	return out, events
}

// NoPrev is the prev argument for a buffer that begins at the start of its
// value or stream, where there is no preceding byte to anchor against.
const NoPrev = -1

// RewritePrefix is Rewrite for a stream, where more bytes are still to come.
//
// Only matches beginning before limit are considered, so that a match cannot
// straddle the end of the buffer and be decided on bytes that have not arrived.
// Callers pass limit = len(b) - MaxMatchLen() while more input is expected, and
// limit = len(b) at EOF.
//
// It returns the rewritten prefix and how many bytes of b it consumed, which is
// at least limit and never less; the caller retains b[consumed:] for the next
// round. This is what lets §4.4's straggler sweep run in-stream: a match is
// replaced before its bytes are emitted, so nothing already written needs
// re-aligning and the body is never buffered whole.
//
// prev is the byte immediately before b[0] in the stream, or NoPrev at the very
// start. The carry-over window protects a match from being decided on bytes
// that have not arrived on the *right*; prev is the same protection on the
// left, and it is not optional. A protocol-relative "//host" is only an origin
// after a separator, so the byte before it decides the match — and that byte
// has usually already been emitted and dropped by the time the rest arrives.
// Passing NoPrev there says "start of stream", which anchors, so the same
// document rewrote differently depending on where the 32 KiB read boundary
// happened to fall.
func (m *Matcher) RewritePrefix(b []byte, limit int, prev int, surface string, explain bool) (out []byte, consumed int, events []Event) {
	return m.rewrite(b, limit, prev, surface, explain)
}

func (m *Matcher) rewrite(b []byte, limit int, prev int, surface string, explain bool) ([]byte, int, []Event) {
	if limit < 0 {
		limit = 0
	}
	if len(b) == 0 {
		return b, 0, nil
	}
	var (
		events []Event
		buf    []byte
		last   int
	)
	// Every candidate is emitted, not only rewrites and not only under
	// --explain. Recording skips exclusively when explaining meant the `--json`
	// counters always showed candidates == rewrites, which reads as "nothing was
	// skipped" when in fact skips were simply never counted — the one number a
	// reader would use to decide whether the map is missing a host.
	emit := func(off int, text, action, reason string) {
		events = append(events, Event{Offset: off, Surface: surface, Text: text, Action: action, Reason: reason})
	}

	// The library's findIter advances with `pos = end - len + 1`, i.e. one byte
	// past the match *start*, not past its end — so it yields overlapping
	// matches even under LeftMostLongestMatch. Starts are therefore strictly
	// increasing, and non-overlapping leftmost-longest semantics are recovered
	// by discarding any match that begins inside one already considered.
	//
	// Without this, "https://h" is matched, and then "//h" is matched again six
	// bytes inside it — which is both a double rewrite and, since the splice
	// bookkeeping runs backwards, a panic.
	scanned := 0

	// consumed is how far into b the caller may advance. It starts at limit and
	// grows to cover any match that began before limit but extends past it, so
	// no match is ever split across two rounds of a streaming scan.
	consumed := limit

	iter := m.ac.IterByte(b)
	for match := iter.Next(); match != nil; match = iter.Next() {
		p := m.pats[match.Pattern()]
		start, hostEnd := match.Start(), match.End()
		if start >= limit {
			// Beyond the safe point: the bytes that decide this match may not
			// have arrived yet. Leave it for the next round.
			break
		}
		if start < scanned {
			continue // shadowed by a longer match that began earlier
		}
		scanned = hostEnd

		if p.relative {
			// The byte before the match, which for start == 0 lives in the
			// caller's stream rather than in b.
			c, have := prev, prev >= 0
			if start > 0 {
				c, have = int(b[start-1]), true
			}
			if have && !okBeforeRelative(byte(c)) {
				emit(start, string(b[start:hostEnd]), ActionSkipped, ReasonUnanchored)
				continue
			}
		}

		end, port := hostEnd, ""

		// A trailing root dot is part of the host: "www.example.fi." is the
		// same host as "www.example.fi" and a browser dereferences it
		// identically, so leaving it unrewritten is a test 28 leak. M0 found
		// five of them in acmecorp' production database.
		//
		// It is consumed into the span and not re-emitted, because the variant
		// is written in its own root-less form — which is why what follows has
		// to be URL structure and not merely a delimiter. Accepting any
		// delimiter ate the full stop in "Read more at https://www.example.fi.
		// Thanks", deleting a character from rendered prose; that is a corpus
		// diff, and prose is exactly where a bare origin followed by a period
		// is common. "c.example.evil/" still falls through to the not-a-url
		// rejection either way.
		//
		// End of buffer counts as structure, so a value that is nothing but an
		// origin keeps working. The residue is a text node ending in a URL and
		// a full stop with nothing after it, which still loses the stop.
		//
		// When the dot is *not* consumed the match still stands — see
		// hostTerminated. Rejecting it instead would leave the whole origin
		// unrewritten, which is the test 28 leak this rule exists to close.
		if end < len(b) && b[end] == '.' && endsHost(b, end+1) {
			end++
		}

		// Optional port, in either spelling. The percent form matters: "%3A" is
		// how a port separator appears inside redirect_to= and friends, and
		// reading only ':' meant "https%3A%2F%2Fh%3A8443%2Fx" had its port
		// treated as a *terminator* — so an origin on a different port was
		// rewritten to the variant host. That is the port family of the
		// double-port bug this design exists to avoid.
		sep := 0
		if end < len(b) && b[end] == ':' {
			sep = 1
		} else if end < len(b) && b[end] == '%' {
			if c, ok := unhex(b, end+1); ok && c == ':' {
				sep = 3
			}
		}
		if sep > 0 {
			j := end + sep
			for j < len(b) && isDigit(b[j]) {
				j++
			}
			if j > end+sep {
				port, end = string(b[end+sep:j]), j
			}
		}
		scanned = end
		consumed = max(consumed, end)
		if !hostTerminated(b, end) {
			// The host is a prefix of a longer host, or this is prose.
			emit(start, string(b[start:end]), ActionSkipped, ReasonNotAURL)
			continue
		}

		// Pick the pair whose port matches. Scheme is deliberately *not* part of
		// this comparison: the map is keyed on origin, but content carries both
		// schemes for the same host and both must be rewritten.
		var pair *Pair
		for _, i := range p.pairs {
			c := m.pairs[i]
			schemeForPort := p.scheme
			if p.relative {
				schemeForPort = c.Canonical.Scheme
			}
			if NormalisePort(schemeForPort, port) == c.Canonical.Port {
				pair = &m.pairs[i]
				break
			}
		}
		if pair == nil {
			emit(start, string(b[start:end]), ActionSkipped, ReasonHostNotInMap)
			continue
		}
		if pair.Identity() {
			emit(start, string(b[start:end]), ActionSkipped, ReasonIdentityMap)
			continue
		}

		var repl string
		if p.relative {
			repl = p.enc.relSep() + p.enc.hostPort(pair.Variant)
		} else {
			repl = pair.Variant.Scheme + p.enc.schemeSep() + p.enc.hostPort(pair.Variant)
		}
		if repl == string(b[start:end]) {
			emit(start, repl, ActionSkipped, ReasonIdentityMap)
			continue
		}

		if buf == nil {
			buf = make([]byte, 0, len(b)+len(b)/8)
		}
		buf = append(buf, b[last:start]...)
		buf = append(buf, repl...)
		last = end
		emit(start, string(b[start:end]), ActionRewrote, "")
	}

	if buf == nil {
		if consumed == len(b) {
			return b, consumed, events // untouched: the same bytes, not a copy
		}
		return b[:consumed], consumed, events
	}
	buf = append(buf, b[last:consumed]...)
	return buf, consumed, events
}

// String renders the map for `hostshift map`.
func (m *Matcher) String() string {
	var sb strings.Builder
	for _, p := range m.pairs {
		fmt.Fprintf(&sb, "%-12s %s -> %s\n", p.Name, p.Canonical, p.Variant)
	}
	return sb.String()
}

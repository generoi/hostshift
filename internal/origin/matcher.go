package origin

import (
	"fmt"
	"sort"
	"strings"

	aho "github.com/petar-dambovaliev/aho-corasick"
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
	pairs []Pair
	pats  []pattern
	ac    aho.AhoCorasick
	ident bool // every pair maps to itself
}

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
	}
	for _, h := range hostNames {
		idx := hosts[h]
		for _, enc := range []encoding{encRaw, encJSON, encPercent} {
			// Both schemes for every canonical host: the fleet's databases carry
			// http:// forms of hosts declared as https (M0 measured
			// nat.acmecorp.ddev.site appearing 165 times over http and zero over
			// https). A host-keyed map would be wrong; matching both schemes and
			// replacing with the *variant's* declared origin is right.
			for _, scheme := range []string{"https", "http"} {
				add(scheme+enc.schemeSep()+h, pattern{enc: enc, scheme: scheme, host: h, pairs: idx})
			}
			add(enc.relSep()+h, pattern{enc: enc, relative: true, host: h, pairs: idx})
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
			out, ev := m.rewrite([]byte(probe), "validate", false)
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

// isDelim implements PLAN §4.4's right-hand anchor: a match is only an origin if
// what follows the host terminates it.
//
// Two additions to the set §4.4 lists, both required rather than defensive:
//   - '%' — without it the percent-encoded form can never match, since
//     "https%3A%2F%2Fhost%2Fpath" continues with '%'. That form is exactly the
//     redirect_to= case §5.5 names.
//   - ',' — srcset, ping and Link headers separate on it.
func isDelim(c byte) bool {
	switch c {
	case '/', ':', '?', '#', '"', '\'', '<', '>', '\\', '&', '%', ',',
		' ', '\t', '\n', '\r', '\f', '\v':
		return true
	}
	return false
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

// Rewrite replaces every canonical origin in b with its variant.
//
// It returns b itself when nothing changed, so an unmodified value is not merely
// equal to the input but the same bytes. base is added to every event offset so
// callers can report cumulative input-stream positions.
func (m *Matcher) Rewrite(b []byte, surface string, explain bool) (out []byte, events []Event) {
	return m.rewrite(b, surface, explain)
}

func (m *Matcher) rewrite(b []byte, surface string, explain bool) ([]byte, []Event) {
	if len(b) == 0 {
		return b, nil
	}
	var (
		events []Event
		buf    []byte
		last   int
	)
	emit := func(off int, text, action, reason string) {
		if explain || action == ActionRewrote {
			events = append(events, Event{Offset: off, Surface: surface, Text: text, Action: action, Reason: reason})
		}
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

	iter := m.ac.IterByte(b)
	for match := iter.Next(); match != nil; match = iter.Next() {
		p := m.pats[match.Pattern()]
		start, hostEnd := match.Start(), match.End()
		if start < scanned {
			continue // shadowed by a longer match that began earlier
		}
		scanned = hostEnd

		if p.relative && start > 0 && !okBeforeRelative(b[start-1]) {
			emit(start, string(b[start:hostEnd]), ActionSkipped, ReasonUnanchored)
			continue
		}

		// Optional :port. A ':' not followed by digits is itself a delimiter.
		end, port := hostEnd, ""
		if end < len(b) && b[end] == ':' {
			j := end + 1
			for j < len(b) && isDigit(b[j]) {
				j++
			}
			if j > end+1 {
				port, end = string(b[end+1:j]), j
			}
		}
		scanned = end
		if end < len(b) && !isDelim(b[end]) {
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
		return b, events // untouched: the same bytes, not a copy
	}
	buf = append(buf, b[last:]...)
	return buf, events
}

// String renders the map for `hostshift map`.
func (m *Matcher) String() string {
	var sb strings.Builder
	for _, p := range m.pairs {
		fmt.Fprintf(&sb, "%-12s %s -> %s\n", p.Name, p.Canonical, p.Variant)
	}
	return sb.String()
}

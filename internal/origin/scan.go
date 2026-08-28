package origin

import "bytes"

// Candidate finding without an automaton.
//
// Aho–Corasick is the right structure for thousands of patterns. This map has
// tens: nine blogs is 81 patterns, and most are the same handful of hosts spelled
// three ways. Against that, a DFA costs 2.9 ms and 257,000 allocations to build,
// allocates an iterator and a prefilter state on every IterByte call, and steps a
// transition table byte by byte through a document that is 99.8% uninteresting.
//
// The structure of the pattern set is what makes something simpler possible.
// Every pattern contains a separator — "//", "\/\/" or "%2F%2F" — and every
// explicit-scheme pattern *ends with* its relative form, because "https://H" is
// "https:" followed by "//H". So finding the separator finds every candidate: the
// host reads forwards from it and the scheme reads backwards. bytes.Index skips
// to the next separator at whatever speed the platform can manage, which on a
// 508 KB page with one separator per ~480 bytes is most of the document skipped
// without being looked at.
//
// This deliberately reproduces the automaton's *sequence*, not merely its
// results: leftmost-longest, one candidate per position, advancing one byte past
// each match start. The loop in rewrite() already handles overlap by discarding
// a match that begins inside one already considered, and that logic — like every
// other decision about anchoring, ports and the root dot — is untouched.

// scanForm is one host spelled one way, with the patterns that can precede it.
type scanForm struct {
	host  []byte
	https *pattern
	http  *pattern
	rel   *pattern
}

// candidate is what the automaton's Match carried: which pattern, and the span
// from the start of the match to the end of the host.
type candidate struct {
	p              *pattern
	start, hostEnd int
}

// scanIter walks the candidates in b, in the order the automaton yielded them.
type scanIter struct {
	m   *Matcher
	b   []byte
	pos int // where to look for the next separator

	// queued holds the candidates found at one separator, in start order —
	// scheme form first, since it begins earlier. The automaton yields both and
	// rewrite() discards the second as shadowed; reproducing that keeps the
	// event stream identical, skips and all.
	queued [2]candidate
	n, i   int
}

func (m *Matcher) scan(b []byte) *scanIter { return &scanIter{m: m, b: b} }

func (s *scanIter) Next() (candidate, bool) {
	for {
		if s.i < s.n {
			c := s.queued[s.i]
			s.i++
			return c, true
		}
		if !s.fill() {
			return candidate{}, false
		}
	}
}

// fill advances to the next separator that starts a candidate.
func (s *scanIter) fill() bool {
	for s.pos < len(s.b) {
		sep, enc, sepLen := s.m.nextSep(s.b, s.pos)
		if sep < 0 {
			s.pos = len(s.b)
			return false
		}
		// One byte past the match start, which is how the automaton advanced —
		// so a relative form nested inside a scheme form is offered and then
		// discarded, exactly as before.
		s.pos = sep + 1

		// sepLen is -1 when this was a lone "\/" or "%2F" rather than the start
		// of a separator: there is no separator here and nothing to read a host
		// after. Adding it as an offset probed one byte *before* the match, and
		// a one-byte canonical host matches there — a zero-width candidate the
		// automaton never yields. For a non-alphanumeric one-byte host it even
		// survived the anchor check and spliced bytes into a response the
		// automaton would have left alone.
		if sepLen < 0 {
			continue
		}

		f := s.m.hostAt(s.b, sep+sepLen, enc)
		if f == nil {
			continue
		}
		hostEnd := sep + sepLen + len(f.host)

		s.n, s.i = 0, 0
		// Scheme first: "https://H" starts before the "//H" inside it.
		if p, start := s.m.schemeBefore(s.b, sep, enc, f); p != nil {
			s.queued[s.n] = candidate{p: p, start: start, hostEnd: hostEnd}
			s.n++
		}
		if f.rel != nil {
			s.queued[s.n] = candidate{p: f.rel, start: sep, hostEnd: hostEnd}
			s.n++
		}
		if s.n > 0 {
			return true
		}
	}
	return false
}

// nextSep finds the next separator at or after from, and says which encoding
// spells it that way.
func (m *Matcher) nextSep(b []byte, from int) (at int, enc encoding, sepLen int) {
	best := -1
	for _, c := range []struct {
		lit []byte
		enc encoding
	}{
		{sepRaw, encRaw},
		{sepJSON, encJSON},
		{sepPercentUpper, encPercent},
		{sepPercentLower, encPercent},
	} {
		i := bytes.Index(b[from:], c.lit)
		if i < 0 {
			continue
		}
		if i += from; best < 0 || i < best {
			best, enc = i, c.enc
		}
	}
	if best < 0 {
		return -1, 0, 0
	}
	// The full separator is the doubled form: "//", "\/\/", "%2F%2F". The index
	// above found its first half.
	rel := []byte(enc.relSep())
	if !hasFoldPrefix(b[best:], rel) {
		// A lone "\/" or "%2F" that is not the start of a separator. Step past
		// its first byte so the search makes progress.
		return best, enc, -1
	}
	return best, enc, len(rel)
}

// hostAt returns the longest host form matching at i, or nil.
func (m *Matcher) hostAt(b []byte, i int, enc encoding) *scanForm {
	if i < 0 || i > len(b) {
		return nil
	}
	var best *scanForm
	for k := range m.byEnc[enc] {
		f := &m.byEnc[enc][k]
		if len(f.host) > len(b)-i {
			continue
		}
		if !hasFoldPrefix(b[i:], f.host) {
			continue
		}
		// Longest wins, which is what LeftMostLongestMatch does when two hosts
		// share a prefix.
		if best == nil || len(f.host) > len(best.host) {
			best = f
		}
	}
	return best
}

// schemeBefore reports the explicit-scheme pattern ending at sep, if the bytes
// before it spell one.
// schemePrefixes is "https"+schemeSep and "http"+schemeSep for each encoding,
// built once. Concatenating them per call allocated a string per candidate —
// about 1,100 on a corpus page, 19% of a whole request's allocations.
var schemePrefixes = [3][2][]byte{
	encRaw:     {[]byte("https" + "://"), []byte("http" + "://")},
	encJSON:    {[]byte(`https:\/\/`), []byte(`http:\/\/`)},
	encPercent: {[]byte("https%3A%2F%2F"), []byte("http%3A%2F%2F")},
}

func (m *Matcher) schemeBefore(b []byte, sep int, enc encoding, f *scanForm) (*pattern, int) {
	lits := &schemePrefixes[enc]
	for i, p := range [2]*pattern{f.https, f.http} {
		if p == nil {
			continue
		}
		lit := lits[i]
		// The separator is the tail of the scheme prefix, so the prefix starts
		// this far back.
		start := sep + len(enc.relSep()) - len(lit)
		if start < 0 {
			continue
		}
		if hasFoldPrefix(b[start:], lit) {
			return p, start
		}
	}
	return nil, 0
}

// hasFoldPrefix is bytes.HasPrefix with the automaton's case folding: ASCII
// only, which is what AsciiCaseInsensitive meant. Unicode folding would be
// wider than the thing it replaces, and a matcher must never be wider than what
// it claims to match.
func hasFoldPrefix(b, prefix []byte) bool {
	if len(b) < len(prefix) {
		return false
	}
	for i := 0; i < len(prefix); i++ {
		if lowerASCII(b[i]) != lowerASCII(prefix[i]) {
			return false
		}
	}
	return true
}

func lowerASCII(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + 'a' - 'A'
	}
	return c
}

package origin

import aho "github.com/petar-dambovaliev/aho-corasick"

// The Aho–Corasick matcher, kept as the oracle for scan.go and nowhere else.
//
// It is the implementation every audit was run against, so it is the right
// thing to check a replacement finder against — but it lives in a test file, so
// `go build` does not link it and the shipped binary carries neither the
// automaton nor 257,000 allocations of DFA construction per map.
//
// It is rebuilt from m.pats rather than kept on the Matcher, which also means
// the test cannot accidentally be comparing the scanner against itself.
func init() { acFinder = acCandidates }

// Built once per Matcher: the DFA costs ~3 ms and 257,000 allocations, which is
// the whole reason it is not in the binary, and rebuilding it per call put the
// differential suite at 133 seconds.
var acCache = map[*Matcher]aho.AhoCorasick{}

func acCandidates(m *Matcher, b []byte) func() (candidate, bool) {
	ac, built := acCache[m]
	if !built {
		ac = buildAC(m)
		acCache[m] = ac
	}
	iter := ac.IterByte(b)
	return func() (candidate, bool) {
		match := iter.Next()
		if match == nil {
			return candidate{}, false
		}
		return candidate{p: &m.pats[match.Pattern()], start: match.Start(), hostEnd: match.End()}, true
	}
}

func buildAC(m *Matcher) aho.AhoCorasick {
	texts := make([]string, len(m.pats))
	for i, p := range m.pats {
		if p.relative {
			texts[i] = p.enc.relSep() + p.host
		} else {
			texts[i] = p.scheme + p.enc.schemeSep() + p.host
		}
	}
	builder := aho.NewAhoCorasickBuilder(aho.Opts{
		AsciiCaseInsensitive: true,
		MatchKind:            aho.LeftMostLongestMatch,
		DFA:                  true,
	})
	return builder.Build(texts)
}

// rewriteAC is rewrite() with the automaton supplying candidates. The two share
// every decision after that — anchoring, ports, the root dot, replacement — so
// a divergence can only come from the finder.
func (m *Matcher) rewriteAC(b []byte, limit, prev int, value bool, surface string, explain bool) ([]byte, int, []Event) {
	m.acOracle = true
	defer func() { m.acOracle = false }()
	return m.rewrite(b, limit, prev, value, surface, explain)
}

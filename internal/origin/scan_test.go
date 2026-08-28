package origin

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// diffMatcher builds the same map twice, one running the scanner and one the
// automaton.
func diffMatcher(t *testing.T, pairs []Pair) (scan, ac *Matcher) {
	t.Helper()
	s, err := NewMatcher(pairs)
	if err != nil {
		t.Fatal(err)
	}
	a, err := NewMatcher(pairs)
	if err != nil {
		t.Fatal(err)
	}
	a.acOracle = true
	return s, a
}

func diffPairs(t *testing.T) []Pair {
	t.Helper()
	return []Pair{
		{Name: "main", Canonical: MustParse("https://www.herrfors.fi"), Variant: MustParse("https://wt-a--herrfors.ddev.site")},
		{Name: "nat", Canonical: MustParse("https://www.herrforsnat.fi"), Variant: MustParse("https://wt-a--nat.herrfors.ddev.site")},
		{Name: "ddev", Canonical: MustParse("https://herrfors.ddev.site"), Variant: MustParse("https://wt-a--herrfors.ddev.site")},
		{Name: "port", Canonical: MustParse("https://www.herrfors.fi:8443"), Variant: MustParse("http://v.ddev.site:8080")},
		{Name: "idn", Canonical: MustParse("https://hämeen.fi"), Variant: MustParse("https://wt-a--hameen.ddev.site")},
		{Name: "short", Canonical: MustParse("https://a.fi"), Variant: MustParse("https://b.fi")},
		// Deliberately a prefix of another canonical host, so leftmost-longest
		// has something to get wrong.
		{Name: "pfx", Canonical: MustParse("https://ex.test"), Variant: MustParse("https://p--ex.test")},
		{Name: "pfxlong", Canonical: MustParse("https://ex.test.uk"), Variant: MustParse("https://q--ex.test.uk")},
	}
}

// diffStats counts what the comparisons actually exercised, so a fuzz that
// never matched anything cannot pass for evidence.
var diffStats struct{ cases, withEvents, rewrites int }

// same asserts the two finders produce identical output, consumed count and
// event stream for one input.
func same(t *testing.T, scan, ac *Matcher, b []byte, limit, prev int, value bool) bool {
	t.Helper()
	gotOut, gotN, gotEv := scan.rewrite(b, limit, prev, value, "diff", true)
	wantOut, wantN, wantEv := ac.rewrite(b, limit, prev, value, "diff", true)

	diffStats.cases++
	if len(gotEv) > 0 {
		diffStats.withEvents++
	}
	for _, e := range gotEv {
		if e.Action == ActionRewrote {
			diffStats.rewrites++
		}
	}

	if string(gotOut) != string(wantOut) || gotN != wantN || len(gotEv) != len(wantEv) {
		t.Errorf("input %q limit=%d prev=%d value=%v\n scan out=%q n=%d ev=%d\n   ac out=%q n=%d ev=%d",
			b, limit, prev, value, gotOut, gotN, len(gotEv), wantOut, wantN, len(wantEv))
		return false
	}
	for i := range gotEv {
		if gotEv[i] != wantEv[i] {
			t.Errorf("input %q event %d:\n scan %+v\n   ac %+v", b, i, gotEv[i], wantEv[i])
			return false
		}
	}
	return true
}

// TestScanMatchesAutomatonOnShapes is the hand-written half: every shape the
// audits turned up, plus the ones the scanner could plausibly get wrong —
// scheme read backwards, a host that is a prefix of another, mixed hex case, a
// lone separator half.
func TestScanMatchesAutomatonOnShapes(t *testing.T) {
	scan, ac := diffMatcher(t, diffPairs(t))
	for _, in := range []string{
		"",
		"/",
		"//",
		"\\/",
		"%2F",
		"%2f",
		"https://www.herrfors.fi/x",
		"HTTPS://WWW.HERRFORS.FI/X",
		"http://www.herrfors.fi",
		"//www.herrfors.fi/",
		"//www.herrfors.fi",
		"https:\\/\\/www.herrfors.fi\\/x",
		"https%3A%2F%2Fwww.herrfors.fi%2Fx",
		"https%3a%2f%2fwww.herrfors.fi%2fx",
		"HTTPS%3A%2f%2Fwww.herrfors.fi",
		"path//www.herrfors.fi/x",
		"x//www.herrfors.fi",
		"https://www.herrfors.fi.",
		"https://www.herrfors.fi./p",
		"see https://www.herrfors.fi. Thanks",
		"https://www.herrfors.fi:8443/x",
		"https://www.herrfors.fi:9999/x",
		"https://www.herrfors.fi%3A8443%2Fx",
		"https://ex.test/x",
		"https://ex.test.uk/x",
		"https://ex.test.uk.other/x",
		"https://hämeen.fi/x",
		"https://HÄMEEN.FI/x",
		"https://xn--hmeen-loa.fi/x",
		"https://www.herrfors.fi.evil.example/x",
		"//www.herrfors.fihttps://www.herrfors.fi",
		"https://https://www.herrfors.fi",
		"\\/\\/www.herrfors.fi",
		"%2F%2Fwww.herrfors.fi",
		"a//a.fi//a.fi//a.fi",
		"<a href=\"https://www.herrfors.fi/a\">https://www.herrforsnat.fi</a>",
		strings.Repeat("//www.herrfors.fi/x ", 20),
	} {
		b := []byte(in)
		for _, value := range []bool{true, false} {
			for _, prev := range []int{NoPrev, int('/'), int('x'), int('"')} {
				for _, limit := range []int{len(b), len(b) / 2, 0, 1} {
					same(t, scan, ac, b, limit, prev, value)
				}
			}
		}
	}
}

// TestScanMatchesAutomatonOnCorpus runs both over every real page and
// adversarial fixture, at several limits and left contexts.
func TestScanMatchesAutomatonOnCorpus(t *testing.T) {
	scan, ac := diffMatcher(t, diffPairs(t))
	var files []string
	for _, dir := range []string{"../../spike/corpus", "../../spike/adv"} {
		got, err := filepath.Glob(filepath.Join(dir, "*.html"))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, got...)
	}
	if len(files) < 50 {
		t.Fatalf("expected the corpus and fixtures, found %d", len(files))
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, limit := range []int{len(b), len(b) - 1, len(b) / 3} {
			if !same(t, scan, ac, b, limit, NoPrev, true) {
				t.Fatalf("first divergence in %s", f)
			}
		}
		if !same(t, scan, ac, b, len(b), int('x'), false) {
			t.Fatalf("first divergence in %s (prose semantics)", f)
		}
	}
}

// TestScanMatchesAutomatonFuzz is the half that matters. The shapes above are
// the ones I thought of; this is the ones I did not.
func TestScanMatchesAutomatonFuzz(t *testing.T) {
	scan, ac := diffMatcher(t, diffPairs(t))
	// Composed, not concatenated. Sticking random fragments together almost
	// never lands a separator immediately before a host — the first attempt
	// produced a candidate in 502 of 40,000 cases, which would have proved
	// nothing at all. So most tokens are built as an origin: an optional
	// scheme, a separator, a host, a tail. The rest is noise around them.
	schemes := []string{"", "", "https", "http", "HTTPS", "hTtP", "xhttps", "shttp"}
	seps := [][2]string{ // scheme spelling, relative spelling
		{"://", "//"}, {":\\/\\/", "\\/\\/"},
		{"%3A%2F%2F", "%2F%2F"}, {"%3a%2f%2f", "%2f%2f"},
		{"%3A%2f%2F", "%2F%2f"},
	}
	hosts := []string{
		"www.herrfors.fi", "WWW.HERRFORS.FI", "Www.Herrfors.Fi",
		"www.herrforsnat.fi", "herrfors.ddev.site", "ex.test", "ex.test.uk",
		"a.fi", "hämeen.fi", "HÄMEEN.FI", "xn--hmeen-loa.fi",
		"www.herrfors.f", "www.herrfors.fix", "ex.tes", "a.f",
	}
	tails := []string{
		"", "/", "/x", ".", "./p", ".x", ":8443", ":80", ":9999", "%3A8443",
		"?q=1", "#f", "\"", "'", "<", ">", " ", "\n", "&", "&#47;", "%2Fx",
	}
	noise := []string{"", "x", "path", "a.fi", "/", "//", "\\/", "%2", "%", ":", "http", "-", "_", "\t"}

	origin := func(rng *rand.Rand) string {
		sep := seps[rng.Intn(len(seps))]
		sc := schemes[rng.Intn(len(schemes))]
		spelling := sep[1]
		if sc != "" {
			spelling = sep[0]
		}
		return sc + spelling + hosts[rng.Intn(len(hosts))] + tails[rng.Intn(len(tails))]
	}

	rng := rand.New(rand.NewSource(20260828))
	const n = 40000
	for i := 0; i < n; i++ {
		var sb strings.Builder
		for j := 0; j < 1+rng.Intn(4); j++ {
			if rng.Intn(4) > 0 {
				sb.WriteString(origin(rng))
			}
			sb.WriteString(noise[rng.Intn(len(noise))])
		}
		b := []byte(sb.String())
		limit := len(b)
		switch rng.Intn(4) {
		case 1:
			limit = rng.Intn(len(b) + 1)
		case 2:
			limit = 0
		}
		prev := NoPrev
		if rng.Intn(2) == 0 {
			prev = int("/x\"<. ="[rng.Intn(7)])
		}
		if !same(t, scan, ac, b, limit, prev, rng.Intn(2) == 0) {
			t.Fatalf("first divergence at case %d: %q", i, b)
		}
	}
	// A fuzz that matches nothing proves nothing.
	t.Logf("%d fuzz cases: %d produced candidates, %d rewrites",
		diffStats.cases, diffStats.withEvents, diffStats.rewrites)
	if diffStats.withEvents < n/2 {
		t.Fatalf("only %d of %d cases produced a candidate; the fuzz is not exercising the matcher",
			diffStats.withEvents, diffStats.cases)
	}
	if diffStats.rewrites == 0 {
		t.Fatal("no case produced a rewrite")
	}
}

// TestScanMatchesAutomatonRandomBytes: structured fuzz shares the vocabulary of
// the thing under test, so it cannot find a case neither side imagined. This
// can.
func TestScanMatchesAutomatonRandomBytes(t *testing.T) {
	scan, ac := diffMatcher(t, diffPairs(t))
	rng := rand.New(rand.NewSource(1))
	alphabet := []byte("/\\%23AaFfHhTtPpSs:.-_wWrRoOeEiI<>\"' \n\x00\xff")
	for i := 0; i < 20000; i++ {
		b := make([]byte, rng.Intn(64))
		for j := range b {
			b[j] = alphabet[rng.Intn(len(alphabet))]
		}
		if !same(t, scan, ac, b, len(b), NoPrev, true) {
			t.Fatalf("first divergence at case %d: %q", i, b)
		}
	}
}

// TestScanMatchesAutomatonAcrossMaps: the finder is built from the pattern set,
// so the pattern set is an input. Vary it.
func TestScanMatchesAutomatonAcrossMaps(t *testing.T) {
	maps := [][]Pair{
		{{Name: "one", Canonical: MustParse("https://a.test"), Variant: MustParse("https://b.test")}},
		{{Name: "ident", Canonical: MustParse("https://a.test"), Variant: MustParse("https://a.test")}},
		{{Name: "port", Canonical: MustParse("http://a.test:8080"), Variant: MustParse("https://b.test")}},
		{
			{Name: "a", Canonical: MustParse("https://x.test"), Variant: MustParse("https://wt--x.test")},
			{Name: "b", Canonical: MustParse("https://sub.x.test"), Variant: MustParse("https://wt--sub.x.test")},
		},
	}
	for i, pairs := range maps {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			scan, ac := diffMatcher(t, pairs)
			for _, in := range []string{
				"https://a.test/x", "//a.test", "https://sub.x.test/y", "https://x.test/y",
				"http://a.test:8080/z", "https:\\/\\/a.test", "https%3A%2F%2Fa.test",
				"//sub.x.test//x.test", "a.test//a.test",
			} {
				same(t, scan, ac, []byte(in), len(in), NoPrev, true)
			}
		})
	}
}

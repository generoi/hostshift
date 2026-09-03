package rewrite

import (
	"bytes"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
)

// Round 46, on 598de7c.

const (
	r46Canon = "www.example.fi"
	r46Var   = "wt-a--example.ddev.site"
	// bs is a single backslash, assembled so nothing in the pipeline reads it.
	bs = "\\"
)

func r46Fwd(t *testing.T) *origin.Matcher {
	t.Helper()
	m, err := origin.NewMatcher([]origin.Pair{{
		Canonical: origin.MustParse("https://" + r46Canon),
		Variant:   origin.MustParse("https://" + r46Var),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func r46Rev(t *testing.T) *origin.Matcher {
	t.Helper()
	m, err := origin.NewMatcher([]origin.Pair{{
		Canonical: origin.MustParse("https://" + r46Var),
		Variant:   origin.MustParse("https://" + r46Canon),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func r46Ratio(t *testing.T, m *origin.Matcher, b []byte) float64 {
	t.Helper()
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	HostLeaksBack(m, b)
	runtime.ReadMemStats(&after)
	return float64(after.TotalAlloc-before.TotalAlloc) / float64(len(b))
}

// TestAllocationStaysBounded's first case is named "every view fires", and after
// 598de7c it no longer does.
//
// Its unit is `&#92;3a %5C\3a [http:` — a reference, a percent escape, a CSS
// escape and a token start. 598de7c added two more views to rewriteAll, and both
// are gated on needles that unit does not contain: `\u` for stripForJSONEsc, and
// `%5Cu` for composeView(stripForPercent, stripForJSONEsc). So the case that
// exists to bound the composite measures four views out of six, and passes at
// 186x against its 200x ceiling with the two new ones never entered.
//
// The commit message treats 304x as the cost it declined to pay:
//
//	"Gated on a two-byte needle — `\u`, and `%5Cu` for the composed view —
//	 because gating on the backslash alone took the allocation composite from
//	 200x the body to 304x."
//
// Gating narrowed *when* that cost is paid; it did not lower it. Add the two
// needles to the same unit and the composite is 297x — the number the gate was
// chosen to avoid — on a body no larger than the one already measured. The
// ceiling is not a formality (its own comment: "the number that matters is the
// peak heap for one request at DefaultMaxBody, and concurrent bodies multiply
// it"), so a case that stopped exercising the thing it is named for is a
// guardrail that stopped guarding.
//
// The logged second half is what makes this ordinary rather than adversarial:
// the needles are two bytes, and one occurrence anywhere in the buffer arms a
// whole-buffer view. `ä` is in the inline JSON of every Finnish page and
// `%5Cu` in every classic-editor save, so 8 MiB of otherwise plain ASCII with
// one of each costs 203x — already over the ceiling — against 17x with neither.
func TestR46EveryViewFiresNoLongerFiresEveryView(t *testing.T) {
	// The property, not the number. `TestAllocationStaysBounded`'s composite case
	// is only a composite while its unit reaches every view, and two of the six
	// are behind two-byte needles — so the unit silently stopped measuring them
	// and passed at 186x against a 200x ceiling with a third of the machinery
	// never entered. The ceiling is now 400x, which is what the composite
	// actually costs; this asserts the fixture still earns it.
	// `%5C3a` is round 56's cell: percent over a CSS escape, which is what
	// `post.php` posts back after cssEscapeLeak emits one. Its composition is
	// three views deep, so a fixture that does not arm it makes the ceiling
	// below fictitious — which is the whole failure this test was written for,
	// one round on.
	for _, needle := range []string{`\u`, `%5Cu`, `%5C3a`} {
		if !strings.Contains(r46CompositeUnit, needle) {
			t.Errorf("the composite allocation unit no longer contains %q, so the view "+
				"gated on it is not measured:\n  %s", needle, r46CompositeUnit)
		}
	}
}

// The reference axis, which the JSON view was not composed onto.
//
// stripForCSS is reachable through a character-reference decode — stripForRefsCSS
// and refsThenCSS exist because "any sanitiser or editor that entity-encodes a
// backslash in an inline style produces it". stripForJSONEsc has no such
// composition, so the same backslash spelled `&#92;` hides the escape from it on
// exactly the surfaces where a reference *is* decoded: the XML family, XHTML's
// script and style, and every request body.
//
// Reported as the narrow thing it is. Unlike `-` itself this has no named
// producer, so it is a hole in the family rather than a demonstrated leak — but
// "a spelling is a family, and 'does anything emit this?' has to be asked of the
// family" is the lesson the commit message itself draws, and the reference axis
// is the one member of the family it did not ask about.
func TestR46TheJSONViewIsNotComposedWithTheReferenceView(t *testing.T) {
	e := func(h string) string { return bs + "u" + h }
	ref := func(s string) string { return strings.ReplaceAll(s, bs, "&#92;") }

	fwd := "https://www" + e("002e") + "example.fi/x"
	rev := "https://wt-a" + e("002d") + e("002d") + "example.ddev.site/x"

	// Control: with a literal backslash both directions read it.
	if got := string(HostLeaksXML(r46Fwd(t), []byte(fwd), true)); got == fwd {
		t.Fatalf("control: the literal spelling is not read either: %s", got)
	}
	if got := string(HostLeaksBack(r46Rev(t), []byte(rev))); got == rev {
		t.Fatalf("control: the literal spelling is not read either: %s", got)
	}

	// The reference spelling of the same backslash, on a surface that decodes
	// references.
	if in := ref(fwd); string(HostLeaksXML(r46Fwd(t), []byte(in), true)) == in {
		t.Errorf("forward: a canonical origin goes out byte-identical: %s", in)
	}
	if in := ref(rev); string(HostLeaksBack(r46Rev(t), []byte(in))) == in {
		t.Errorf("reverse: the variant hostname goes upstream byte-identical: %s", in)
	}
}

// -----------------------------------------------------------------------------
// What held. Everything below passes; it is here because the shipped guardrails
// cannot reach the code 598de7c added.
// -----------------------------------------------------------------------------

// FuzzRewriteInvariants and FuzzHostLeaksInvariants carry no seed containing
// `\u` or `%5Cu`, so neither view added in 598de7c is reachable from their
// corpora. Same properties — identity is byte-identity (test 24), a real map is
// a fixed point (test 7), nothing panics — seeded for the new views.
//
// 6.6M executions, no failure.
func FuzzR46EscapeViews(f *testing.F) {
	for _, s := range []string{
		`{"u":"https://wt-a--example.ddev.site/x"}`,
		`{"u":"https://www.example.fi/x"}`,
		`<!-- wp:cover {"url":"https://www.example.fi/bg.jpg"} -->`,
		`content=%22https%3A%2F%2Fwww%5Cu002eexample.fi%2Fx%22`,
		`https://www.example.fi/x`,
		`a\\u002d\\u002db and https://www.example.fi/x`,
		`--`,
		`%5Cu002d%5Cu002d`,
		` https://www.example.fi/x`,
		`<div style="background:url(https\3a \2f \2f www.example.fi/x)">-</div>`,
	} {
		f.Add([]byte(s))
	}
	ident, err := origin.NewMatcher([]origin.Pair{{
		Canonical: origin.MustParse("https://" + r46Canon),
		Variant:   origin.MustParse("https://" + r46Canon),
	}})
	if err != nil {
		f.Fatal(err)
	}
	fwd, err := origin.NewMatcher([]origin.Pair{{
		Canonical: origin.MustParse("https://" + r46Canon),
		Variant:   origin.MustParse("https://" + r46Var),
	}})
	if err != nil {
		f.Fatal(err)
	}
	rev, err := origin.NewMatcher([]origin.Pair{{
		Canonical: origin.MustParse("https://" + r46Var),
		Variant:   origin.MustParse("https://" + r46Canon),
	}})
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		for name, fn := range map[string]func(*origin.Matcher, []byte) []byte{
			"HostLeaks":     func(m *origin.Matcher, b []byte) []byte { return HostLeaks(m, b, true) },
			"HostLeaksXML":  func(m *origin.Matcher, b []byte) []byte { return HostLeaksXML(m, b, true) },
			"HostLeaksBack": HostLeaksBack,
			"HTML": func(m *origin.Matcher, b []byte) []byte {
				return runHTML(t, b, m, Options{})
			},
		} {
			if out := fn(ident, in); !bytes.Equal(in, out) {
				t.Fatalf("%s: identity map changed the bytes:\n in  %q\n out %q", name, in, out)
			}
			for _, m := range []*origin.Matcher{fwd, rev} {
				once := fn(m, in)
				if twice := fn(m, once); !bytes.Equal(once, twice) {
					t.Fatalf("%s: not a fixed point:\n in    %q\n once  %q\n twice %q",
						name, in, once, twice)
				}
			}
		}
	})
}

// 598de7c widened occupiesItsField's *leading* scan to skip `+` and `%XX`
// whitespace. The trailing scan's identical widening is gated on `open ==
// ownField`, under a comment explaining that skipping percent-newlines inside a
// `%22`-quoted field walks the scan onto the closing quote and accepts a
// truncation residue — §4.3's custom_css loss. The new leading skip carries no
// such gate, so the question is whether it accepts a buffer it should not.
//
// The property that answers it: a *decline* hands the whole buffer to rw and
// re-emits nothing, which this file documents as a known cost. An *accept*
// promises a length that describes the data, and BrokenSerialized is the oracle
// for that. 11.9M executions, no failure — the ungated leading skip converts
// declines into correct accepts and nothing else.
//
// Mutation-checked against `emitLen`, which is what an accept promises: with
// `return syn.dlen(repaired)` off by one it fails on the first seed. It does
// *not* fail for a widened leading skip (`c == '+' || c == '"'`, or dropping
// pctWhitespace), and that is the reason the skip is safe rather than an
// accident: the leading scan can only walk further back than it used to, which
// can only change `open`, and every residue that would make a length wrong is
// caught by the *trailing* scan — where `ownField`, the arm the widened leading
// scan lands on, is the strictest rule of the eight.
func FuzzR46RepairNeverBreaksMore(f *testing.F) {
	host := "https://" + r46Var + "/x"
	blob := `a:1:{s:3:"url";s:` + strconv.Itoa(len(host)) + `:"` + host + `";}`
	for _, pre := range []string{"", "opt=", "opt=+", "opt=%20", "opt=%0A", "opt=x+",
		"opt=x%20", "opt=%22%0A", "opt=%22+", `opt=%22`, "a=1&opt=", "<textarea>\n",
		`data-x="` + "\n", "opt=+%0D%0A+"} {
		for _, post := range []string{"", "&b=2", "%0A", "+", `%22`, "\n", `"`} {
			f.Add(pre+blob+post, true)
			f.Add(pre+blob+post, false)
		}
	}
	rev, err := origin.NewMatcher([]origin.Pair{{
		Canonical: origin.MustParse("https://" + r46Var),
		Variant:   origin.MustParse("https://" + r46Canon),
	}})
	if err != nil {
		f.Fatal(err)
	}
	rw := func(b []byte) []byte {
		nv, _ := rev.Rewrite(b, SurfaceRequestBody, false)
		return HostLeaksBack(rev, nv)
	}
	f.Fuzz(func(t *testing.T, in string, fields bool) {
		was := BrokenSerialized([]byte(in))
		var out []byte
		if fields {
			out = RepairSerializedFields([]byte(in), rw)
		} else {
			out = RepairSerialized([]byte(in), rw)
		}
		// A decline is rw over the whole buffer with nothing re-emitted; that
		// cost is stated in occupiesItsField's own comment. The question here is
		// the other one: where the gate now accepts, is the length right?
		if bytes.Equal(out, rw([]byte(in))) {
			return
		}
		if now := BrokenSerialized(out); now > was {
			t.Fatalf("an accepted repair broke %d -> %d serialized headers:\n in  %q\n out %q",
				was, now, in, out)
		}
	})
}

// The splice, over every shape the new view can be handed. All pass: an escape
// inside the host splices over its whole source range, a truncated or non-hex
// `\u` decodes to nothing and leaves the buffer alone, and `\\u002d` — an
// escaped backslash followed by a literal `u002d`, which JSON does *not* read as
// an escape — correctly does not match.
func TestR46JSONEscViewSplicesItsWholeSourceRange(t *testing.T) {
	hr := hostsFor(r46Rev(t))
	splice := func(s string) string {
		return string(hr.spliceHostsIn(stripForJSONEsc([]byte(s), ctlJoin), []byte(s), urlTokenStarts, true, SurfaceHTMLAttr, nil))
	}
	e := func(h string) string { return bs + "u" + h }
	canon := "https://" + r46Canon
	for _, c := range []struct{ in, want string }{
		{"https://wt-a" + e("002d") + e("002d") + "example.ddev.site/x", canon + "/x"},
		// The host's first and last byte spelled as escapes, so pos[hs] and
		// end[he-1] are what decide the replaced range.
		{"https://" + e("0077") + "t-a--example.ddev.site/x", canon + "/x"},
		{"https://wt-a--example.ddev.sit" + e("0065") + "/x", canon + "/x"},
		{"https://wt-a--example" + e("002e") + "ddev.site/x", canon + "/x"},
		{"https" + e("003a") + "//wt-a--example.ddev.site/x", "https" + e("003a") + "//" + r46Canon + "/x"},
		{"https://wt-a--example.ddev.site" + e("002f") + "x", canon + e("002f") + "x"},
		{"https://wt-a--example.ddev.site/x" + bs + "u00", canon + "/x" + bs + "u00"},
		{"https://wt-a--example.ddev.site/x?q=" + bs + "uzzzz", canon + "/x?q=" + bs + "uzzzz"},
		// `\\u002d` is a backslash then the four literal characters `u002d`.
		{"https://wt-a" + bs + bs + "u002d" + bs + bs + "u002dexample.ddev.site/x",
			"https://wt-a" + bs + bs + "u002d" + bs + bs + "u002dexample.ddev.site/x"},
		{"just some " + e("0041") + " text", "just some " + e("0041") + " text"},
	} {
		if got := splice(c.in); got != c.want {
			t.Errorf("%q\n got  %q\n want %q", c.in, got, c.want)
		}
	}
}

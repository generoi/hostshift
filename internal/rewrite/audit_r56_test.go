package rewrite

import (
	"io"
	"strings"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
)

// Round 56. The request direction, on its own terms.
//
// rewriteAll states the rule this file tests, in as many words: "every spelling
// the forward direction can *emit*, the reverse direction must be able to
// *read*". What it does not do is enumerate the spellings. There are two
// independent axes — an *inner* encoding, which is the escape alphabet of the
// consumer that will parse the value (none, CSS, JSON/JavaScript), and an
// *outer* one, which is what the transport did to it on the way back (none, the
// percent encoding a form applies, the character references markup applies) —
// and the code fills in the grid one hand-written cell at a time:
//
//	                inner: none   inner: css        inner: json-esc
//	outer: none     stripForURL   stripForCSS       escView
//	outer: refs     stripForRefs  refsThenCSS       stripForRefs+escView
//	outer: percent  stripForPct   ** MISSING **     stripForPct+escView
//
// and `+` is a further member of the outer axis that no view reads at all: in a
// urlencoded body and a query string a `+` *is* a space, and a space is what
// terminates a CSS hex escape, so `%5C3a+` is the same `\3a ` one spelling over.
//
// The two tests that were supposed to hold the rule are lists too:
// TestForwardEmissionsAreReadableInReverse names eleven shapes and
// TestEveryRequestBodyArmReadsEverySpelling six, and both spell their one
// percent case as `https%3A%5C%2F%5C%2F…` — percent over a *JSON* escape. The
// cell neither of them writes down is percent over a *CSS* escape, which is the
// only cell the code is missing. A list cannot pin a list; this one is
// generated from the two axes.
const (
	r56Canon = "www.example.fi"
	r56Var   = "wt-a--example.ddev.site"
)

func r56Fwd(t *testing.T) *origin.Matcher {
	t.Helper()
	m, err := origin.NewMatcher([]origin.Pair{{
		Canonical: origin.MustParse("https://" + r56Canon),
		Variant:   origin.MustParse("https://" + r56Var),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func r56Rev(t *testing.T) *origin.Matcher {
	t.Helper()
	m, err := origin.NewMatcher([]origin.Pair{{
		Canonical: origin.MustParse("https://" + r56Var),
		Variant:   origin.MustParse("https://" + r56Canon),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// The inner axis: the escape alphabet of the parser that will consume the value.
var r56Inner = []struct {
	name string
	fn   func(string) string
}{
	{"none", func(u string) string { return u }},
	// A CSS hex escape, terminated by the space css-syntax-3 4.3.7 consumes.
	{"css", strings.NewReplacer(":", `\3a `, "/", `\2f `).Replace},
	// The same with the six-digit padding the grammar also allows.
	{"css-6hex", strings.NewReplacer(":", `\00003a`, "/", `\00002f`).Replace},
	// wp_json_encode's slash, and Gutenberg's `--`.
	{"json-slash", func(u string) string { return strings.ReplaceAll(u, "/", `\/`) }},
	{"json-u", strings.NewReplacer("/", `/`, "-", `-`).Replace},
}

// The outer axis: what carried it back.
var r56Outer = []struct {
	name string
	fn   func(string) string
}{
	{"none", func(s string) string { return s }},
	// encodeURIComponent, which is what a script or a JSON field writes.
	{"percent", strings.NewReplacer(`\`, "%5C", " ", "%20").Replace},
	// A form encoder, where the space is a `+` — and `+` means space only
	// here, which is why it is its own row rather than the one above.
	{"percent-form", strings.NewReplacer(`\`, "%5C", " ", "+").Replace},
	{"refs-numeric", func(s string) string { return strings.ReplaceAll(s, `\`, "&#92;") }},
	{"refs-named", func(s string) string { return strings.ReplaceAll(s, `\`, "&bsol;") }},
}

// TestR56TheEncodingGridHasNoHoles crosses the two axes and asks the request
// direction to read every cell.
//
// Every cell is a value that resolves to the variant origin. The CSS rows were
// asked of a CSS tokenizer (css-syntax-3 §4.3.7: `\` then one to six hex digits,
// with one following whitespace consumed) and then of ada:
//
//	"https\3a \2f \2f wt-a--example.ddev.site\2f x"  -> "https://wt-a--example.ddev.site/x"
//	  new URL(that).host === "wt-a--example.ddev.site"
//
// and the percent row is that same value after PHP's own decode of a form
// field, which is byte-for-byte the `outer: none` row again:
//
//	decodeURIComponent("https%5C3a+%5C2f+%5C2f+wt-a--example.ddev.site%5C2f+x".replace(/\+/g," "))
//	  === "https\3a \2f \2f wt-a--example.ddev.site\2f x"
//
// So the value the shared database stores is a variant hostname, and production
// serves a `.ddev.site` URL to real visitors. §4.3, which says the database
// stays byte-identical to production, with no undo.
//
// The producer is not hypothetical and is named by rewriteAll's own comment:
// cssEscapeLeak splices the variant into a style attribute that already spells
// its URL with CSS escapes, so the page goes to the browser carrying exactly
// the `outer: none` row — and `post.php` posts that field back percent-encoded,
// which is the `outer: percent` row. That is the identical producer and
// transport the `%5Cu` gate three lines above was written for.
func TestR56TheEncodingGridHasNoHoles(t *testing.T) {
	rev := r56Rev(t)
	varURL := "https://" + r56Var + "/x"
	for _, in := range r56Inner {
		for _, out := range r56Outer {
			t.Run(in.name+"/"+out.name, func(t *testing.T) {
				spelled := out.fn(in.fn(varURL))
				// HostLeaksBack is what the proxy runs over a request body, a
				// request line and a request header.
				back := string(HostLeaksBack(rev, []byte(spelled)))
				if strings.Contains(back, r56Var) {
					t.Errorf("a variant hostname survives the request direction, so it "+
						"would be written into the shared database:\n  sent %s\n  back %s",
						spelled, back)
				}
			})
		}
	}
}

// And the same grid on the surfaces the proxy actually names, because the
// alphabet is a property of the surface and the request arms do not share one
// with HostLeaksBack's html-attr.
func TestR56TheEncodingGridHasNoHolesOnAnyRequestSurface(t *testing.T) {
	rev := r56Rev(t)
	varURL := "https://" + r56Var + "/x"
	for _, surface := range []string{SurfaceRequestBody, SurfaceRequestLine, SurfaceHeader} {
		for _, in := range r56Inner {
			for _, out := range r56Outer {
				t.Run(surface+"/"+in.name+"/"+out.name, func(t *testing.T) {
					spelled := out.fn(in.fn(varURL))
					st := NewStats(false)
					nv, _ := rev.Rewrite([]byte(spelled), surface, false)
					back := string(RepairSerialized(HostLeaksBackCounted(rev, nv, st, surface, 0),
						func(b []byte) []byte { return b }))
					if strings.Contains(back, r56Var) {
						t.Errorf("a variant hostname survives the %s arm:\n  sent %s\n  back %s",
							surface, spelled, back)
					}
				})
			}
		}
	}
}

// The forward half of the same cell, so the test cannot pass by the engine
// never emitting the shape it then fails to read.
func TestR56AStyleAttributeStillGoesOutCSSEscaped(t *testing.T) {
	fwd := r56Fwd(t)
	in := `<div style="background:url(https\3a \2f \2f ` + r56Canon + `\2f hero.jpg)">x</div>`
	r := NewResponseBody(io.NopCloser(strings.NewReader(in)), fwd, nil, Options{Stats: NewStats(false)})
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	want := `https\3a \2f \2f ` + r56Var + `\2f hero.jpg`
	if !strings.Contains(string(out), want) {
		t.Fatalf("the forward direction no longer emits a CSS-escaped variant, so the "+
			"grid above is testing a shape nothing produces:\n%s", out)
	}
}

// A `\u{…}` with more than six hex digits.
//
// JavaScript's CodePoint escape takes HexDigits with no bound on leading zeros —
// `"\u{0000002f}"` is `/` — and ada resolves
//
//	fetch("https://www.example.fi\u{0000002f}x")
//
// to the host www.example.fi, which is a live production origin in the
// developer's authenticated browser. Test 28.
//
// Round 55 bounded escTerminates' scan for the closing brace at six characters,
// on the reasoning that "a code point is at most 0x10FFFF, so six hex digits say
// everything", and made anything past the bound an escape it declines to read —
// which declines the whole match. The bound is right and its scope is not: six
// digits is the width of the *value*, not of the escape, because the grammar
// admits any number of leading zeros before them. 0f74c2d rewrites all three of
// these; 0c25e43 rewrites only the first.
func TestR56ALongUBraceIsAnEscapeItCanStillRead(t *testing.T) {
	m := r56Fwd(t)
	for _, u := range []string{
		`https://` + r56Canon + `\u{00002f}x`,       // 6 digits: read
		`https://` + r56Canon + `\u{0000002f}x`,     // 8: ada says the same URL
		`https://` + r56Canon + `\u{00000000002f}x`, // 12: and the same again
	} {
		in := `<script>fetch("` + u + `")</script>`
		r := NewResponseBody(io.NopCloser(strings.NewReader(in)), m, nil, Options{Stats: NewStats(false)})
		out, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(out), r56Var) {
			t.Errorf("a browser resolves this to the canonical and it was served live "+
				"to an authenticated developer:\n  %s", out)
		}
	}
}

// The scanner behind TestSurfaceNamesAreKnownHere has to see every surface this
// package declares, and it does not see the one declared on its own line.
//
// `^\s*(Surface\w+)\s*=\s*"…"` matches a member of a `const (…)` block and not
// `const SurfaceStraggler = "straggler"`, which is how sweep.go:13 declares it.
// Two consequences, and the second is the one that matters: SurfaceStraggler is
// listed in that test's `want` map and is never actually checked, and a *new*
// surface added the way sweep.go adds one falls through escapeAlphabetFor to
// escJS with nothing failing — which is the exact drift the test exists to make
// impossible. `len(out) < 10` does not catch it either: thirteen of the fourteen
// are still found.
func TestR56TheSurfaceScannerSeesEverySurface(t *testing.T) {
	found := surfaceConstants(t)
	for _, name := range []string{"SurfaceStraggler", "SurfaceHTMLAttr", "SurfaceRequestBody"} {
		if _, ok := found[name]; !ok {
			t.Errorf("%s is declared in this package and the scanner did not find it, "+
				"so nothing pins its alphabet and a new surface declared the same way "+
				"would fall to escJS in silence; found %d: %v", name, len(found), found)
		}
	}
}

// ---------------------------------------------------------------------------
// Fix-side coverage for round 56.
// ---------------------------------------------------------------------------

// The escaped-colon rule, in the direction round 55 did not assert.
//
// Mutation M12 — letting escColonLen run on an escPath surface — survived the
// whole suite, because TestR55AnEscapedColonCarriesAPort exercises the script
// half only. In an attribute a backslash folds to `/`, so there is no escape and
// no port: `<a href="https://www.example.fi\x3a8443/x">` is the canonical with
// the path `/x3a8443/x`, and reading a port there declines the match and leaves
// a live production origin in a plain link. Both spellings verified against ada.
func TestR56AnAttributeHasNoEscapedColon(t *testing.T) {
	m := r56Fwd(t)
	for _, url := range []string{
		`https://www.example.fi\x3a8443/x`,
		`https://www.example.fi\0728443/x`,
	} {
		in := `<a href="` + url + `">x</a>`
		out := rewriteHTML(t, m, in, NewStats(false))
		if !strings.Contains(out, r55Variant) {
			t.Errorf("a browser resolves this to %s and it was served live:\n  %s",
				r55Canonical, out)
		}
	}
}

// `\u{...}`'s bound is on the scan, not on the value.
//
// JavaScript's `CodePoint :: HexDigits` admits unlimited leading zeros, so
// `\u{0000002f}` is `/` and the host before it is this map's. Round 55 stopped
// the scan after six hex digits — six is every code point *value*, and an
// escape is a different length from the number it spells — and past the bound
// reported an escape it could not read, which declines. That turned a rewritten
// origin into a test-28 leak.
func TestR56TheBraceBoundIsOnTheScanNotTheValue(t *testing.T) {
	m := r56Fwd(t)
	for _, c := range []struct {
		url     string
		rewrite bool
	}{
		{`https://www.example.fi\u{2f}x`, true},
		{`https://www.example.fi\u{0000002f}x`, true},
		{`https://www.example.fi\u{00000000000000002f}x`, true},
		// Seven significant digits is past the last code point whatever it
		// spells, and an unterminated brace is not an escape we can read —
		// both decline rather than accepting the match.
		{`https://www.example.fi\u{10FFFFF}x`, false},
		{`https://www.example.fi\u{2f` + strings.Repeat("0", 200), false},
	} {
		in := `<script>fetch("` + c.url + `")</script>`
		out := rewriteHTML(t, m, in, NewStats(false))
		if got := strings.Contains(out, r55Variant); got != c.rewrite {
			t.Errorf("rewrote=%v, want %v:\n  in  %s\n  out %s", got, c.rewrite, in, out)
		}
	}
}

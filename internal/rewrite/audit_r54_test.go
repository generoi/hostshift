package rewrite

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
)

// Round 54. The boundary itself.
//
// Rounds 52 and 53 enumerated two surfaces and found them clean, and round 53's
// LARGE lived *between* them: a JSON escape read as a host boundary. Neither
// cross product could contain it, because audit_r52's corpus has no row with an
// escape immediately after a host and audit_r53 varied encodings but not what
// follows the origin. testdata/url-shapes.tsv.gz cannot contain it either — its
// tails are `/x`, `/x?q=1` and `""`, so every row ends the host with a path or
// with the end of the value.
//
// So this round enumerates what sits immediately before and after the origin:
// 102 boundary tokens (every JSON and JS string escape, every HTML character
// reference spelling of them, percent escapes, CSS escapes, whitespace and its
// encoded forms, quotes, brackets, punctuation, the composed pairs) x 5
// positions (after the host, between its labels, before the first label, before
// the host, inside the separator) x 4 tails x 5 surfaces (href attribute, inline
// script, JSON body, CSS url(), raw) — 9,644 cells, of which 7,472 are
// expressible on their surface, each asserting what a browser resolves rather
// than what the code emits. 110 cells leak and 388 emit a variant the browser
// reads as some other host.
//
// The expectations below are ada's, the parser Chrome ships and the one
// testdata/url-shapes.tsv.gz is generated from, asked as
//
//	new URL(decode_surface(src), "https://wt-a--example.ddev.site/dir/page").host
//
// where decode_surface is the surface's own parser: JSON.parse for a JSON body,
// a JS string literal for an inline <script>, character-reference decoding for
// an attribute, the CSS tokenizer for url(). Each host below was read off that.
//
// The contract is oracle_test.go's, unchanged:
//
//   - the browser resolves it to the canonical  =>  hostshift must rewrite it
//   - the browser resolves it anywhere else     =>  hostshift must not touch it
//
// The disagreements have one root cause, and the tests below are its faces: a
// backslash means three different things on the three surfaces this engine
// serves — a path separator in an HTML attribute or a bare URL value, the JSON
// escape alphabet in a JSON string, the larger JavaScript alphabet in an inline
// script — and nothing in the matcher is told which of the three it is holding.
// Matcher.rewrite has `surface` in scope at the hostTerminated call; delimAt is
// not given it.

const (
	r54Canonical = "www.example.fi"
	r54Variant   = "wt-a--example.ddev.site"
)

func r54Matcher(t *testing.T) *origin.Matcher {
	t.Helper()
	m, err := origin.NewMatcher([]origin.Pair{{
		Canonical: origin.MustParse("https://" + r54Canonical),
		Variant:   origin.MustParse("https://" + r54Variant),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// ---------------------------------------------------------------------------
// 1. An escape that spells a character the URL parser *removes*.
// ---------------------------------------------------------------------------

// The URL parser deletes every ASCII tab, LF and CR from its input before it
// parses, which is why `https://www.example<TAB>.fi` is the canonical origin and
// why TestObfuscatedOriginsAreRewritten asserts hostshift rewrites it. The three
// decoders in this package all know that: stripForURL removes the literal bytes,
// and stripForRefs removes their character-reference spellings, on the rule its
// own comment states — "a decoder must never *emit* a control character …
// Removing one is not emitting one."
//
// stripForJSONEsc kept the ban and never added the removal. `\t`, `\n` and `\r`
// are not decoded there at all (they are two-byte escapes, and the view only
// looks at `\/`, `\xNN`, `\u{…}` and `\uXXXX`), and the `\uXXXX` and `\xNN`
// spellings of the same three characters are decoded and then refused for
// landing outside printable ASCII — the ban on *emitting* a control, applied
// where a removal was due. So
// the six spellings below leave the host split in the view, no origin is found,
// and a live production origin goes to the browser inside an inline <script> —
// which PLAN §5.2 calls Tier 1.
//
// The proof that this is a defect rather than a choice is one function away:
// RewriteJSON's decodeJSONLeak unquotes the string with jsontext.AppendUnquote,
// so the *same bytes in a JSON response body* are rewritten. json_test.go states
// the invariant outright for JSON-LD — "inside a <script> tag the HTML raw-text
// scan handles it. Either way the answer is the same" — and it is not the same.
func TestR54EscapedControlInAHostLeaksInsideAScript(t *testing.T) {
	m := r54Matcher(t)
	// Each `url` is what the document's source spells; ada resolves every one of
	// them to www.example.fi once the surface's parser has decoded it.
	for _, url := range []string{
		`https://www.example\t.fi/p`,      // \t -> TAB, which the URL parser removes
		`https://www.example\n.fi/p`,      // \n -> LF
		`https://www.example\r.fi/p`,      // \r -> CR
		"https://www.example\\u0009.fi/p", // the \\uXXXX spelling of the same TAB
		"https://www.example\\u000a.fi/p", // and of the LF
		`https://www.example\x09.fi/p`,    // the JS spelling a minifier writes
		`https://www.\texample.fi/p`,      // between the host's labels
		`https://\twww.example.fi/p`,      // before the host
		`https:/\t/www.example.fi/p`,      // inside the separator
	} {
		for _, w := range []struct{ name, open, close string }{
			{"inline script", `<script>fetch("`, `")</script>`},
			{"ld+json script", `<script type="application/ld+json">{"url":"`, `"}</script>`},
		} {
			in := w.open + url + w.close
			out := rewriteHTML(t, m, in, NewStats(false))
			if !strings.Contains(out, r54Variant) {
				t.Errorf("[%s] a browser resolves this to %s and it was served unrewritten:\n  %s",
					w.name, r54Canonical, out)
			}
		}
	}
}

// The same string, through the JSON body path, which does rewrite it. Any fix
// has to close the gap, not widen it: this is the assertion that says which of
// the two behaviours is right.
func TestR54TheJSONBodyPathAlreadyRewritesWhatTheScriptPathLeaks(t *testing.T) {
	m := r54Matcher(t)
	for _, url := range []string{
		`https://www.example\t.fi/p`,
		"https://www.example\\u0009.fi/p",
		`https://www.example\n.fi/p`,
	} {
		body := `{"url":"` + url + `"}`
		if got := string(RewriteJSON([]byte(body), m, NewStats(false), quiet(), false)); !strings.Contains(got, r54Variant) {
			t.Fatalf("the JSON body path stopped rewriting %q: %s — the asymmetry this test pins is gone in the wrong direction", url, got)
		}
		page := `<script type="application/ld+json">` + body + `</script>`
		if got := rewriteHTML(t, m, page, NewStats(false)); !strings.Contains(got, r54Variant) {
			t.Errorf("byte-identical JSON: rewritten as a response body, served whole as an inline script:\n  %s", got)
		}
	}
}

// And the straggler sweep cannot see it either, so the leak is uncounted: §4.4's
// backstop runs the same byte matcher, which is the half of the engine that
// never decoded the escape. `check` and `diff` both sum these counters.
func TestR54TheEscapedControlLeakIsUncounted(t *testing.T) {
	m := r54Matcher(t)
	in := `<script>fetch("https://www.example\t.fi/p")</script>`
	st := NewStats(false)
	out := rewriteHTML(t, m, in, st)
	swept := string(SweepBytes([]byte(out), m, st, quiet()))
	if strings.Contains(swept, r54Variant) {
		return // rewritten somewhere; nothing to report
	}
	t.Errorf("a production origin survived both the structured pass and the straggler sweep, and the census is silent:\n  %s", swept)
}

// The same class, one position over, fails in the opposite direction.
//
// After the host a removable control does not end the host either: the URL
// parser deletes it and the host runs on into what follows. delimAt asks
// jsonEscChar for the byte and then asks isHostByte about it, and a tab is not a
// host byte — so `https://www.example.fi\tx` matches and is rewritten, while a
// browser resolves it to www.example.fix. That is round 53's harm chain again:
// the page never pointed at production, the variant goes out glued to a letter,
// and the reverse direction cannot read it back.
//
// Note that mutating jsonEscChar's `\n`, `\r` and `\t` arms to report a host
// byte instead leaves the whole suite green (mutants m04-m06), so nothing pins
// today's answer either way.
func TestR54ARemovedControlEscapeIsNotAHostBoundary(t *testing.T) {
	m := r54Matcher(t)
	for _, c := range []struct{ url, resolves string }{
		{`https://www.example.fi\tx`, "www.example.fix"},
		{`https://www.example.fi\nx`, "www.example.fix"},
		{`https://www.example.fi\rx`, "www.example.fix"},
		{"https://www.example.fi\\u0009x", "www.example.fix"},
		{"https://www.example.fi\\u000ax", "www.example.fix"},
		{`https://www.example.fi\x09x`, "www.example.fix"},
	} {
		for _, w := range []struct{ name, open, close string }{
			{"inline script", `<script>fetch("`, `")</script>`},
			{"json body", `{"u":"`, `"}`},
		} {
			in := w.open + c.url + w.close
			var out string
			if w.name == "json body" {
				out = string(RewriteJSON([]byte(in), m, NewStats(false), quiet(), false))
			} else {
				out = rewriteHTML(t, m, in, NewStats(false))
			}
			if out != in {
				t.Errorf("[%s] a browser resolves this to %s, so nothing may change:\n  in  %s\n  out %s",
					w.name, c.resolves, in, out)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// 2. The JS escapes delimAt does not decode.
// ---------------------------------------------------------------------------

// Round 53 taught delimAt that a backslash inside a JSON string introduces an
// escape whose decoded character continues the host, and jsonEscChar decodes the
// JSON alphabet: `\/ \\ \" \n \r \t \b \f \uXXXX`. A *JavaScript* string carries
// three more spellings of the same byte, and an inline <script> is a JavaScript
// string, not a JSON one: `\xNN`, `\u{…}` and legacy octal `\NNN`. An escape the
// switch does not recognise is not a boundary either — JS decodes `\A` to `A`.
//
// urlobf.go's jsEscAt already decodes all three, and its comment names the first
// one for exactly the reason round 53 widened the fourth: "`\xe4` in particular
// is what a minifier run with `ascii_only` writes for every byte above 0x7E —
// the same IDN authority the `ä` case was widened for, one member over."
// The view was widened; the byte matcher's boundary test was not, so the two
// halves of round 53's fix disagree about the same document.
//
// The harm is round 53's, unchanged: ada resolves `https://www.example.fi\xe4x`
// to www.example.xn--fix-rla, so nothing here ever pointed at production; the
// rewrite happens anyway; the browser decodes the escape and holds the *variant*
// hostname with a letter glued to it, which the reverse direction cannot read —
// see TestR54TheGluedVariantSurvivesTheRequestDirection.
func TestR54JavaScriptEscapesAreReadAsHostBoundaries(t *testing.T) {
	m := r54Matcher(t)
	for _, c := range []struct{ url, resolves string }{
		{`https://www.example.fi\xe4x`, "www.example.xn--fix-rla"},
		{`https://www.example.fi\xe4/p`, "www.example.xn--fi-wia"},
		{`https://www.example.fi\x41`, "www.example.fia"},
		{`https://www.example.fi\x2ex`, "www.example.fi.x"},
		{`https://www.example.fi\u{e4}x`, "www.example.xn--fix-rla"},
		{`https://www.example.fi\u{41}`, "www.example.fia"},
		{`https://www.example.fi\101`, "www.example.fia"},
		{`https://www.example.fi\056x`, "www.example.fi.x"},
		{`https://www.example.fi\A`, "www.example.fia"},
		{`https://www.example.fi\U0041`, "www.example.fiu0041"},
	} {
		in := `<script>fetch("` + c.url + `")</script>`
		out := rewriteHTML(t, m, in, NewStats(false))
		if out != in {
			t.Errorf("a browser resolves this to %s, which is not the canonical, so nothing may change:\n  in  %s\n  out %s",
				c.resolves, in, out)
		}
	}
	// The control: the raw character, which the engine already leaves alone.
	// Round 53 fixed that spelling; these are the same character, written the
	// three other ways a JavaScript string can write it.
	in := `<script>fetch("https://www.example.fi` + "ä" + `x")</script>`
	if out := rewriteHTML(t, m, in, NewStats(false)); out != in {
		t.Fatalf("the raw non-ASCII control case regressed: %s", out)
	}
}

// The half that puts the variant in the shared production database.
//
// The page is served with the variant host followed by an escape the browser
// decodes into a letter. What the browser then holds — and posts back on a save
// — is the variant hostname with that letter attached, which the reverse
// direction reads as one unknown host and leaves alone. §4.3's round trip is
// what stops a worktree-local hostname reaching production; this is the shape
// that walks through it.
func TestR54TheGluedVariantSurvivesTheRequestDirection(t *testing.T) {
	fwd := r54Matcher(t)
	rev, err := origin.NewMatcher([]origin.Pair{{
		Canonical: origin.MustParse("https://" + r54Variant),
		Variant:   origin.MustParse("https://" + r54Canonical),
	}})
	if err != nil {
		t.Fatal(err)
	}
	// `\xe4` in a JS string is U+00E4; the browser decodes it before it parses
	// the URL, so the value the page holds — and submits — is the decoded one.
	served := rewriteHTML(t, fwd,
		`<script>fetch("https://www.example.fi\xe4x")</script>`, NewStats(false))
	if !strings.Contains(served, r54Variant) {
		return // nothing was rewritten, so nothing can leak back
	}
	decoded := strings.ReplaceAll(served, `\xe4`, "ä")
	back := string(HostLeaksBack(rev, []byte(decoded)))
	if strings.Contains(back, r54Variant) {
		t.Errorf("the variant hostname survives the request direction and lands in the shared database:\n  served  %s\n  browser %s\n  back    %s",
			served, decoded, back)
	}
}

// ---------------------------------------------------------------------------
// 3. delimAt decodes a percent escape but not a character reference.
// ---------------------------------------------------------------------------

// delimAt's own comment explains why '%' cannot be a blanket delimiter: in the
// percent encoding it introduces a host byte, so `www.example.com%2Eattacker.test`
// is a different registrable domain and rewriting it was a bug. In an HTML
// attribute a character reference introduces a host byte for exactly the same
// reason and with exactly the same effect — the browser decodes `&#65;` before
// the URL parser sees anything — and delimAt reads the `&` as a terminator.
//
// The locator's views know this (stripForRefs exists for it, and §5.3's entity
// carve-out is named in json.go); the byte matcher's boundary test does not, so
// the two halves disagree once more.
func TestR54ACharacterReferenceIsNotAHostBoundary(t *testing.T) {
	m := r54Matcher(t)
	for _, c := range []struct{ url, resolves string }{
		{`https://www.example.fi&#65;/p`, "www.example.fia"},
		{`https://www.example.fi&#x41;/p`, "www.example.fia"},
		{`https://www.example.fi&#46;x`, "www.example.fi.x"},
		{`https://www.example.fi&period;x`, "www.example.fi.x"},
		{`https://www.example.fi&#46x`, "www.example.fi.x"},
	} {
		in := `<a href="` + c.url + `">x</a>`
		out := rewriteHTML(t, m, in, NewStats(false))
		if out != in {
			t.Errorf("a browser resolves this to %s, so nothing may change:\n  in  %s\n  out %s",
				c.resolves, in, out)
		}
	}
}

// ---------------------------------------------------------------------------
// 4. The round-53 rule applied where a backslash is a path separator.
// ---------------------------------------------------------------------------

// delimAt is handed a surface by Matcher.Rewrite and does not look at it. Round
// 53's arm is right inside a JSON or JavaScript string and wrong everywhere
// else: in an HTML attribute the backslash is not an escape introducer, it is
// what the WHATWG parser folds to '/', so `https://www.example.fi\u0041` is the
// canonical origin with the path `/u0041` — a live production origin, in a plain
// <a href>, with no obfuscation at all beyond the backslash the parser is
// specified to accept.
//
// Only `\u` + four hex digits triggers it: every other escape jsonEscChar knows
// decodes to a byte that is not a host byte, so the arm still answers
// "delimiter" and the match survives. That makes this narrow, not absent — and
// it is a production origin reaching the browser, uncounted, which is test 28.
func TestR54TheJSONEscapeRuleFiresWhereABackslashIsAPathSeparator(t *testing.T) {
	m := r54Matcher(t)
	for _, url := range []string{
		"https://www.example.fi\\u0041",
		"https://www.example.fi\\u002d/p",
		"https://www.example.fi\\u00e4/p",
		"https://www.example.fi\\u00a0x",
	} {
		in := `<a href="` + url + `">x</a>`
		out := rewriteHTML(t, m, in, NewStats(false))
		if !strings.Contains(out, r54Variant) {
			t.Errorf("a browser resolves this to %s — the backslash is a path separator here, not an escape — and it was served unrewritten:\n  %s",
				r54Canonical, out)
		}
	}
	// The control, on the surface where round 53's reading is the right one.
	// A fix must keep this passing: inside a JSON string the escape really does
	// continue the host, and ada resolves the decoded form somewhere that is not
	// the canonical.
	in := "{\"u\":\"https://www.example.fi\\u00a0x\"}"
	if got := string(RewriteJSON([]byte(in), m, NewStats(false), quiet(), false)); got != in {
		t.Fatalf("round 53's fix regressed on the surface it was written for: %s", got)
	}
}

// ---------------------------------------------------------------------------
// Fix-side coverage: the mutations round 54's fixes survived on arrival.
//
// The tests above are the breaker's, and they pin the *declining* half of every
// escape arm — `\xe4`, `\101`, `\u{41}` all decode to host bytes, so removing
// the arm that reads them still declines and the test still passes. What nothing
// reached was the other half: the same three spellings decoding to a *delimiter*,
// where dropping the arm turns a live origin into a leak. Verified against ada.
// ---------------------------------------------------------------------------

// An escape that decodes to a slash ends the host, so the origin before it is
// this map's and must be rewritten. Without the `\xNN`, octal and `\u{...}` arms
// escTerminates falls through to "an unrecognised escape is the character
// itself", reads `x`, `0` and `u` as more host, and declines — leaving a
// dereferenceable production origin in an inline script, which is test 28.
func TestR54AnEscapedSlashStillEndsTheHost(t *testing.T) {
	m := r54Matcher(t)
	for _, url := range []string{
		`https://www.example.fi\x2fp`,
		`https://www.example.fi\057p`,
		`https://www.example.fi\u{2f}p`,
	} {
		in := `<script>fetch("` + url + `")</script>`
		out := rewriteHTML(t, m, in, NewStats(false))
		if !strings.Contains(out, r54Variant) {
			t.Errorf("a browser resolves this to %s and it was left unrewritten:\n  %s",
				r54Canonical, out)
		}
	}
}

// And the mirror: on a surface where nothing decodes, the same bytes are not
// escapes and the host is a different, shorter one. Reading them anyway invents
// a match — the round-53 harm, arriving through the view instead of through the
// boundary test.
//
// `<a href>` is the surface, because that is where WHATWG folds the backslash to
// a `/`: ada resolves the first of these to www.e and the second to
// www.example, neither of which this map contains.
func TestR54NoDecoderRunsInAnAttribute(t *testing.T) {
	m := r54Matcher(t)
	for _, url := range []string{
		`https://www.e\x78ample.fi/x`, // ada: www.e
		`https://www.example\t.fi/x`,  // ada: www.example
		// The same, with a `\u` elsewhere in the value so hasJSONEsc arms the
		// escape view. Without it the view never runs and the row proves
		// nothing about whether the view would have decoded correctly — which
		// is how the ungated two-byte arm survived its first mutation.
		`https://www.example\t.fi/x?a=\u002d`, // ada: www.example
	} {
		in := `<a href="` + url + `">x</a>`
		if out := rewriteHTML(t, m, in, NewStats(false)); out != in {
			t.Errorf("a browser resolves this to a host this map does not contain, so nothing may change:\n  in  %s\n  out %s", in, out)
		}
	}
	// And the complement, which is the half that leaks: in an attribute `\t` is
	// two ordinary path characters, so `https://www.example.fi\tx` is the
	// canonical with the path `/tx` and must be rewritten. Round 55's mutation
	// M23 — dropping removedEscLen's escPath guard — survived the whole suite on
	// this row, because every case here asserted the *declining* direction.
	for _, url := range []string{
		`https://www.example.fi\tx`,
		`https://www.example.fi\u0009x`,
	} {
		in := `<a href="` + url + `">x</a>`
		if out := rewriteHTML(t, m, in, NewStats(false)); !strings.Contains(out, "wt-a--example.ddev.site") {
			t.Errorf("a browser resolves this to the canonical and it was served live:\n  %s", out)
		}
	}
	// The same two spellings in a script, where a decoder does run and both are
	// the canonical. Without this the test above passes by never decoding at all.
	for _, url := range []string{
		`https://www.e\x78ample.fi/x`,
		`https://www.example\t.fi/x`,
		`https://www.example\t.fi/x?a=\u002d`,
	} {
		in := `<script>fetch("` + url + `")</script>`
		if out := rewriteHTML(t, m, in, NewStats(false)); !strings.Contains(out, r54Variant) {
			t.Errorf("the script surface stopped decoding it:\n  %s", out)
		}
	}
}

// origin.escapeAlphabetFor spells its surfaces as string literals, because
// package rewrite imports package origin and not the other way round. That makes
// a rename here a silent fall-through to the default there — a script surface
// renamed would keep working, an attribute surface renamed would quietly start
// reading escapes and deleting bytes.
//
// The names are read out of the source rather than repeated, because the first
// version of this test repeated them and that is exactly how it failed: it
// listed twelve of the fourteen surface names this package defines, and two of
// the two it omitted were surfaces round 54 had put on the wrong side. A
// hand-written list cannot pin a hand-written list. Adding a Surface constant
// now fails here until someone says which alphabet it takes.
func TestSurfaceNamesAreKnownHere(t *testing.T) {
	// false = a backslash is a path separator: the buffer is an attribute value,
	// a header, prose, or markup, and no string decoder will run over it.
	want := map[string]bool{
		SurfaceHTMLAttr:       false,
		SurfaceText:           false,
		SurfaceXMLText:        false,
		SurfaceComment:        false,
		SurfaceHeader:         false,
		SurfaceResponseHeader: false,
		SurfaceRequestLine:    false,
		SurfaceRequestBody:    false,
		SurfaceInlineStyle:    false,
		SurfaceHTMLEntity:     false,
		SurfaceHTMLObfuscated: false,
		SurfaceRawText:        false,
		SurfaceInlineScript:   true,
		SurfaceStraggler:      true,
		SurfaceJSONString:     true,
		SurfaceJSONEscape:     false,
	}
	// The second axis: whether a CSS tokenizer runs before the URLs are read.
	// surfaceDecodesCSS defaults to true, and its comment claimed this test
	// guarded it a round before the guard existed — so `text` inherited the
	// default in silence and a text/plain body was read as a stylesheet.
	wantCSS := map[string]bool{
		SurfaceHTMLAttr:       true, // a style="" attribute
		SurfaceText:           false,
		SurfaceXMLText:        true, // an SVG's <style>
		SurfaceComment:        true,
		SurfaceHeader:         true, // the request side, which reads what we emit
		SurfaceResponseHeader: false,
		SurfaceRequestLine:    true,
		SurfaceRequestBody:    true,
		SurfaceInlineStyle:    true,
		SurfaceHTMLEntity:     true,
		SurfaceHTMLObfuscated: true,
		SurfaceRawText:        true,
		SurfaceInlineScript:   true,
		SurfaceStraggler:      true,
		SurfaceJSONString:     true,
		SurfaceJSONEscape:     true,
	}
	// The third axis: whether something will hand this buffer to a URL parser,
	// which decides whether a tab, LF or CR after a host joins what follows or
	// ends it. Rounds 70 and 71 each got this wrong on a surface nobody had
	// classified — round 70 by asking value-vs-prose instead, round 71 by finding
	// that JSON, the straggler sweep and an inline `<script>` each need a
	// different answer from the one that gave.
	wantJoins := map[string]bool{
		SurfaceHTMLAttr:       true, // an href the URL parser reads
		SurfaceText:           false,
		SurfaceXMLText:        false,
		SurfaceComment:        false,
		SurfaceHeader:         true, // a Location is a URL
		SurfaceResponseHeader: true,
		SurfaceRequestLine:    true,
		SurfaceRequestBody:    false,
		SurfaceInlineStyle:    true, // url(…)
		SurfaceHTMLEntity:     true,
		SurfaceHTMLObfuscated: true,
		SurfaceRawText:        false,
		SurfaceInlineScript:   true,  // string and template literals
		SurfaceStraggler:      true,  // no surface, and it must not override a pass
		SurfaceJSONString:     false, // decided per string; see joinsControlsIn
		SurfaceJSONEscape:     true,
	}

	found := surfaceConstants(t)
	// Every name in the table must have been seen, or the scanner is narrower
	// than the thing it guards and the check passes by looking at less. Round 55
	// wrote the pattern for a `const (…)` block member and missed
	// `const SurfaceStraggler = "straggler"` on its own line — so the one escJS
	// surface named below was never actually checked, and a new surface declared
	// that way would fall through escapeAlphabetFor to escJS with nothing
	// failing. That is the exact hole this test exists to close.
	seen := map[string]bool{}
	for _, value := range found {
		seen[value] = true
	}
	for value := range want {
		if !seen[value] {
			t.Errorf("%q is classified here but the scanner did not find it in "+
				"the package source — the scanner is narrower than the table, so "+
				"this test is passing by looking at less than it claims", value)
		}
	}
	for name, value := range found {
		joins, classifiedJoins := wantJoins[value]
		if !classifiedJoins {
			t.Errorf("%s = %q is not classified for the control axis: decide "+
				"whether anything will parse a buffer on that surface as a URL, "+
				"say so in origin.surfaceJoinsControls, and list it here", name, value)
		} else if got := origin.SurfaceJoinsControls(value); got != joins {
			t.Errorf("%s (%q): SurfaceJoinsControls = %v, want %v", name, value, got, joins)
		}
		css, classified := wantCSS[value]
		if !classified {
			t.Errorf("%s = %q is not classified for the CSS axis: decide whether a "+
				"tokenizer runs over that surface before its URLs are read, and "+
				"say so in rewrite.surfaceDecodesCSS and here", name, value)
		} else if got := surfaceDecodesCSS(value); got != css {
			t.Errorf("%s (%q): surfaceDecodesCSS = %v, want %v", name, value, got, css)
		}
		esc, listed := want[value]
		if !listed {
			t.Errorf("%s = %q is not classified: decide whether a buffer on that "+
				"surface still carries string escapes, add it to origin."+
				"escapeAlphabetFor if it does not, and list it here", name, value)
			continue
		}
		if got := origin.SurfaceDecodesEscapes(value); got != esc {
			t.Errorf("%s (%q): SurfaceDecodesEscapes = %v, want %v — either the "+
				"name changed or the alphabet did, and both need deciding here",
				name, value, got, esc)
		}
	}
}

// surfaceConstants reads every `SurfaceX = "y"` this package declares.
func surfaceConstants(t *testing.T) map[string]string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	// `const X = "y"` as well as a `const (…)` block member: sweep.go declares
	// SurfaceStraggler the first way and the block-only pattern never saw it.
	re := regexp.MustCompile(`(?m)^\s*(?:const\s+)?(Surface\w+)\s*=\s*"([^"]+)"`)
	out := map[string]string{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			out[m[1]] = m[2]
		}
	}
	if len(out) < 10 {
		t.Fatalf("found only %d surface constants, so the scan is broken and "+
			"this test would pass by finding nothing: %v", len(out), out)
	}
	return out
}

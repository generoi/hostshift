package rewrite

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
)

// Round 51, on 2c6f6a8.

func r51Map(t *testing.T, canon, variant string) *origin.Map {
	t.Helper()
	m, err := origin.NewMap([]origin.Site{{
		Name:      "main",
		Canonical: origin.MustParse(canon),
		Variant:   origin.MustParse(variant),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func r51Body(t *testing.T, m *origin.Matcher, xml bool, in string) string {
	t.Helper()
	out, err := io.ReadAll(NewResponseBody(bytes.NewReader([]byte(in)), m, nil, Options{XMLEntities: xml}))
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// TestR51TheSchemeArmInventsPercentEncodingItDidNotFind.
//
// 2c6f6a8 taught `locateHostIn`'s `needScheme` arm to write the separator back
// in the encoding it was found in — the round-50 LARGE. `schemeSepAt` infers
// that encoding from the *source width* of the scheme's own colon:
//
//	if width(i) >= 3 { return "%3A%2F%2F" }
//
// Three source bytes for one `:` does mean `%3A`. It also means `\3a`, the CSS
// escape this file has a whole view for; five means `&#58;`, the character
// reference the XML views exist for; six means `\u003a`. Every one of those is
// answered `%3A%2F%2F`, and in none of those contexts is `%3A` a colon.
//
// So the arm now does, to the two encodings it does not know, exactly what
// round 50 stopped it doing to the one it does. `\3a` is decoded by the CSS
// tokenizer before a URL parser ever sees the value, and `%3A` is not — so
//
//	background:url(http\3a\2f\2fwww.example.fi/l.png)
//
// comes out as
//
//	background:url(https%3A%2F%2Fwt-a--example.ddev.site/l.png)
//
// which is not an absolute URL at all. The browser resolves it against the
// document: a relative path whose single segment is the whole origin, a 404 in
// the preview where production served an image. Before 2c6f6a8 this same input
// came out `url(https://wt-a--example.ddev.site/l.png)` and worked, so the fix
// for one spelling is a regression on two others.
//
// It is the same class of harm the round-50 finding was ranked LARGE for, and
// it reaches the shared database by the same route: `HostLeaksBack` runs this
// arm over the request path and every request body, and its own comment names
// "CSS-escaped spellings inside a page, and the editor or form that posts that"
// as the reason it exists. A block re-saved through the editor writes the
// unresolvable spelling upstream, where production serves it to real visitors.
//
// The fix is to read the separator's *bytes*, not its width — the source is
// `v` and `n.pos/n.end` already point into it — and to fall back to a raw
// `://` for any spelling that is neither `%3A` nor `:\/`. Raw is correct in a
// CSS value, in an attribute and in XML text; it is what this arm emitted for
// every one of these shapes before 2c6f6a8. No new view, so
// TestAllocationStaysBounded stays at 382x/172x/118x/104x.
func TestR51TheSchemeArmInventsPercentEncodingItDidNotFind(t *testing.T) {
	m := r51Map(t, "https://www.example.fi", "https://wt-a--example.ddev.site")
	const variant = "https://wt-a--example.ddev.site/l.png"

	// A CSS escape, through the whole response pipeline, in the surface a
	// theme actually writes it on. `http:` against an `https` variant is what
	// arms `needScheme`, and a database full of legacy `http://` URLs is the
	// ordinary case, not a contrived one.
	css := `<div style="background:url(http\3a\2f\2fwww.example.fi/l.png)">x</div>`
	out := r51Body(t, m.Forward(), false, css)
	if !strings.Contains(out, "wt-a--example.ddev.site") {
		t.Fatalf("not rewritten at all, so this fixture no longer tests the arm:\n  %s", out)
	}
	if strings.ContainsRune(css, '%') {
		t.Fatal("the fixture must contain no percent sign for the assertion below to mean anything")
	}
	if strings.Contains(out, "%3A%2F%2F") {
		t.Errorf("percent-encoding was invented for a value that had none:\n  in:  %s\n  out: %s", css, out)
	}
	// The real question, asked of the decoder that actually runs on this
	// surface: after the CSS tokenizer unescapes it, is it still an absolute
	// URL at the variant origin?
	if dec := string(stripForCSS([]byte(out)).b); !strings.Contains(dec, variant) {
		t.Errorf("CSS-decoded, the rewritten value is not %s — it is a relative path:\n"+
			"  in:  %s\n  out: %s\n  css-decoded: %s", variant, css, out, dec)
	}

	// The same defect one encoding over: a character reference, on the XML
	// surfaces `HostLeaksXML` and `stripForRefs` exist for.
	ref := `http&#58;&#47;&#47;www.example.fi/l.png`
	rout := string(HostLeaksXML(m.Forward(), []byte(ref), true))
	if !strings.Contains(rout, "wt-a--example.ddev.site") {
		t.Fatalf("not rewritten at all, so this fixture no longer tests the arm:\n  %s", rout)
	}
	if strings.Contains(rout, "%3A%2F%2F") {
		t.Errorf("percent-encoding was invented for a reference-encoded value:\n  in:  %s\n  out: %s", ref, rout)
	}
	if dec := string(stripForRefs([]byte(rout)).b); !strings.Contains(dec, variant) {
		t.Errorf("reference-decoded, the rewritten value is not %s:\n"+
			"  in:  %s\n  out: %s\n  ref-decoded: %s", variant, ref, rout, dec)
	}

	// And the round-50 case must keep working, so a fix cannot be "go back".
	pct := `/go/http%3A%2F%2Fwww.example.fi%2Fbar`
	pout := string(HostLeaksBack(m.Reverse(), []byte(pct)))
	_ = pout
	fwd := string(HostLeaks(m.Forward(), []byte(pct), true))
	if !strings.Contains(fwd, "%3A%2F%2F") {
		t.Errorf("the percent-encoded separator must still be written back percent-encoded:\n"+
			"  in:  %s\n  out: %s", pct, fwd)
	}
}

// TestR51TheJSONSeparatorArmHasATest: M3/M4 in round 51's survey — the JSON
// spelling of the separator had no test at all, because the percent fixture
// reaches the percent arm first. So `:\/\/` could have been emitted as `://`,
// or the arm's slash-width check made nonsense, and nothing would have failed.
func TestR51TheJSONSeparatorArmHasATest(t *testing.T) {
	m := r51Matcher(t, "https://www.example.fi", "https://wt-a--example.ddev.site")
	// A JSON-escaped URL whose scheme differs from the variant's, so the
	// needScheme arm is the one that fires.
	in := []byte(`{"u":"http:\/\/www.example.fi\/x"}`)
	out := string(HostLeaks(m.Forward(), in, true))

	if !strings.Contains(out, `https:\/\/wt-a--example.ddev.site`) {
		t.Errorf("the JSON separator was not written back in its own spelling:\n  %s", out)
	}
	if strings.Contains(out, "%3A%2F%2F") {
		t.Errorf("percent-encoding was invented for a JSON-escaped URL:\n  %s", out)
	}
}

// TestR51TheOctalDecoderKeepsItsRange: M5/M7 in round 51's survey — the octal
// arm's printable-range bound and its upper digit were both unasserted.
//
// Each case is chosen so the mutation *changes the outcome*. `\56` is a dot
// whose first digit is above 3, so narrowing the digit range stops it matching
// at all.
//
// The `\11` case used to assert the opposite of what it asserts now, on the
// reasoning that decoding a control "would splice `www.e` and `xample.fi` into
// a host that matches". It does, and that is the right answer: `\11` is octal
// for a tab, the URL parser *deletes* tab, and ada resolves
// `https://www.e<TAB>xample.fi/x` to www.example.fi. So the buffer-unchanged
// expectation was pinning a live production origin in place — the same defect
// round 54 corrected in audit_r45, one spelling over, and it survived because
// `\t` and a literal tab were both handled while the octal spelling was not.
//
// The rule it invoked survives untouched: the view must never *emit* a control
// byte. Removing one is not emitting it, which is what stripForURL has said
// about the literal spellings all along.
func TestR51TheOctalDecoderKeepsItsRange(t *testing.T) {
	m := r51Matcher(t, "https://www.example.fi", "https://wt-a--example.ddev.site")
	// Through the HTML path, because the octal arm is a *JavaScript* spelling and
	// round 54 made the view that decodes it surface-aware: a bare buffer is
	// prose, where a backslash is a path separator and `\56` is three characters.
	// The fixture was always an inline <script>; only the entry point was wrong.
	rw := func(in string) string {
		return rewriteHTML(t, m.Forward(), in, NewStats(false))
	}

	// `\56` is `.`, first digit 5. The view is armed because `\u` is present.
	dots := `<script>var a="\u002d";fetch("https://www\56example\56fi/x")</script>`
	if out := rw(dots); out == dots {
		t.Errorf("an octal escape above digit 3 was not read:\n  %s", out)
	}

	// `\11` is a tab. A browser deletes it before reading the host, so this is
	// www.example.fi and must be rewritten — and the view must arrive there by
	// *removing* the escape, never by emitting a 0x09 into its own bytes.
	tab := `<script>var a="\u002d";fetch("https://www.e\11xample.fi/x")</script>`
	if out := rw(tab); !strings.Contains(out, "wt-a--example.ddev.site") {
		t.Errorf("a browser resolves this to www.example.fi and it was served "+
			"live:\n  %s", out)
	}
	for _, c := range stripForJSONEscCtl([]byte(tab)).b {
		if c < 0x20 || c == 0x7F {
			t.Errorf("the view emitted control byte %#02x — removing an escape is "+
				"not the same as decoding one into the view", c)
			break
		}
	}

	// `\8` is not octal: JS reads it as a literal `8`.
	non := `<script>var a="\u002d";fetch("https://www.e\8ample.fi/x")</script>`
	if out := rw(non); out != non {
		t.Errorf("a non-octal digit was treated as an octal escape:\n  %s", out)
	}
}

func r51Matcher(t *testing.T, canonical, variant string) *origin.Map {
	t.Helper()
	m, err := origin.NewMap([]origin.Site{{
		Name:      "s",
		Canonical: origin.MustParse(canonical),
		Variant:   origin.MustParse(variant),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

package rewrite

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
)

// Round 48, on 717ad9d.

func r48Map(t *testing.T, canon, variant string) *origin.Map {
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

func r48HTML(t *testing.T, m *origin.Matcher, in string) string {
	t.Helper()
	out, err := io.ReadAll(NewResponseBody(bytes.NewReader([]byte(in)), m, nil, Options{}))
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// TestR48AnIDNCanonicalComesBackAsPunycode is §5.5's last bullet, which the
// engine does not implement:
//
//	**IDN / punycode** — real for `.fi` client domains; compare on normalised
//	punycode, preserve the original form on output
//
// Nothing preserves it. origin.Parse folds the host to punycode at parse time
// and no other spelling is kept anywhere, so every replacement the engine
// splices is `Origin.HostPort()` — the ACE form. In the *forward* direction that
// is invisible, because the thing being written is the variant and a variant is
// ASCII by construction. In the reverse direction the thing being written is the
// canonical, and for an IDN client it is written in a spelling production never
// used.
//
// The reverse direction is the request path: `HostLeaksBackCounted` on the
// request line, the headers, and every request body, plus the multipart arm. So
// the sequence is ordinary use of the block editor, and it is the same channel
// §4.3's whole argument is about:
//
//  1. production's `post_content` holds `https://www.hämeenlinna.fi/x`, in UTF-8,
//     because that is what the editor's link UI inserts for an IDN site;
//  2. the developer opens the post on the variant host — the forward pass turns
//     it into the variant, correctly;
//  3. the developer saves — the reverse pass turns it back into
//     `https://www.xn--hmeenlinna-q5a.fi/x`;
//  4. those are the bytes written to the *shared production database*, which
//     §4.3 says stays byte-identical to production, shared by canonical, every
//     worktree and CI.
//
// The link still resolves, so nothing breaks visibly and nothing warns. What is
// lost is the invariant: a row the developer did not mean to change has changed,
// permanently, in the database production serves, and a later
// `wp search-replace 'www.hämeenlinna.fi'` will not find it.
func TestR48AnIDNCanonicalComesBackAsPunycode(t *testing.T) {
	m := r48Map(t, "https://www.hämeenlinna.fi", "https://wt-a--hml.ddev.site")

	// The control: with an ASCII canonical the same round trip is byte-identical,
	// so it is the IDN spelling and not the round trip that is at fault.
	ascii := r48Map(t, "https://www.acme.fi", "https://wt-a--acme.ddev.site")
	ctl := `<a href="https://www.acme.fi/x">t</a>`
	if got := r48HTML(t, ascii.Reverse(), r48HTML(t, ascii.Forward(), ctl)); got != ctl {
		t.Fatalf("the control case no longer holds, so this test measures nothing:\n  %s", got)
	}

	for _, in := range []string{
		`<a href="https://www.hämeenlinna.fi/x">t</a>`,
		`{"url":"https://www.hämeenlinna.fi/x"}`,
		`https://www.hämeenlinna.fi/x`,
	} {
		out := r48HTML(t, m.Forward(), in)
		if strings.Contains(out, "meenlinna") {
			t.Fatalf("the forward pass did not rewrite it, so this measures nothing:\n  %s", out)
		}
		back := string(HostLeaksBack(m.Reverse(), []byte(out)))
		if back != in {
			t.Errorf("the request direction rewrote the canonical into a spelling "+
				"production never used, and this is what lands in the shared database:\n"+
				"  in   %s\n  back %s", in, back)
		}
	}
}

// TestR48TheEngineCannotReadJSsOtherStringEscapes: `jsonEscRune` is the view for
// "the escape a producer writes into a JS string", and it reads exactly one of
// the four escapes ECMAScript defines. An inline `<script>` is a Tier 1 surface
// — §5.2 calls it "where the CSS and JS URLs actually are" — and the JS parser
// decodes all four before the fetch:
//
//	"\x2f"       hex escape, two digits
//	"\u{2f}"     code-point escape, ES6
//	"\57"        legacy octal, still accepted in sloppy mode
//	"a\<LF>b"    a line continuation, which is *removed*
//
// Each of the strings below is `https://www.example.fi/x` to a browser, and each
// goes out untouched with the census reporting a clean page. `\xe4` is the one
// with a named producer: a minifier run with `ascii_only` writes every byte
// above 0x7E that way, and it is the same IDN authority 717ad9d widened
// `jsonEscRune` to read in its `ä` spelling — the family, one member over.
func TestR48TheEngineCannotReadJSsOtherStringEscapes(t *testing.T) {
	m, err := origin.NewMatcher([]origin.Pair{{
		Canonical: origin.MustParse("https://www.example.fi"),
		Variant:   origin.MustParse("https://wt-a--example.ddev.site"),
	}})
	if err != nil {
		t.Fatal(err)
	}

	// The `\u` spelling of the same host on the same surface is read, so neither
	// the surface nor the host is what stops the four below.
	ctl := `<script>fetch("https://www.e` + bs + `u0078ample.fi/x")</script>`
	if out := r48HTML(t, m, ctl); out == ctl {
		t.Fatalf("the control case no longer holds, so this test measures nothing:\n  %s", out)
	}

	for name, body := range map[string]string{
		"hex":               `fetch("https://www.e` + bs + `x78ample.fi/x")`,
		"code point":        `fetch("https://www.e` + bs + `u{78}ample.fi/x")`,
		"octal":             `fetch("https://www.e` + bs + `170ample.fi/x")`,
		"line continuation": "fetch(\"https://www.exam" + bs + "\nple.fi/x\")",
	} {
		in := "<script>" + body + "</script>"
		if out := r48HTML(t, m, in); out == in {
			t.Errorf("%s: a production origin the JS parser dereferences went out "+
				"byte-identical:\n  %s", name, in)
		}
	}
}

// TestR48TheRefJSONGateStillMissesAReferenceSpelledU: 717ad9d replaced
// hasRefJSONEsc's six fixed needles with parseURLRef, on the argument that "a
// gate narrower than the thing it guards is the same defect as a needle list
// narrower than a family". It asks the decoder about the backslash and then
// requires a *literal* `u` immediately after it — so the same argument applies
// once more: `&#117;` is `u`, the view behind the gate decodes it, and the gate
// refuses to build that view.
func TestR48TheRefJSONGateStillMissesAReferenceSpelledU(t *testing.T) {
	m, err := origin.NewMatcher([]origin.Pair{{
		Canonical: origin.MustParse("https://wt-a--example.ddev.site"),
		Variant:   origin.MustParse("https://www.example.fi"),
	}})
	if err != nil {
		t.Fatal(err)
	}

	// The gate's own example, which it does read: `&#92;` then a literal `u`.
	ctl := []byte(`{"u":"https://wt-a&#92;u002d&#92;u002dexample.ddev.site/x"}`)
	if out := HostLeaksBack(m, ctl); bytes.Equal(out, ctl) {
		t.Fatalf("the control case no longer holds, so this test measures nothing:\n  %s", out)
	}

	in := []byte(`{"u":"https://wt-a&#92;&#117;002d&#92;&#117;002dexample.ddev.site/x"}`)
	if out := HostLeaksBack(m, in); bytes.Equal(out, in) {
		t.Errorf("a variant hostname the reference view decodes went upstream "+
			"unreversed, which is the shared database taking a .ddev.site host:\n  %s", in)
	}
}

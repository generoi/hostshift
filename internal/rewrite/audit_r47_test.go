package rewrite

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
)

// Round 47.

// r47EscapedIDN is what wp_json_encode writes for www.hämeenlinna.fi.
//
// Built by concatenation rather than typed, so the six bytes are unmistakable in
// the source: backslash, 'u', '0', '0', 'e', '4'.
const r47EscapedIDN = "www.h" + "\\u00e4" + "meenlinna.fi"

func r47Matcher(t *testing.T, canon, variant string) *origin.Matcher {
	t.Helper()
	m, err := origin.NewMatcher([]origin.Pair{{
		Canonical: origin.MustParse(canon),
		Variant:   origin.MustParse(variant),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func r47HTML(t *testing.T, m *origin.Matcher, in string) string {
	t.Helper()
	src := io.NopCloser(bytes.NewReader([]byte(in)))
	out, err := io.ReadAll(NewResponseBody(src, m, src, Options{}))
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// TestR47AnIDNCanonicalEscapedByWPJSONEncodeReachesTheBrowser is test 28 on the
// spelling PHP emits by default.
//
// wp_json_encode does not pass JSON_UNESCAPED_UNICODE — proxy.go's text arm says
// so itself, and calls the result a PLAN M4 test-28 leak — so every non-ASCII
// byte of a host comes out as `\uXXXX`. §5.5 calls IDN "real for .fi client
// domains", and wp_localize_script puts a wp_json_encode blob inside an inline
// `<script>` on essentially every WordPress page.
//
// stripForJSONEsc is the view that exists to read that spelling, and jsonEscByte
// refuses every escape above 0x7E on the reasoning that "no authority byte lives
// there". An IDN authority is made of exactly those bytes. So the one surface
// where the escape is *not* decoded for us — anything that is not a JSON
// document — never sees the host at all: not the inline script, not the block
// delimiter, not a standalone text body.
//
// The inline script is the dereferenceable one. A JS string literal decodes
// `ä`, so `{"ajax":"https:\/\/www.hämeenlinna.fi\/wp-admin\/admin-ajax.php"}`
// is the URL every admin-ajax POST on the page goes to: the client's live site,
// from the developer's browser, with the developer's session, on a write path.
func TestR47AnIDNCanonicalEscapedByWPJSONEncodeReachesTheBrowser(t *testing.T) {
	m := r47Matcher(t, "https://www.hämeenlinna.fi", "https://wt-a--hml.ddev.site")

	// The same blob with the host spelled raw UTF-8 is rewritten, so neither the
	// surface, nor the `\/`, nor the IDN fold is what stops the escaped one.
	ctl := `<script>var w={"ajax":"https:\/\/www.hämeenlinna.fi\/wp-admin\/admin-ajax.php"};</script>`
	if out := r47HTML(t, m, ctl); strings.Contains(out, "meenlinna.fi") {
		t.Fatalf("the control case no longer holds, so this test is measuring nothing:\n  %s", out)
	}

	for _, in := range []string{
		// wp_localize_script / wc-settings: a JS string literal, decoded by the
		// JS parser and then fetched.
		`<script>var w={"ajax":"https:\/\/` + r47EscapedIDN + `\/wp-admin\/admin-ajax.php"};</script>`,
		// JSON-LD, read by anything that parses the document.
		`<script type="application/ld+json">{"url":"https:\/\/` + r47EscapedIDN + `\/"}</script>`,
		// A Gutenberg block delimiter, which parse_blocks json_decodes.
		`<!-- wp:image {"url":"https:\/\/` + r47EscapedIDN + `\/x"} -->`,
	} {
		if out := r47HTML(t, m, in); strings.Contains(out, r47EscapedIDN) {
			t.Errorf("the canonical origin went to the browser untouched:\n  in : %s\n  out: %s", in, out)
		}
	}

	// And the standalone entry point the proxy runs over a text/plain body and
	// every Tier 1 response header.
	txt := []byte(`https:\/\/` + r47EscapedIDN + `\/x`)
	if out := HostLeaks(m, txt, false); bytes.Equal(out, txt) {
		t.Errorf("and the text/header arm left it in place:\n  %s", out)
	}
}

// TestR47TheForwardTextArmLostThePercentComposedView: 4e8c68e moved
// composeView(stripForPercent, stripForJSONEsc) out of rewriteAll and into
// rewriteAllRefs, on the reasoning that "a urlencoded body is a *request*" and
// that in a response "the shape cannot occur".
//
// rewriteAll is not the request path. It is what the proxy runs over every Tier
// 1 response header — Location, Link, Refresh, CSP — and over a non-XML
// `text/plain` body, in the *forward* direction. Checked against 598de7c, where
// the same call returns the variant: this is a coverage move, and it moved off a
// surface a browser follows.
func TestR47TheForwardTextArmLostThePercentComposedView(t *testing.T) {
	fwd := r47Matcher(t, "https://www.acme.fi", "https://wt-a--acme.ddev.site")
	rev := r47Matcher(t, "https://wt-a--acme.ddev.site", "https://www.acme.fi")

	// The request direction still reads the composition, which is what makes
	// this a move rather than a removal — and what makes the two directions
	// disagree about one spelling.
	back := []byte(`https%3A%5C%2F%5C%2Fwt-a%5Cu002d%5Cu002dacme.ddev.site%2Fx`)
	if bytes.Equal(HostLeaksBack(rev, back), back) {
		t.Fatalf("the request direction does not read this spelling either, so this " +
			"test is not measuring what it says")
	}

	in := []byte(`https%3A%5C%2F%5C%2Fwww%5Cu002eacme.fi%2Fx`)
	if out := HostLeaks(fwd, in, false); bytes.Equal(out, in) {
		t.Errorf("the forward text/header arm left the canonical origin in place:\n"+
			"  in : %s\n  out: %s", in, out)
	}
}

// TestR47TheReferenceSpelledBackslashIsAFamily: 4e8c68e added
// composeView(stripForRefs, stripForJSONEsc) and gated it on hasRefJSONEsc,
// whose needle list is six fixed strings. The decoder behind stripForRefs is
// parseURLRef, which accepts far more spellings of the same backslash: leading
// zeros, the semicolon-less form browsers accept, and `&bsol;` — a name in this
// package's own urlNamedRefs table, annotated "the JSON separator's byte".
//
// So the gate refuses buffers the view it guards would read. On the request path
// that is the variant hostname going upstream into the shared production
// database, which §4.3 says the whole design exists to prevent.
func TestR47TheReferenceSpelledBackslashIsAFamily(t *testing.T) {
	rev := r47Matcher(t, "https://wt-a--acme.ddev.site", "https://www.acme.fi")

	// The spelling 4e8c68e added, so the composition itself is known to fire.
	const covered = `"https://wt-a&#92;u002d&#92;u002dacme.ddev.site/x"`
	if got := string(HostLeaksBack(rev, []byte(covered))); got == covered {
		t.Fatalf("the composed view does not fire at all, so this test is not "+
			"measuring the gate:\n  %s", got)
	}

	for _, in := range []string{
		// A numeric reference with a leading zero.
		`"https://wt-a&#092;u002d&#092;u002dacme.ddev.site/x"`,
		// The named reference for a backslash, from this package's own table.
		`"https://wt-a&bsol;u002d&bsol;u002dacme.ddev.site/x"`,
		// Without the terminating semicolon, which parseURLRef accepts because
		// browsers do.
		`"https://wt-a&#92u002d&#92u002dacme.ddev.site/x"`,
	} {
		if got := string(HostLeaksBack(rev, []byte(in))); got == in {
			t.Errorf("the variant hostname went upstream unread:\n  in : %s\n  out: %s",
				in, got)
		}
	}
}

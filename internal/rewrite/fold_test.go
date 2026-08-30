package rewrite

import (
	"strings"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
)

// A host that only *folds* onto a canonical one shares no bytes with it, so the
// byte matcher cannot see it on any surface.
//
// The browser's domain-to-ASCII runs UTS46 mapping before punycoding, and this
// code punycoded only. Every input here resolves to the canonical origin in a
// browser — verified against Node's WHATWG parser — and every one came out
// byte-identical, with --explain printing nothing at all: no rewrites, no
// candidates, no skips, no straggler WARN.
//
// NFD is the one that arrives without an attacker: macOS filesystems and pasted
// content produce it, and §5.5 calls IDN real for .fi client domains.
func TestFoldedHostsAreRewritten(t *testing.T) {
	m, err := origin.NewMatcher([]origin.Pair{{
		Canonical: origin.MustParse("https://www.example.fi"),
		Variant:   origin.MustParse("https://wt-a--example.ddev.site"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, host string }{
		{"soft hyphen", "www.exa­mple.fi"},
		{"fullwidth letters", "ｗｗｗ.example.fi"},
		{"ideographic full stop", "www。example。fi"},
		{"fullwidth full stop", "www．example．fi"},
		{"halfwidth ideographic full stop", "www｡example｡fi"},
		{"zero-width space", "​www.example.fi"},
		{"uppercase", "WWW.EXAMPLE.FI"},
		// UTS46 *produces* an ASCII root dot from these, and the trim used to run
		// before the fold — so the table, keyed without one, missed. A plain
		// <a href> with no userinfo, no odd slashes and no encoding trick, on
		// every surface and every content type.
		{"ideographic full stop as the root dot", "www。example。fi。"},
		{"fullwidth full stop as the root dot", "www．example．fi．"},
		{"halfwidth ideographic root dot", "www｡example｡fi｡"},
		{"a fullwidth letter and a non-ASCII root dot", "ｗww.example.fi。"},
	} {
		// Every surface, not only a URL attribute: a production origin in a text
		// node, an inline script, a stylesheet or a comment is still one the
		// browser resolves when something reads it.
		for _, s := range []struct{ name, tmpl string }{
			{"href", `<a href="https://%s/x">`},
			{"text", `see https://%s/x`},
			{"inline script", `<script>var u="https://%s/x";</script>`},
			{"inline style", `<style>a{background:url(https://%s/x)}</style>`},
			{"comment", `<!-- https://%s/x -->`},
		} {
			t.Run(c.name+" in "+s.name, func(t *testing.T) {
				in := strings.Replace(s.tmpl, "%s", c.host, 1)
				out := rewriteHTML(t, m, in, NewStats(false))
				if !strings.Contains(out, "wt-a--example.ddev.site") {
					t.Errorf("a production origin reached the browser:\n%q", out)
				}
			})
		}
	}
}

// NFD, in its own test because it needs a punycode canonical: the composed and
// decomposed spellings punycode differently, and only the fold makes them equal.
func TestNFDHostIsRewritten(t *testing.T) {
	m, err := origin.NewMatcher([]origin.Pair{{
		Canonical: origin.MustParse("https://www.xn--hmeen-gra.fi"),
		Variant:   origin.MustParse("https://wt-a--h.ddev.site"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, in string }{
		{"NFD", "<a href=\"https://www.hämeen.fi/2\">"},
		{"NFC", "<a href=\"https://www.hämeen.fi/2\">"},
		{"punycode", `<a href="https://www.xn--hmeen-gra.fi/2">`},
	} {
		t.Run(c.name, func(t *testing.T) {
			if out := rewriteHTML(t, m, c.in, NewStats(false)); !strings.Contains(out, "wt-a--h.ddev.site") {
				t.Errorf("not rewritten:\n%q", out)
			}
		})
	}
}

// The fold must not fire on a page that is already correct. It cannot run at all
// without a non-ASCII byte, and an ASCII host is the byte matcher's business.
func TestFoldIsIdentitySafe(t *testing.T) {
	m, err := origin.NewMatcher([]origin.Pair{{
		Canonical: origin.MustParse("https://www.example.fi"),
		Variant:   origin.MustParse("https://www.example.fi"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range []string{
		"<a href=\"https://www.exa­mple.fi/x\">",
		"tervetuloa äö https://www.example.fi/x",
		"<p>café // not a host</p>",
	} {
		if out := rewriteHTML(t, m, in, NewStats(false)); out != in {
			t.Errorf("identity map changed bytes:\n got %q\nwant %q", out, in)
		}
	}
}

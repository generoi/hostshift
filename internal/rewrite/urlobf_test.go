package rewrite

import (
	"io"
	"strings"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
)

func obfMatcher(t *testing.T) *origin.Matcher {
	t.Helper()
	m, err := origin.NewMatcher([]origin.Pair{{
		Canonical: origin.MustParse("https://www.example.fi"),
		Variant:   origin.MustParse("https://wt-a--example.ddev.site"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func rewriteHTML(t *testing.T, m *origin.Matcher, in string, st *Stats) string {
	t.Helper()
	r := NewResponseBody(io.NopCloser(strings.NewReader(in)), m, nil, Options{Stats: st})
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// Test 28 through the URL parser rather than through the byte model.
//
// The matcher matches a contiguous `scheme://host`. The WHATWG URL parser needs
// neither the run to be contiguous nor the separator to be two forward slashes,
// so every input here was served unrewritten and — worse — uncounted: --json
// reported a clean page and the straggler sweep saw nothing, because the sweep
// runs through the same matcher.
//
// Each of these was verified against the WHATWG parser (Node's ada, which is
// what Chrome ships) to resolve to https://www.example.fi/x, i.e. to a live
// production origin in the developer's authenticated browser.
func TestObfuscatedOriginsAreRewritten(t *testing.T) {
	m := obfMatcher(t)
	for _, c := range []struct{ name, in string }{
		{"tab in host", "<a href=\"https://www.example\t.fi/x\">"},
		{"LF in host", "<a href=\"https://www.example\n.fi/x\">"},
		{"CR in host", "<a href=\"https://www.example\r.fi/x\">"},
		{"tab in scheme", "<a href=\"htt\tps://www.example.fi/x\">"},
		{"leading tab", "<a href=\"\thttps://www.example.fi/x\">"},
		{"tab inside the separator", "<a href=\"https:/\t/www.example.fi/x\">"},
		{"backslashes", `<a href="https:\\www.example.fi/x">`},
		{"slash backslash", `<a href="https:/\www.example.fi/x">`},
		{"backslash slash", `<a href="https:\/www.example.fi/x">`},
		{"three slashes", `<a href="https:///www.example.fi/x">`},
		{"four slashes", `<a href="https:////www.example.fi/x">`},
		{"scheme relative, three slashes", `<a href="///www.example.fi/x">`},
		{"scheme relative, backslashes", `<a href="\\www.example.fi/x">`},
		{"scheme relative, tab in host", "<a href=\"//www.example\t.fi/x\">"},
		{"tab as a character reference", `<a href="https://www.example&#9;.fi/x">`},
		{"LF as a named reference", `<a href="https://www.example&NewLine;.fi/x">`},
		{"hex reference", `<a href="https://www.example&#x0A;.fi/x">`},
		{"src, not only href", `<a src="https:\\www.example.fi/x">`},
		// A scheme that differs from the document's needs no slashes at all: the
		// parser goes to special-authority-ignore-slashes, which skips a run of
		// length zero as happily as one of length three. The fleet's databases
		// carry http:// spellings of https canonicals — M0 measured one host 165
		// times over http and zero over https — so this is not an attacker shape.
		{"a different scheme, no slashes", `<a href="http:www.example.fi/x">`},
		{"a different scheme, one slash", `<a href="http:/www.example.fi/x">`},
		{"a different scheme, uppercase", `<a href="HTTP:www.example.fi/x">`},
		// Leading C0 and spaces are stripped before parsing, which moves the
		// anchor every positional rule depends on.
		{"a leading space", `<a href=" https:\\www.example.fi/x">`},
		{"a leading C0 control", "<a href=\"\x01https:\\\\www.example.fi/x\">"},
		// Userinfo pushes the host off the separator entirely.
		{"userinfo", `<a href="https://user@www.example.fi/x">`},
		{"empty userinfo", `<a href="https://@www.example.fi/x">`},
		{"userinfo with a password", `<a href="https://u:p@www.example.fi/x">`},
		{"scheme-relative userinfo", `<a href="//user@www.example.fi/x">`},
		// The host is percent-decoded before domain-to-ASCII.
		{"a percent-encoded host byte", `<a href="https://www.ex%61mple.fi/x">`},
		{"a percent-encoded first byte", `<a href="https://%77ww.example.fi/x">`},
		// The two passes have to compose: the entity decode used to return the
		// value *as written* whenever the decoded form did not itself rewrite,
		// so the URL pass never saw a decoded byte.
		{"references then a tab", `<a href="https:&#47;&#47;www.example&#9;.fi/x">`},
		{"references then extra slashes", `<a href="https:&#47;&#47;&#47;www.example.fi/x">`},
		{"named reference slashes", `<a href="&sol;&sol;&sol;www.example.fi/x">`},
		{"reference backslashes", `<a href="https:&bsol;&bsol;www.example.fi/x">`},
		{"a reference scheme colon", `<a href="https&#58;\\www.example.fi/x">`},
		// xlink:href is resolved and dereferenced for <image> and <use>.
		{"xlink:href", `<svg><image xlink:href="https:\\www.example.fi/a.png"/></svg>`},
		// A value is not always one URL. The plain spelling of every one of
		// these is caught by the anchored matcher, which knows none of their
		// grammars either — it just scans — so the obfuscated spelling has no
		// business being the exception.
		{"srcset entry", `<img srcset="https:\\www.example.fi/a.png 1x">`},
		{"a later srcset entry", `<img srcset="/local.png 1x, https:\\www.example.fi/b.png 2x">`},
		{"ping list", `<a ping="https:\\www.example.fi/p">x</a>`},
		{"meta refresh", `<meta http-equiv="refresh" content="0;url=https:\\www.example.fi/">`},
		{"css url() in a style attribute", `<div style="background:url(https:\\www.example.fi/x.png)">`},
	} {
		t.Run(c.name, func(t *testing.T) {
			out := rewriteHTML(t, m, c.in, NewStats(false))
			if strings.Contains(out, "www.example.fi") {
				t.Errorf("a production origin reached the browser:\n%s", out)
			}
			if !strings.Contains(out, "wt-a--example.ddev.site") {
				t.Errorf("nothing was rewritten:\n%s", out)
			}
		})
	}
}

// The locator ran on attribute values only, so every *ASCII* URL-parser shape
// went out untouched in an inline script, an inline stylesheet, a text node and
// a comment — with the census reporting a clean page, which is the exact
// property this file's header says was fixed one surface over.
//
// §5.2 calls inline script and style Tier 1 and "where the CSS and JS URLs
// actually are", and `fetch("https://www%2eexample%2efi/a")` is a production
// request carrying the developer's session.
func TestObfuscatedOriginsOnEverySurface(t *testing.T) {
	m := obfMatcher(t)
	for _, shape := range []string{
		`https:\\www.example.fi/x`,
		`https:///www.example.fi/x`,
		`http:www.example.fi/x`,
		`https://u@www.example.fi/x`,
		`https://www%2eexample%2efi/x`,
		"https://www.example	.fi/x",
	} {
		for _, s := range []struct{ name, tmpl string }{
			{"inline script", `<script>fetch("%s")</script>`},
			{"inline style", `<style>a{background:url(%s)}</style>`},
			{"text", `<p>%s</p>`},
			{"comment", `<!-- %s -->`},
		} {
			t.Run(s.name+" "+shape, func(t *testing.T) {
				in := strings.Replace(s.tmpl, "%s", shape, 1)
				out := rewriteHTML(t, m, in, NewStats(false))
				if strings.Contains(out, "www.example.fi") {
					t.Errorf("a production origin reached the browser:\n%s", out)
				}
			})
		}
	}
}

// CSS unescapes before the URL parser runs, so `aff` is a spelling of
// `://` that the locator cannot reach by construction and the byte matcher
// cannot see at all. Measured in Chrome, both cssText and
// getComputedStyle().backgroundImage resolve it to a live production fetch.
func TestCSSEscapesAreRewritten(t *testing.T) {
	m := obfMatcher(t)
	for _, c := range []struct{ name, in string }{
		{"style element", `<style>a{background:url("https\3a\2f\2fwww.example.fi/x")}</style>`},
		{"style attribute", `<div style="background:url(https\3a\2f\2fwww.example.fi/y)">`},
		{"escape with a terminating space", `<style>a{background:url(https\3a \2f \2f www.example.fi/z)}</style>`},
	} {
		t.Run(c.name, func(t *testing.T) {
			out := rewriteHTML(t, m, c.in, NewStats(false))
			if strings.Contains(out, "www.example.fi") {
				t.Errorf("a production origin reached the browser:\n%s", out)
			}
		})
	}
}

// A trailing dot is the host's root label in a URL and a full stop in prose.
// Absorbing it on a text surface ate the sentence's punctuation.
func TestRootDotIsPunctuationInProse(t *testing.T) {
	out := rewriteHTML(t, obfMatcher(t),
		`<p>Visit https://www.example.fi. Then leave.</p>`, NewStats(false))
	if !strings.Contains(out, "https://wt-a--example.ddev.site. Then") {
		t.Errorf("the full stop did not survive:\n%s", out)
	}
}

// XML element content begins right after `>`, and the boundary test was an
// allowlist that did not include it — so the whole obfuscated-URL family was
// invisible in a sitemap or a feed, on the arm that was added for them, while
// the same bytes one space later rewrote fine. A boundary is now anything that
// cannot be in an authority, which is how the byte matcher has always defined
// the other end of a host.
func TestBoundariesAreNotAnAllowlist(t *testing.T) {
	m := obfMatcher(t)
	for _, prefix := range []string{"", " ", ">", "<", ")", "]", "&", "#", "|", "\t", "\n"} {
		t.Run("prefix "+prefix, func(t *testing.T) {
			in := prefix + `https:\\www.example.fi/x`
			out := string(HostLeaks(m, []byte(in), false))
			if strings.Contains(out, "www.example.fi") {
				t.Errorf("a production origin survives after %q:\n%s", prefix, out)
			}
		})
	}
	// ...and a slash run inside a path is still not an authority.
	in := `https://cdn.other.test/p//www.example.fi/q`
	if out := string(HostLeaks(m, []byte(in), false)); out != in {
		t.Errorf("a path segment was rewritten:\n got %s\nwant %s", out, in)
	}
}

// Inside <svg> and <math> the HTML tokenizer never enters the raw-text states,
// so a browser decodes character references in <style>, <script> and <title>
// there — verified in Chrome, where an inline `<svg><script>` with a
// reference-encoded origin *ran*. x/net/html is context-free and hands those
// back as raw text either way, so nothing downstream could tell the difference,
// and the same SVG served standalone was rewritten while inlined in a page it
// was not.
func TestForeignContentDecodesReferences(t *testing.T) {
	m := obfMatcher(t)
	for _, c := range []struct {
		name, in string
		want     bool
	}{
		{"svg style", `<svg><style>a{background:url(https:&#47;&#47;www.example.fi/a.png)}</style></svg>`, true},
		{"svg script", `<svg><script>var u="https:&#47;&#47;www.example.fi/api";</script></svg>`, true},
		{"svg title", `<svg><title>https:&#47;&#47;www.example.fi/t</title></svg>`, true},
		{"math", `<math><style>a{background:url(https:&#47;&#47;www.example.fi/m.png)}</style></math>`, true},
		// An HTML parser does not decode inside script, so a reference there is
		// not a URL — and the depth must go back down when the svg closes.
		{"html script after an svg", `<svg><title>a</title></svg><script>var z="https:&#47;&#47;www.example.fi/h";</script>`, false},
		{"html script alone", `<script>var z="https:&#47;&#47;www.example.fi/h";</script>`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			out := rewriteHTML(t, m, c.in, NewStats(false))
			got := strings.Contains(out, "wt-a--example.ddev.site")
			if got != c.want {
				t.Errorf("rewritten=%v, want %v:\n%s", got, c.want, out)
			}
		})
	}
}

// A reference fragment that would fuse disables decodeURLRefs for the whole
// value — and the fragment can be anywhere, so one in a query string used to
// leave an ordinary origin in the same attribute undecoded and live. The
// reference *view* needs no such guard: it emits nothing, it only locates a host
// whose byte range is then replaced.
func TestAFusingFragmentDoesNotShieldAnOrigin(t *testing.T) {
	m := obfMatcher(t)
	in := `<a href="&#6&#48;;https:&#47;&#47;www.example.fi/x">y</a>`
	out := rewriteHTML(t, m, in, NewStats(false))
	if strings.Contains(out, "www.example.fi") {
		t.Errorf("a production origin a browser dereferences was left:\n%s", out)
	}
	// And the fragment itself is untouched, because nothing is re-serialised.
	if !strings.Contains(out, "&#6&#48;;") {
		t.Errorf("the declined fragment was altered:\n%s", out)
	}
}

// The census has to see them too. A leak the counters call zero is a leak
// nobody goes looking for, and --json reporting a clean page is what made this
// survive three audit rounds.
func TestObfuscatedOriginsAreCounted(t *testing.T) {
	st := NewStats(false)
	rewriteHTML(t, obfMatcher(t), `<a href="https:\\www.example.fi/x">`, st)
	if got := st.Rewrites(SurfaceHTMLObfuscated); got != 1 {
		t.Errorf("the census counts %d obfuscated rewrites, want 1: %+v",
			got, st.Snapshot())
	}
}

// The pass deletes tab, LF and CR, because the URL parser does. Everywhere else
// those bytes are content, and in srcset and ping they are separators — so it
// must not run there, and must not run on a value that holds no origin.
func TestNormalisationIsConfined(t *testing.T) {
	m := obfMatcher(t)
	for _, c := range []struct{ name, in, want string }{
		{
			"srcset keeps its whitespace separators",
			"<img srcset=\"https://www.example.fi/a.png 1x,\thttps://www.example.fi/b.png 2x\">",
			"1x,\thttps://wt-a--example.ddev.site/b.png",
		},
		{
			// The descriptors and the separator survive an obfuscated entry too:
			// only the host's bytes are replaced.
			"an obfuscated srcset keeps its descriptors",
			`<img srcset="https:\\www.example.fi/a.png 1x, https:\\www.example.fi/b.png 2x">`,
			`srcset="https:\\wt-a--example.ddev.site/a.png 1x, https:\\wt-a--example.ddev.site/b.png 2x"`,
		},
		{
			"a title is text, not a URL",
			"<p title=\"see\thttps://www.example.fi/x\">",
			"see\thttps://wt-a--example.ddev.site/x",
		},
		{
			"a path with an empty segment is not an authority",
			`<a href="/a//b">`,
			`href="/a//b"`,
		},
		{
			"an unrelated host is left exactly as written",
			`<a href="https:\\cdn.other.example/x">`,
			`href="https:\\cdn.other.example/x"`,
		},
		{
			"a value with nothing to rewrite keeps its tabs",
			"<a href=\"/search?q=a\tb\">",
			"q=a\tb",
		},
		{
			// Same scheme as the document, fewer than two slashes: the parser
			// reads this as a path, so it never reaches production and must not
			// be touched.
			"a same-scheme reference with no slashes is a path",
			`<a href="https:www.example.fi/x">`,
			`href="https:www.example.fi/x"`,
		},
		{
			// Only the host's bytes change. The separator, userinfo, port, query
			// and fragment are all copied through as written.
			"everything but the host is preserved",
			`<a href="https:\\user@www.example.fi/a b?q=1&r=2#f">`,
			`href="https:\\user@wt-a--example.ddev.site/a b?q=1&r=2#f"`,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			if out := rewriteHTML(t, m, c.in, NewStats(false)); !strings.Contains(out, c.want) {
				t.Errorf("got:\n%s\nwant it to contain:\n%s", out, c.want)
			}
		})
	}
}

// Test 24: an identity map is byte-identical, and that has to hold for every
// shape above — the pass must never fire when there is nothing to fix.
func TestObfuscationIsIdentitySafe(t *testing.T) {
	m, err := origin.NewMatcher([]origin.Pair{{
		Canonical: origin.MustParse("https://www.example.fi"),
		Variant:   origin.MustParse("https://www.example.fi"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range []string{
		"<a href=\"https://www.example\t.fi/x\">",
		`<a href="https:\\www.example.fi/x">`,
		`<a href="https:///www.example.fi/x">`,
		`<a href="https://www.example&#9;.fi/x">`,
	} {
		if out := rewriteHTML(t, m, in, NewStats(false)); out != in {
			t.Errorf("identity map changed bytes:\n got %q\nwant %q", out, in)
		}
	}
}

// An encoding composed with another one hides from all three of the engine's
// models at once.
//
// WooCommerce emits `JSON.parse(decodeURIComponent("…"))` inline, and
// percent-encoding a JSON-escaped URL gives `https%3A%5C%2F%5C%2Fhost` — one
// `%5C%2F` per `\/`. Measured on a live store: a logged-in /cart/ carried
// fourteen canonical origins that way and wp-admin eighteen, with --json
// reporting zero candidates and zero skips and diff printing GREEN.
func TestComposedEncodings(t *testing.T) {
	m := obfMatcher(t)
	for _, c := range []struct {
		name, in string
		want     bool
	}{
		{"percent-encoded JSON escapes",
			`<script>var d=decodeURIComponent("https%3A%5C%2F%5C%2Fwww.example.fi%5C%2Fshop");</script>`, true},
		{"percent-encoded plain", `<script>var d="https%3A%2F%2Fwww.example.fi%2Fshop";</script>`, true},
		{"in an attribute", `<a href="https%3A%5C%2F%5C%2Fwww.example.fi%5C%2Fx">y</a>`, true},
		// Percent-encoding that is not an origin must be left exactly as written.
		{"an ordinary escaped query", `<a href="/s?q=100%25%20off">y</a>`, false},
		{"an unrelated host", `<a href="https%3A%2F%2Fcdn.other.example%2Fx">y</a>`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			out := rewriteHTML(t, m, c.in, NewStats(false))
			got := strings.Contains(out, "wt-a--example.ddev.site")
			if got != c.want {
				t.Errorf("rewritten=%v, want %v:\n%s", got, c.want, out)
			}
			if !c.want && out != c.in {
				t.Errorf("a value with no canonical origin was changed:\n got %s\nwant %s", out, c.in)
			}
		})
	}
}

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

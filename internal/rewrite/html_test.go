package rewrite

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/net/html"

	"github.com/generoi/hostshift/internal/origin"
)

func run(t *testing.T, in string, m *origin.Matcher, opt Options) string {
	t.Helper()
	out, err := io.ReadAll(NewResponseBody(strings.NewReader(in), m, nil, opt))
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// quiet discards the sweep's warnings, which are deliberately loud.
func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestEveryAttributeScan is the heart of §5.2's surface decision: run the origin
// automaton over *every* attribute value rather than an allowlist. An allowlist
// guarantees a long tail of leaks, and the fleet already supplies three —
// style="…url(https://…)" on cover blocks, data-src/data-srcset from lazyload,
// and Yoast's JSON-LD graph on every page.
//
// The cases here are the ones §5.2 lists as needing *structured* parsing.
// Anchoring means they do not: it finds origins wherever they sit, so the
// grammar of the value — commas and descriptors in srcset, "N;url=" in a meta
// refresh, spaces in ping, entity-encoded HTML in srcdoc — never has to be
// parsed. Each of these would have needed its own parser under an allowlist.
func TestEveryAttributeScan(t *testing.T) {
	m := pairMatcher(t, "https://c.example", "https://v.example")
	cases := []struct{ name, in, want string }{
		{
			// Test 3: srcset with width descriptors and commas.
			"srcset",
			`<img srcset="https://c.example/a.jpg 1x, https://c.example/b.jpg 2000w" src="https://c.example/f.jpg">`,
			`<img srcset="https://v.example/a.jpg 1x, https://v.example/b.jpg 2000w" src="https://v.example/f.jpg">`,
		},
		{
			"meta refresh",
			`<meta http-equiv="refresh" content="5;url=https://c.example/x">`,
			`<meta http-equiv="refresh" content="5;url=https://v.example/x">`,
		},
		{
			"ping, space separated",
			`<a ping="https://c.example/p1 https://c.example/p2">x</a>`,
			`<a ping="https://v.example/p1 https://v.example/p2">x</a>`,
		},
		{
			// srcdoc is nested HTML, entity-encoded. The origins still appear
			// literally, so no recursion is needed to find them.
			"iframe srcdoc",
			`<iframe srcdoc="&lt;a href=&quot;https://c.example/x&quot;&gt;k&lt;/a&gt;"></iframe>`,
			`<iframe srcdoc="&lt;a href=&quot;https://v.example/x&quot;&gt;k&lt;/a&gt;"></iframe>`,
		},
		{
			// Test 18. §5.2's highest-severity single omission: one tag
			// re-points every relative URL at canonical and the browser leaves
			// the proxy entirely.
			"base href",
			`<base href="https://c.example/">`,
			`<base href="https://v.example/">`,
		},
		{
			// Test 21.
			"protocol-relative",
			`<img src="//c.example/pr.jpg">`,
			`<img src="//v.example/pr.jpg">`,
		},
		{
			// Test 5: a JS statement with unescaped slashes, in an inline
			// script. This is where the JS URLs actually are.
			"wpApiSettings in an inline script",
			`<script>var wpApiSettings={"root":"https://c.example/wp-json/","nonce":"a"};</script>`,
			`<script>var wpApiSettings={"root":"https://v.example/wp-json/","nonce":"a"};</script>`,
		},
		{
			// This is where the CSS URLs actually are — not in .css files
			// (§5.2 Tier 2: 88 CSS files in the fleet, zero absolute URLs).
			"inline style",
			`<style>.a{background:url(https://c.example/bg.png)}</style>`,
			`<style>.a{background:url(https://v.example/bg.png)}</style>`,
		},
		{
			"style attribute on a cover block",
			`<div class="wp-block-cover" style="background-image:url(https://c.example/hero.jpg)">x</div>`,
			`<div class="wp-block-cover" style="background-image:url(https://v.example/hero.jpg)">x</div>`,
		},
		{
			"lazyload data attributes",
			`<img data-src="https://c.example/a.jpg" data-large_image="https://c.example/b.jpg">`,
			`<img data-src="https://v.example/a.jpg" data-large_image="https://v.example/b.jpg">`,
		},
		{
			// Test 28's explicit exclusion: a bare hostname in prose is not a
			// URL and must not be rewritten.
			"bare hostname in prose",
			`<p>Order at c.example today</p>`,
			`<p>Order at c.example today</p>`,
		},
		{
			// M0 found five of these in acmecorp' production database. A browser
			// dereferences the trailing-dot form identically, so leaving it is a
			// test 28 leak.
			"trailing root dot",
			`<a href="https://c.example./x">k</a>`,
			`<a href="https://v.example/x">k</a>`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := run(t, c.in, m, Options{Log: quiet()}); got != c.want {
				t.Errorf("\n got %s\nwant %s", got, c.want)
			}
		})
	}
}

// TestRawTextElementsAreScanned is the regression test for a gap the corpus
// found during M3.
//
// The tokenizer hands back the *markup* inside a raw-text element as one opaque
// text token, so an <a href> inside <noscript>, <textarea>, <iframe> or
// <svg><title> never reaches the attribute scan. Scanning only <script> and
// <style> left all of those to §4.4's sweep — which is meant to be a backstop,
// not a load-bearing part. §5.2 listed the foreign-content case as a known gap
// to be caught by the sweep; scanning every raw-text element closes it in the
// structured pass instead, which is where §4.4 says such gaps belong.
func TestRawTextElementsAreScanned(t *testing.T) {
	m := pairMatcher(t, "https://c.example", "https://v.example")
	for _, in := range []string{
		`<noscript><a href="https://c.example/">x</a></noscript>`,
		`<svg><title><a href="https://c.example/">x</a></title></svg>`,
		`<textarea><a href="https://c.example/">x</a></textarea>`,
		`<iframe><a href="https://c.example/">x</a></iframe>`,
		`<title>https://c.example/</title>`,
	} {
		got := run(t, in, m, Options{NoSweep: true, Log: quiet()})
		if strings.Contains(got, "c.example") {
			t.Errorf("the structured pass missed a raw-text element, leaving it to the backstop: %s", got)
		}
	}
}

// TestStragglerSweep is acceptance test 29: a URL in a deliberately unhandled
// position is caught, rewritten and reported, and running the sweep twice is a
// fixed point.
//
// The fixture is a comment, which is now the honest example of a position the
// structured pass does not handle *by design* — comments are passed through
// verbatim, and a URL in one is not dereferenceable by the browser. The
// foreign-content case this used to use is handled properly now, see above.
func TestStragglerSweep(t *testing.T) {
	m := pairMatcher(t, "https://c.example", "https://v.example")
	const in = `<p>x</p><!-- see https://c.example/in-a-comment --><p>z</p>`

	if got := run(t, in, m, Options{NoSweep: true, Log: quiet()}); !strings.Contains(got, "c.example") {
		t.Fatalf("the structured pass now handles comments; this fixture no longer tests the sweep: %s", got)
	}

	st := NewStats(true)
	got := run(t, in, m, Options{Stats: st, Log: quiet()})

	if strings.Contains(got, "c.example") {
		t.Errorf("the sweep did not catch the straggler: %s", got)
	}
	if !strings.Contains(got, "https://v.example/in-a-comment") {
		t.Errorf("the straggler was not rewritten correctly: %s", got)
	}
	if n := st.Rewrites(SurfaceStraggler); n != 1 {
		t.Errorf("straggler count is %d, want 1 — each one is a bug in the structured pass and must be reported", n)
	}
	ev := st.Events()
	if len(ev) == 0 || ev[len(ev)-1].Surface != SurfaceStraggler {
		t.Errorf("the straggler was not traced for --explain: %+v", ev)
	}

	// Running the sweep twice is a fixed point.
	again := run(t, got, m, Options{Log: quiet()})
	if again != got {
		t.Errorf("the sweep is not a fixed point:\n once %s\ntwice %s", got, again)
	}
}

// TestSweepStreamsAcrossChunkBoundaries: the carry-over window has to hold a
// match together across a chunk boundary, or a straggler split by a read is
// missed. Feeding one byte at a time is the adversarial case.
func TestSweepStreamsAcrossChunkBoundaries(t *testing.T) {
	m := pairMatcher(t, "https://c.example", "https://v.example")
	in := `<p>x</p><!-- https://c.example/x --><p>` + strings.Repeat("y", 200) + `</p>`

	for _, chunk := range []int{1, 3, 17, 4096} {
		out, err := io.ReadAll(NewResponseBody(&chunkReader{b: []byte(in), n: chunk}, m, nil, Options{Log: quiet()}))
		if err != nil {
			t.Fatalf("chunk=%d: %v", chunk, err)
		}
		if bytes.Contains(out, []byte("c.example")) {
			t.Errorf("chunk=%d: a straggler survived a chunk boundary: %s", chunk, out)
		}
	}
}

// TestIdentityMapThroughTheSweep: the sweep must not break test 24 either. With
// canonical == variant it has nothing to do, and it must do nothing.
func TestIdentityMapThroughTheSweep(t *testing.T) {
	m := identityMatcher(t)
	for _, f := range corpusFiles(t) {
		in, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		out, err := io.ReadAll(NewResponseBody(bytes.NewReader(in), m, nil, Options{Log: quiet()}))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(in, out) {
			t.Errorf("%s: the sweep broke identity-map byte-identity (%d -> %d)", f, len(in), len(out))
		}
	}
}

// ---------------------------------------------------------------------------
// test 28

// urlPositions is a deliberately independent extractor: it does not use the
// matcher, so test 28 is not asserting the implementation against itself.
// It pulls every URL-valued position out of an HTML document — attribute
// values, CSS url(), and inline script/style text — and returns the hosts.
var urlRe = regexp.MustCompile(`(?i)\b(?:https?:)?(?:\\?/\\?/|%2F%2F)([a-z0-9.-]+)`)

func urlHosts(doc []byte) []string {
	var hosts []string
	add := func(b []byte) {
		for _, m := range urlRe.FindAllSubmatch(b, -1) {
			hosts = append(hosts, strings.ToLower(strings.TrimSuffix(string(m[1]), ".")))
		}
	}
	z := html.NewTokenizer(bytes.NewReader(doc))
	var raw string
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			return hosts
		}
		switch tt {
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			n := string(name)
			if tt == html.StartTagToken && (n == "script" || n == "style") {
				raw = n
			}
			for hasAttr {
				var v []byte
				_, v, hasAttr = z.TagAttr()
				add(v)
			}
		case html.EndTagToken:
			raw = ""
		case html.TextToken:
			// Text is prose. Bare hostnames there are explicitly out of scope
			// (test 28) — only script and style carry URLs.
			if raw == "script" || raw == "style" {
				add(z.Text())
			}
		}
	}
}

// TestNoDereferenceableProductionOrigin is acceptance test 28, and it is
// safety-critical: under production-canonical an unrewritten URL *works* — the
// browser leaves for the real site, and an agent could issue writes against
// production.
//
// It runs over the whole corpus with the real acmecorp canonical set.
func TestNoDereferenceableProductionOrigin(t *testing.T) {
	canonical := []string{
		"https://www.acmecorp.fi",
		"https://www.acmecorpnat.fi",
		"https://acmecorp.ddev.site",
	}
	var sites []origin.Site
	for i, c := range canonical {
		sites = append(sites, origin.Site{
			Name:      string(rune('a' + i)),
			Canonical: origin.MustParse(c),
			Variant:   origin.MustParse("https://v" + string(rune('a'+i)) + ".ddev.site"),
		})
	}
	hm, err := origin.NewMap(sites)
	if err != nil {
		t.Fatal(err)
	}
	banned := map[string]bool{}
	for _, c := range canonical {
		banned[origin.MustParse(c).Host] = true
	}

	var checked, positions int
	for _, f := range corpusFiles(t) {
		in, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		out, err := io.ReadAll(NewResponseBody(bytes.NewReader(in), hm.Forward(), nil, Options{Log: quiet()}))
		if err != nil {
			t.Fatal(err)
		}
		checked++
		for _, h := range urlHosts(out) {
			positions++
			if banned[h] {
				t.Errorf("%s: a dereferenceable canonical origin reached the browser at host %q", f, h)
			}
		}
	}
	t.Logf("test 28: %d documents, %d URL-valued positions, zero canonical origins", checked, positions)
}

// TestForeignContentGapQuantified answers §9's "quantify on the corpus during
// M3": how much does the tokenizer's lack of foreign-content tracking actually
// cost on real pages?
func TestForeignContentGapQuantified(t *testing.T) {
	m := pairMatcher(t, "https://www.acmecorp.fi", "https://v.example")
	var withSweep, withoutSweep int
	for _, f := range corpusFiles(t) {
		in, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		a := NewStats(false)
		io.ReadAll(NewResponseBody(bytes.NewReader(in), m, nil, Options{Stats: a, NoSweep: true, Log: quiet()}))
		withoutSweep += a.Total()

		b := NewStats(false)
		io.ReadAll(NewResponseBody(bytes.NewReader(in), m, nil, Options{Stats: b, Log: quiet()}))
		withSweep += b.Rewrites(SurfaceStraggler)
	}
	t.Logf("structured pass: %d rewrites; straggler sweep caught a further %d across %d documents",
		withoutSweep, withSweep, len(corpusFiles(t)))
	if withSweep > 0 {
		t.Logf("each straggler is a gap in the structured pass and should be closed there, not left to the backstop")
	}
}

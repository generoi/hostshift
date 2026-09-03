package rewrite

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
)

// Round 72. All four are RED on d9a1ba0 by design: they are the finding.
//
// Round 71 answered "does a tab, LF or CR after a host join what follows or end
// it" with origin.surfaceJoinsControls, and threaded the answer through
// delimAt, escTerminates, removedEscLen and hostTerminated. That is one half of
// the engine. The other half — rewrite's reference/URL locator — asks the same
// question in stripForURL (urlobf.go:181) and stripRemovals (urlobf.go:745),
// through isURLStripped (urlobf.go:128), and neither takes a surface at all.
// They remove tab, LF and CR unconditionally, everywhere, which is round 70's
// "always a value" reading still shipping in the half round 71 did not visit.
//
// This is round 53's two-halves-disagree again, and rewrite.TestSurfaceNames-
// AreKnownHere cannot see it: it pins origin.SurfaceJoinsControls and nothing
// in this package consults that table.
//
// Every case below was measured on a real WordPress (DDEV multisite, database
// on production hostnames, the add-on's own proxy), reading the outbound half
// off the wire and the inbound half out of MySQL with `mysql`.

func r72Fwd(t *testing.T) *origin.Matcher { // canonical -> variant (response)
	t.Helper()
	m, err := origin.NewMatcher([]origin.Pair{{
		Canonical: origin.MustParse("https://www.r72a.example"),
		Variant:   origin.MustParse("https://wt-a--r72w-hs.ddev.site"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func r72Rev(t *testing.T) *origin.Matcher { // variant -> canonical (request)
	t.Helper()
	m, err := origin.NewMatcher([]origin.Pair{{
		Canonical: origin.MustParse("https://wt-a--r72w-hs.ddev.site"),
		Variant:   origin.MustParse("https://www.r72a.example"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func r72JSON(t *testing.T, m *origin.Matcher, in string) string {
	t.Helper()
	return string(RewriteJSON([]byte(in), m, NewStats(false), slog.Default(), false))
}

// A. The locator's own spellings, at the end of a line, in prose.
//
// The byte matcher cannot see either of these: `&#47;&#47;` is a character
// reference and `https:\\host` has neither `//`, `\/` nor `%2F` for the
// prefilter. The locator is the only pass that can, and it strips the LF before
// it looks, so the host reads as `www.r72a.examplenext` and no map names it.
//
// Both spellings are enumerated in this package as real and safety-critical —
// json.go's decodeJSONLeak header lists character references in
// content.rendered, and HostLeaks' header lists `https:\\host` as "a variant
// hostname was written into the shared database".
//
// Measured, post 8 on the fixture, one paragraph, one save:
//
//	ENTITY-LINEEND https:&#47;&#47;www.r72a.example
//	next line. BACKSLASH-LINEEND https:\\www.r72a.example
//	next line. ENTITY-SPACE https:&#47;&#47;www.r72a.example ok. …
//
// Served at the variant, the two SPACE controls came back as the variant and
// the two LINEEND origins came back as production — in the HTML page, in
// GET /wp-json/wp/v2/posts/8 (content.rendered, where the entity form had been
// decoded to a plain `https://www.r72a.example`), and in /feed/'s
// content:encoded. Node's URL resolves `https:\\www.r72a.example` to the host
// www.r72a.example, so that one is dereferenceable as written.
func TestR72ALocatorSpellingsAtALineEndInProse(t *testing.T) {
	fwd, rev := r72Fwd(t), r72Rev(t)
	for _, tc := range []struct {
		name, in, gone string
		m              *origin.Matcher
	}{
		{"out/entity", "<p>A https:&#47;&#47;www.r72a.example\nnext line.</p>", "www.r72a.example", fwd},
		{"out/backslash", `<p>A https:\\www.r72a.example` + "\nnext line.</p>", "www.r72a.example", fwd},
		{"in/entity", "<p>A https:&#47;&#47;wt-a--r72w-hs.ddev.site\nnext line.</p>", "wt-a--r72w-hs.ddev.site", rev},
		{"in/backslash", `<p>A https:\\wt-a--r72w-hs.ddev.site` + "\nnext line.</p>", "wt-a--r72w-hs.ddev.site", rev},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The control: the same spelling with a space after it, which the
			// locator does rewrite. Only what follows the host differs.
			ctl := strings.Replace(tc.in, "\n", " ", 1)
			out := rewriteHTML(t, tc.m, tc.in, NewStats(false))
			ctlOut := rewriteHTML(t, tc.m, ctl, NewStats(false))
			if strings.Contains(ctlOut, tc.gone) {
				t.Fatalf("control did not rewrite either; the case is not about the newline\n  %q", ctlOut)
			}
			if strings.Contains(out, tc.gone) {
				t.Errorf("%q survives a line end on a prose surface\n  in:  %q\n  out: %q\n"+
					"  same bytes with a space instead: %q", tc.gone, tc.in, out, ctlOut)
			}
		})
	}
}

// B. The same, through RewriteJSON, which is how the block editor and every
// wp.apiFetch move a document.
//
// Measured: POST /wp-json/wp/v2/posts/5 through the proxy with
// {"content":"… LINEEND https:&#47;&#47;wt-a--r72w-hs.ddev.site\nnext line …"}
// stored `https://wt-a--r72w-hs.ddev.site` in wp_posts.post_content and in the
// revision row beside it, while two controls in the same field came home to
// www.r72a.example. §4.3, no undo.
func TestR72BLocatorSpellingsInAJSONDocument(t *testing.T) {
	for _, tc := range []struct {
		name, in, gone string
		m              *origin.Matcher
	}{
		{"out", `{"content":"see https:&#47;&#47;www.r72a.example\nnext line here"}`,
			"www.r72a.example", r72Fwd(t)},
		{"in", `{"content":"see https:&#47;&#47;wt-a--r72w-hs.ddev.site\nnext line here"}`,
			"wt-a--r72w-hs.ddev.site", r72Rev(t)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := r72JSON(t, tc.m, tc.in); strings.Contains(got, tc.gone) {
				t.Errorf("%q survives a line end in a JSON document\n  in:  %s\n  out: %s",
					tc.gone, tc.in, got)
			}
		})
	}
}

// C. The straggler joins, and on three paths it is not backing anything up.
//
// origin.surfaceJoinsControls' comment justifies "straggler" => true with "it
// can only ever be more conservative than the pass it backs up". On
// proxy.go:596, 664 and 844 there is no pass in front of it: RewriteJSON
// returns the body untouched whenever jsontext rejects the document — a
// duplicate object member is legal JSON and is rejected — and SweepBytes is
// then the only pass that touches it. Reading a URL parser's semantics there
// declines an origin nothing else will look at.
//
// No obfuscation is needed: this is a plain `https://host`.
//
// Measured through the proxy against a two-line PHP file in the fixture's
// docroot emitting a duplicate member. Out:
//
//	{"a":"see https://www.r72a.example\nnext line here","a":"dup",
//	 "b":"see https://wt-a--r72w-hs.ddev.site next line"}
//
// and in, read off the web container's filesystem rather than through the
// proxy, the variant survived into the body upstream received.
func TestR72CTheSweepIsSometimesTheOnlyPass(t *testing.T) {
	for _, tc := range []struct {
		name, in, gone string
		m              *origin.Matcher
	}{
		{"out", `{"a":"see https://www.r72a.example\nnext line here"}`, "www.r72a.example", r72Fwd(t)},
		{"in", `{"a":"see https://wt-a--r72w-hs.ddev.site\nnext line here"}`, "wt-a--r72w-hs.ddev.site", r72Rev(t)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := string(SweepBytes([]byte(tc.in), tc.m, NewStats(false), slog.Default()))
			if strings.Contains(got, tc.gone) {
				t.Errorf("%q survives the sweep, which is the only pass on RewriteJSON's "+
					"decline path\n  in:  %s\n  out: %s", tc.gone, tc.in, got)
			}
		})
	}
}

// D. `inline-script` cannot answer for a <script> that is not JavaScript.
//
// `json-string` got joinsControlsIn because "the surface cannot answer for
// JSON". A <script> element has the same problem and does not have to guess at
// it: its `type` attribute says so. `application/ld+json` is JSON — every SEO
// plugin in the fleet emits it, and pasting a block of it into a Custom HTML
// block is ordinary WordPress authoring — and a `\n` in one of its strings is a
// line break, not a byte a URL parser strips. `text/template` and
// `text/html` (wp-admin's media modals, the customizer) are markup.
//
// Measured: post 12 on the fixture, a Custom HTML block holding
//
//	<script type="application/ld+json">
//	{"@context":"https://schema.org","@type":"Article","description":
//	 "LDLINEEND https://www.r72a.example\nnext line. LDSPACE https://www.r72a.example ok."}
//	</script>
//
// served at the variant with LDSPACE rewritten and LDLINEEND still production.
// The inbound half is clean, because the request reads the whole post_content
// as one JSON string that does contain spaces — so this one is asymmetric: what
// goes out as production comes back as production.
func TestR72DAScriptTypeSaysWhetherItIsJavaScript(t *testing.T) {
	in := `<script type="application/ld+json">` +
		`{"description":"LDLINEEND https://www.r72a.example` + `\n` + `next line."}` +
		`</script>`
	got := rewriteHTML(t, r72Fwd(t), in, NewStats(false))
	if strings.Contains(got, "www.r72a.example") {
		t.Errorf("a production origin in a JSON-LD string reached the browser — test 28\n"+
			"  in:  %s\n  out: %s", in, got)
	}
}

// E. joinsControlsIn's space heuristic, in both directions.
//
// The discriminant is "does this string contain a literal space, after trimming
// quotes, and does it start with http or //". A prose field with no space in it
// reads as a lone URL value, so a `\n` joins and the origin before it is
// declined; adding one space anywhere in the same field flips the whole field.
//
// Measured: POST /wp-json/wp/v2/posts/9 through the proxy with
// {"content":"https://wt-a--r72w-hs.ddev.site\nhttps://wt-a--r72w-hs.ddev.site/a"}
// stored
//
//	https://wt-a--r72w-hs.ddev.site
//	https://www.r72a.example/a
//
// in wp_posts.post_content and in the revision — a variant hostname in the
// shared database, §4.3. The identical body with " x" appended stored both
// origins as www.r72a.example.
func TestR72EOneSpaceDecidesAWholeField(t *testing.T) {
	rev := r72Rev(t)
	const noSpace = `{"content":"https:\/\/wt-a--r72w-hs.ddev.site\nhttps:\/\/wt-a--r72w-hs.ddev.site\/a"}`
	const oneSpace = `{"content":"https:\/\/wt-a--r72w-hs.ddev.site\nhttps:\/\/wt-a--r72w-hs.ddev.site\/a x"}`
	a, b := r72JSON(t, rev, noSpace), r72JSON(t, rev, oneSpace)
	if strings.Contains(b, "wt-a--r72w-hs.ddev.site") {
		t.Fatalf("the control did not come home either; the case is not about the space\n  %s", b)
	}
	if strings.Contains(a, "wt-a--r72w-hs.ddev.site") {
		t.Errorf("a variant hostname reached the shared database because the field held "+
			"no space — §4.3\n  in:  %s\n  out: %s\n  same field plus one space: %s",
			noSpace, a, b)
	}
}

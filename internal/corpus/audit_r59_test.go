package corpus

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
	"github.com/generoi/hostshift/internal/rewrite"
)

// Round 59, on 05ae5ea. `hostshift diff`'s Location scorer, the mirror of the
// response-header emission pipeline.

// Round 58 moved the scorer onto the pipeline the proxy runs and left half of it
// behind.
//
// `compare` now computes:
//
//	RepairSerialized(Rewrite(SurfaceResponseHeader) → HostLeaks(…, true))
//
// and `HostLeaks` takes no surface: it renames the buffer with `bareSurface`,
// which for `value == true` is `SurfaceHTMLAttr`. `surfaceDecodesCSS` is written
// as `surface != SurfaceResponseHeader`, so the rename turns the CSS view back
// on — in the one stage of the expression that does the leak-backstop work. The
// proxy calls `HostLeaksCounted(…, SurfaceResponseHeader, 0)` and gets the view
// off. So the scorer still expects, on exactly the shapes round 58's own finding
// was about, something the proxy does not emit: measured over the corpus,
// 3,622 header-safe Location spellings (3,496 css-encoded, 126 raw).
//
// ada, with the variant origin as base:
//
//	new URL("/go.php?u=https://c.exampl\\65/x", "https://v.example/p").href ===
//	  "https://v.example/go.php?u=https://c.exampl\\65/x"
//
// The backslash survives into the query byte for byte: `\65` is a *CSS* hex
// escape for `e`, and neither the URL parser that follows the Location nor the
// PHP that reads `u` runs a CSS tokenizer. So the value names `c.exampl` and
// this map's canonical is nowhere in it. The proxy is right to leave it alone;
// the scorer wants it turned into the variant and prints a RED whose "want" is
// an origin ada never resolves this to, on the run the README calls the check
// that validates a deployment against reality. That is PLAN §566 again: a
// carve-out must be as narrow in the check as it is in the code.
//
// A relative Location and not an absolute one, because `diff`'s own fetcher
// cannot carry the absolute form: `url.Parse` rejects a backslash in a host, so
// `compare` reports "failed to parse Location header" before the scorer is
// reached at all. The reachable half is the ordinary one — a `redirect_to`
// carried in the query, in the spelling a form encoder writes.
func TestR59TheScorerRunsTheBackstopOnTheHeaderSurface(t *testing.T) {
	m := testMap(t)

	for _, loc := range []string{
		// The literal CSS escape, and the percent-encoded spelling `post.php`
		// sends back — the `hasPercentCSSEsc` cell, gated by the same call.
		`/go.php?u=https://c.exampl\65/x`,
		`/go.php?u=https%3A%2F%2Fc.exampl%5C65%2Fx`,
	} {
		t.Run(loc, func(t *testing.T) {
			results, err := Run(context.Background(), Options{
				Canonical: r58Redirector(t, loc),
				Variant:   r58Redirector(t, loc),
				Map:       m, N: 3,
			})
			if err != nil {
				t.Fatal(err)
			}
			var buf bytes.Buffer
			if !WriteReport(&buf, results) {
				t.Errorf("nothing decodes a CSS escape in a Location, so the proxy correctly\n"+
					"leaves this alone and both sides are byte-identical — the scorer\n"+
					"red-flags it anyway, because HostLeaks renames the surface to\n"+
					"html-attr and turns the CSS view back on:\n%s", buf.String())
			}
			if s := buf.String(); strings.Contains(s, "v.example%2Fx") ||
				strings.Contains(s, variantOrigin+"/x") {
				t.Errorf("the scorer's `want` names %s, which nothing resolves this "+
					"Location to:\n%s", variantOrigin, s)
			}
		})
	}
}

// And the half round 58 did add, which nothing pins: the length re-emission.
//
// `RepairSerialized` is what makes a `Location` carrying a serialized blob come
// out with a length that still describes it. Removing the wrapper from `compare`
// leaves every test in the tree green, so the scorer could drift back to
// expecting `s:30` against a proxy that emits `s:39` — and a stale length inside
// an `s:N:"…"` is the wp_options corruption serialized.go exists to prevent.
//
// The expectation is arithmetic, not a second reading of the code:
// `https://www.example.fi/landing` is 30 bytes and
// `https://wt-a--example.ddev.site/landing` is 39.
func TestR59TheScorerRepairsTheLengthItReEmits(t *testing.T) {
	m, err := origin.NewMap([]origin.Site{{
		Name:      "main",
		Canonical: origin.MustParse("https://www.example.fi"),
		Variant:   origin.MustParse("https://wt-a--example.ddev.site"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	const canonLoc = `https://www.example.fi/go.php?state=s:30:"https://www.example.fi/landing";`
	const variantLoc = `https://wt-a--example.ddev.site/go.php?state=s:39:"https://wt-a--example.ddev.site/landing";`

	results, err := Run(context.Background(), Options{
		Canonical: r58Redirector(t, canonLoc),
		Variant:   r58Redirector(t, variantLoc),
		Map:       m, N: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if !WriteReport(&buf, results) {
		t.Errorf("the proxy repairs the length when it splices a longer host into a\n"+
			"serialized Location; a scorer that does not expect the repair red-flags\n"+
			"the correct emission and its `want` carries a stale s:30:\n%s", buf.String())
	}
}

// The scorer's census names the arm it ran, like the proxy's.
//
// Round 60 split the text arm by media type in both engines and said they "make
// the same choice by the same question". Mutating the scorer's `st.Record`
// surface back to `text` survived the whole suite: the entire census for the
// commonest XML case — a feed `<link>` — moves to the wrong surface unnoticed,
// on the field `check` tells a developer to grep at a test-28 refusal.
func TestR61TheScorerCensusNamesTheArm(t *testing.T) {
	m, err := origin.NewMatcher([]origin.Pair{{
		Canonical: origin.MustParse("https://www.canon.test"),
		Variant:   origin.MustParse("https://v.ddev.site"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ ctype, body, want string }{
		{"application/rss+xml",
			`<rss><channel><item><link>https://www.canon.test/x</link></item></channel></rss>`,
			rewrite.SurfaceXMLText},
		{"text/plain", `see https://www.canon.test/x here`, rewrite.SurfaceText},
	} {
		t.Run(c.ctype, func(t *testing.T) {
			st := rewrite.NewStats(false)
			if _, err := applyLikeTheProxy(m, []byte(c.body), c.ctype, st); err != nil {
				t.Fatal(err)
			}
			if st.Snapshot().Rewrites[c.want] == 0 {
				t.Errorf("a %s body rewrote nothing under %q; the census says %v",
					c.ctype, c.want, st.Snapshot().Rewrites)
			}
		})
	}
}

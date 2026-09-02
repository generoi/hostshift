package rewrite

import (
	"strings"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
)

// Round 65, on dbe39bd, auditing the surface rounds 60–64 have each broken in
// the previous round's fix: the foreign-content state machine.
//
// The model now has a stack of open `<svg>`/`<math>` elements, the vocabulary
// each child is read in, a depth at which an integration point resumed HTML
// rules, 13.2.6.5's breakout list, and a byte span for a `<svg>` re-entered
// inside a raw-text token. What it does not have is the *exception* built into
// the integration-point rule itself.
//
// HTML 13.2.6 dispatches a token to the HTML insertion mode when "the adjusted
// current node is a MathML text integration point and the token is a start tag
// whose tag name is **neither `mglyph` nor `malignmark`**". Those two names are
// the carve-out: inside `<mi>`, `<mo>`, `<mn>`, `<ms>` or `<mtext>` they are
// processed by the foreign-content rules instead, so they are inserted as
// *MathML* elements — and once one of them is the current node, the parser is
// back in foreign content and decodes character references in everything below
// it, including a `<script>` it will run and a `<style>` whose `@import` it
// fetches.
//
// hostshift's model sets `foreignObjectAt` on `<mi>`/`<mtext>` and then sees
// `<mglyph>` as an unremarkable name, so `inForeignContent()` stays false for
// the rest of the subtree and rewriteValueInner withholds `refsLeak`. The
// canonical origin is served through byte-identical, inside a script the page
// runs. That is test 28.
//
// Verified against the oracle this file's predecessors use — x/net/html's
// *parser*, which implements 13.2.6 — both as "does the text decode"
// (parserDecodesReferences) and as the namespace it assigns:
//
//	<math><mi><mglyph><script>…  =>  <mglyph> ns="math", <script> ns="math"
//	<math><mi><span><script>…    =>  <span>   ns="",     <script> ns=""
//
// The second is the control: an ordinary start tag in the same position *does*
// resume HTML rules, and hostshift is right about it. Only the two carve-out
// names are wrong, and they are wrong in the direction that leaks.
func TestR65MathTextIntegrationPointCarveOut(t *testing.T) {
	m := obfMatcher(t)
	for _, ip := range []string{"mi", "mo", "mn", "ms", "mtext"} {
		for _, ex := range []string{"mglyph", "malignmark"} {
			for _, holder := range []struct{ name, doc string }{
				{"script", `<script>fetch("` + r61Ref + `")</script>`},
				{"style", `<style>@import url("` + r61Ref + `")</style>`},
				{"iframe", `<iframe>` + r61Ref + `</iframe>`},
				{"xmp", `<xmp>` + r61Ref + `</xmp>`},
			} {
				name := ip + "/" + ex + "/" + holder.name
				t.Run(name, func(t *testing.T) {
					doc := "<math><" + ip + "><" + ex + ">" + holder.doc
					if !parserResolvesCanonical(t, doc) {
						t.Fatalf("oracle: the parser does not resolve the canonical origin "+
							"here, so this fixture no longer tests the claim:\n  %s", doc)
					}
					out := rewriteHTML(t, m, doc, nil)
					if !strings.Contains(out, "wt-a--example.ddev.site") {
						t.Errorf("`<%s>` inside `<math><%s>` is processed by the foreign-content\n"+
							"  rules — 13.2.6's integration-point dispatch excepts exactly `mglyph`\n"+
							"  and `malignmark` — so the parser is back in foreign content and\n"+
							"  decodes `&#47;&#47;` below it. hostshift's model still says HTML rules\n"+
							"  resumed at `<%s>` and withheld the reference view, serving this map's\n"+
							"  canonical origin through byte-identical. Test 28.\n"+
							"  in:  %s\n  out: %s", ex, ip, ip, doc, out)
					}
				})
			}
		}
	}
}

// The control: an ordinary start tag in the same position really does resume
// HTML rules, and hostshift must *not* decode there. This is the half round 61
// broke in the other direction, kept here so a fix for the carve-out cannot be
// "treat everything inside `<mi>` as foreign".
func TestR65MathTextIntegrationPointStillResumesHTML(t *testing.T) {
	m := obfMatcher(t)
	for _, ip := range []string{"mi", "mo", "mn", "ms", "mtext"} {
		for _, ord := range []string{"span", "div", "b", "svgx"} {
			t.Run(ip+"/"+ord, func(t *testing.T) {
				doc := "<math><" + ip + "><" + ord + `><script>fetch("` + r61Ref + `")</script>`
				if parserResolvesCanonical(t, doc) {
					t.Skipf("oracle says the parser decodes here: %s", doc)
				}
				out := rewriteHTML(t, m, doc, nil)
				if strings.Contains(out, "wt-a--example.ddev.site") {
					t.Errorf("the parser does not decode references here, so nothing in this\n"+
						"  script points at production and rewriting it corrupts bytes the\n"+
						"  page runs literally.\n  in:  %s\n  out: %s", doc, out)
				}
			})
		}
	}
}

// There is no one urlencoded encoder, so the peel stopped asking for one.
//
// A `<form>` POST, `URLSearchParams`, and jQuery's `encodeURIComponent` with
// `%20`→`+` — which is what WordPress core posts from `admin-ajax.php` and the
// Customizer — disagree on `!`, `'`, `(`, `)` and `~`. Round 64's guard required
// the value to re-encode byte-identically under one of them, so every
// disagreement declined the peel whole and the variant hostname went upstream
// into the shared database. `url()` in CSS always holds parens, and the
// Customizer posts every setting in a single `customized` field, so one
// apostrophe in a tagline leaked the whole payload. Read back out of a real
// database, and served from the canonical hostname afterwards.
//
// The peel splices now: only the changed bytes are re-encoded, and every other
// byte keeps the spelling the sender used.
func TestR65AnySendersEncodingComesHome(t *testing.T) {
	rev, err := origin.NewMatcher([]origin.Pair{{
		Canonical: origin.MustParse("https://wt-a--example.ddev.site"),
		Variant:   origin.MustParse("https://www.example.fi"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	rw := func(b []byte) []byte {
		out, _ := rev.Rewrite(b, SurfaceRequestBody, false)
		return out
	}
	const dbl = "https%253A%252F%252Fwt-a--example.ddev.site%252Fbg.png"
	const want = "https%253A%252F%252Fwww.example.fi%252Fbg.png"
	for _, c := range []struct{ name, in, out string }{
		{"the Customizer's url() with parens",
			"customized=.hero%7Bbackground%3Aurl(" + dbl + ")%7D",
			"customized=.hero%7Bbackground%3Aurl(" + want + ")%7D"},
		{"an apostrophe jQuery leaves raw", "opt=" + dbl + "'y", "opt=" + want + "'y"},
		{"the whole set jQuery leaves raw", "opt=" + dbl + "!~()", "opt=" + want + "!~()"},
		{"%20 stays %20 and is not normalised to +",
			"opt=" + dbl + "%20y", "opt=" + want + "%20y"},
		{"a + stays a +", "opt=a+b+" + dbl, "opt=a+b+" + want},
		// An invalid escape is three literal bytes to the WHATWG parser and to
		// PHP, so it is three literal bytes here. Declining the field instead
		// meant one stray `%` — `50% off` in a setting — carried the variant
		// hostname all the way into the database.
		{"a stray % does not stop the origin coming home",
			"opt=50%25%20off%20" + dbl + "%20x%zz", "opt=50%25%20off%20" + want + "%20x%zz"},
		{"a lone trailing %", "opt=" + dbl + "%", "opt=" + want + "%"},
		{"nothing to rewrite beside a stray %", "opt=50% off", "opt=50% off"},
		// A stray escape *between* two rewrites falls inside the spliced range,
		// so it is the one place its spelling does change — to `%25zz`, which
		// PHP and the WHATWG parser both decode back to `%zz`. Decoding it as a
		// byte instead would emit `%00` and hand the app a value it never sent.
		{"a stray % between two origins stays the same value",
			"opt=" + dbl + "%zz" + dbl, "opt=" + want + "%25zz" + want},
		{"nothing to do", "opt=nothing-to-do", "opt=nothing-to-do"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := string(RepairSerializedFields([]byte(c.in), rw)); got != c.out {
				t.Errorf("the sender's encoding decided whether the origin came home\n"+
					"  in:   %s\n  got:  %s\n  want: %s", c.in, got, c.out)
			}
		})
	}
}

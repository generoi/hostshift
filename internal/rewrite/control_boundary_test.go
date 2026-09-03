package rewrite

import (
	"strings"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
)

// A generated grid over the one question rounds 70, 71 and 72 each answered in a
// different place: when a control follows a host, does it join what follows or
// end it?
//
// Each of those rounds fixed it where it was being looked at and missed another
// pass that never asked. Round 70 the byte matcher; round 71 the JSON escape
// spelling, the sole-pass sweep and the script surface; round 72 the *locator* —
// the only pass that sees a character reference, a `\`-obfuscated separator or a
// JSON escape, so every one of those spellings kept joining across a line break
// while the matcher no longer did.
//
// `TestSurfaceNamesAreKnownHere` pins the axis for the surfaces the matcher
// consults, and could not see a pass that never asks at all — which is how round
// 72's LARGE hid. This runs whole documents through the shipped pipelines
// instead, so a spelling only one pass can see is still covered by whichever pass
// ends up handling it.
//
// Written at the pipeline level deliberately. The first version called
// `Matcher.RewriteText` directly and reported fifteen failing cells, all of them
// spellings the byte matcher was never responsible for — the eighth harness error
// in this project, and the same shape as the rest: measuring a part and reporting
// on the whole.

// controlSpellings are the ways a page spells an origin that only some passes can
// see. `%s` takes the host.
var controlSpellings = []struct{ name, tmpl string }{
	{"plain", "https://%s"},
	{"entity-slash", "https:&#47;&#47;%s"},
	{"entity-hex", "https:&#x2f;&#x2f;%s"},
	{"backslash", `https:\\%s`},
	{"scheme-relative", "//%s"},
}

// controlContexts are the markup a prose origin sits in. Each `%s` takes the
// text, which ends a line on the origin.
var controlContexts = []struct{ name, tmpl string }{
	{"paragraph", "<p>%s</p>"},
	// No comment context. A browser does not decode character references in
	// comment data (13.2.5.4 consumes it in the comment state), while
	// x/net/html's parser does — so the two oracles disagree and this grid
	// cannot adjudicate whether an entity-spelled origin in a comment is
	// dereferenceable at all. Comments are still rewritten, for the copy-paste
	// reason §4.4 gives; that is just not this property.
	{"title", "<title>%s</title>"},
	{"textarea", "<textarea>%s</textarea>"},
	{"svg text", "<svg><text>%s</text></svg>"},
	{"noscript", "<noscript>%s</noscript>"},
}

func TestAControlAfterAHostIsAnsweredTheSameWayByEveryPass(t *testing.T) {
	m := obfMatcher(t)
	const canon = "www.example.fi"
	const variant = "wt-a--example.ddev.site"

	checked := 0
	var bad []string

	// Outward: an origin ending a line, in every spelling, in every prose
	// context. The line ends on the origin, so it is a whole origin and the
	// browser resolves it — leaving it is test 28.
	for _, sp := range controlSpellings {
		lit := strings.Replace(sp.tmpl, "%s", canon, 1)
		for _, cx := range controlContexts {
			in := strings.Replace(cx.tmpl, "%s", "see "+lit+"\nnext line", 1)
			out := rewriteHTML(t, m, in, nil)
			checked++
			if strings.Contains(out, canon) {
				bad = append(bad, "  ["+cx.name+" / "+sp.name+"] left naming production"+
					"\n    in:  "+in+"\n    out: "+out)
			}
		}
	}

	// Inward: the same spellings coming back through the request arms, where the
	// damage is a row in production's database.
	rev, err := origin.NewMatcher([]origin.Pair{{
		Canonical: origin.MustParse("https://" + variant),
		Variant:   origin.MustParse("https://" + canon),
	}})
	if err != nil {
		t.Fatal(err)
	}
	// Composed the way the proxy's request arm composes it: the byte matcher and
	// then the locator half. Asking `Rewrite` alone measures one pass and reports
	// on the whole, which is what the first version of this test did.
	st := NewStats(false)
	rw := func(b []byte) []byte {
		out, _ := rev.Rewrite(b, SurfaceRequestBody, false)
		return HostLeaksBackCounted(rev, out, st, SurfaceRequestBody, 0)
	}
	for _, sp := range controlSpellings {
		lit := strings.Replace(sp.tmpl, "%s", variant, 1)
		body := "see " + lit + "\nnext line here"

		got := string(RepairSerializedFields([]byte("field="+body), rw))
		checked++
		if strings.Contains(got, variant) {
			bad = append(bad, "  [form body / "+sp.name+
				"] a variant hostname reached the shared database — §4.3, no undo"+
				"\n    in:  field="+body+"\n    out: "+got)
		}

		// And as a JSON document, which is what the block editor posts —
		// RewriteJSON *and* the sweep behind it, which is what the proxy's JSON
		// arm runs. Omitting the sweep reported a leak the shipped path does not
		// have: the fourth harness correction in this one test, and every one of
		// them the same shape — a part measured, the whole reported on.
		j := `{"content":"` + strings.ReplaceAll(lit, `\`, `\\`) + `\nnext line here"}`
		jgot := string(SweepBytes(RewriteJSON([]byte(j), rev, st, quiet(), false),
			rev, st, quiet()))
		checked++
		if strings.Contains(jgot, variant) {
			bad = append(bad, "  [json body / "+sp.name+
				"] a variant hostname reached the shared database — §4.3, no undo"+
				"\n    in:  "+j+"\n    out: "+jgot)
		}
	}

	if checked == 0 {
		t.Fatal("nothing was checked, so this asserts nothing")
	}
	if len(bad) > 0 {
		t.Errorf("%d of %d cells answer the control question differently from the "+
			"pass beside them:\n%s", len(bad), checked, strings.Join(bad, "\n"))
	}
	t.Logf("%d cells agree across the spellings", checked)
}

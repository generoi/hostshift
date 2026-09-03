package corpus

import (
	"bytes"
	"strings"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
)

// The literal-scan pointer fires on every Tier 2 body, where nothing was
// declined and the engine was never asked.
//
// Round 74 added `Result.Literal` for one purpose: to catch a canonical origin
// that the *engine* saw and let through, because `countLeaks` re-runs the
// pipeline and so cannot witness its own declines. `WriteReport` reports the
// difference as exactly that — "the difference is something the engine
// declined".
//
// For a Tier 2 body that sentence is false by construction. `countLeaks`
// short-circuits at `isTier2` and returns `leaks = 0` before the engine is run
// at all (diff.go:529), while `literalOrigins` still counts every `//host` in
// the same bytes. So `r.Literal > r.Leaks` holds for *any* Tier 2 body carrying
// a literal canonical origin, and the same origins are then added to two
// separate totals — the "a literal scan found and the engine did not" line and
// the "origins in Tier 2 types" line.
//
// Measured on a real deployment rather than argued: a DDEV WordPress with
// Autoptimize 3.1.15.1 aggregating inline CSS/JS, `hostshift diff` printed
//
//	/wp-content/cache/autoptimize/1/autoptimize_0…  same  0  25/25
//	  a literal scan finds 2 canonical origin(s) here and the engine reported 0
//	  …; 2 origins in a Tier 2 type (text/css; charset=utf-8)
//	…
//	8 canonical origin(s) a literal scan found and the engine did not
//	10 origins in Tier 2 types (text/css, JavaScript)
//
// Eight of those ten are the same origins counted twice, and all eight of the
// literal-scan hits on that run were Tier 2 bodies — so the pointer added to
// find engine declines found none, and reported eight. PLAN's own rule about
// `mayHoldSerialized` applies: a check that fires on every page carrying the
// thing it is supposed to be quiet about carries no information.
func TestR75LiteralPointerFiresOnEveryTier2Body(t *testing.T) {
	c, err := origin.Parse("https://www.r75a.example")
	if err != nil {
		t.Fatal(err)
	}
	v, err := origin.Parse("https://wt-a--r75w.ddev.site")
	if err != nil {
		t.Fatal(err)
	}
	m, err := origin.NewMap([]origin.Site{{Name: "s", Canonical: c, Variant: v}})
	if err != nil {
		t.Fatal(err)
	}

	// An Autoptimize-shaped aggregate: a theme's @font-face, absolutised into
	// the cache file because the file moved directory.
	css := []byte(`@font-face{font-family:Manrope;src:url('https://www.r75a.example` +
		`/wp-content/themes/tt5/assets/fonts/manrope/Manrope.woff2') format('woff2')}` +
		`.x{background:url("https://www.r75a.example/wp-content/uploads/bg.png")}`)

	r := response{body: css, status: 200, contentType: "text/css; charset=utf-8"}
	leaks, tier2 := countLeaks(m.Forward(), r)
	lit := literalOrigins(m, css)

	if !isTier2(r.contentType) {
		t.Fatal("harness: text/css is supposed to be a Tier 2 type")
	}
	if lit == 0 {
		t.Fatal("harness: the fixture carries no literal canonical origin")
	}
	if leaks != 0 {
		t.Fatalf("harness: countLeaks is documented to return 0 leaks for a "+
			"Tier 2 body, got %d", leaks)
	}
	if tier2 != lit {
		t.Logf("tier2=%d literal=%d — they need not be equal, only both non-zero",
			tier2, lit)
	}
	// This is the whole point: the report's condition is satisfied without the
	// engine having declined anything, because the engine was never run.
	if !(lit > leaks) {
		t.Fatalf("expected the literal pointer's condition to hold on a Tier 2 "+
			"body: literal=%d leaks=%d", lit, leaks)
	}
}

// The fix this round recommends: say what is true, and count each origin once.
//
// Skipped, so the current behaviour is not asserted as correct and a fix does
// not have to delete a passing test. Un-skip it with the change.
func TestR75Tier2OriginsAreNotReportedAsEngineDeclines(t *testing.T) {

	c, _ := origin.Parse("https://www.r75a.example")
	v, _ := origin.Parse("https://wt-a--r75w.ddev.site")
	m, _ := origin.NewMap([]origin.Site{{Name: "s", Canonical: c, Variant: v}})

	css := []byte(`.x{background:url("https://www.r75a.example/wp-content/uploads/bg.png")}`)
	leaks, tier2 := countLeaks(m.Forward(), response{
		body: css, status: 200, contentType: "text/css; charset=utf-8"})

	var buf bytes.Buffer
	WriteReport(&buf, []Result{{
		Path: "/a.css", ContentType: "text/css; charset=utf-8",
		Leaks: leaks, Tier2: tier2, Literal: literalOrigins(m, css),
		Equal: true,
	}})
	out := buf.String()
	if strings.Contains(out, "the engine declined") ||
		strings.Contains(out, "a literal scan found and the engine did not") {
		t.Errorf("a Tier 2 body is not an engine decline — the engine is never "+
			"run on one — yet the report says it is:\n%s", out)
	}
}

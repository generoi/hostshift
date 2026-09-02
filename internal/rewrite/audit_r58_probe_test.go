package rewrite

import (
	"io"
	"strings"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
)

// Both staleness checks, not just the first.
//
// rewriteAll builds one percent view and shares it across three passes, marking
// it stale when a splice moves bytes. Round 57 corrected the test from "did the
// length change" to "did any byte move" at two sites and the suite reached one:
// dropping the second — after the `%5Cu` pass — leaves the third pass reading a
// view whose offsets describe a buffer that no longer exists, and on a two-site
// map it does not merely corrupt, it panics with a slice bounds error.
//
// The producer is the one rounds 56 and 57 were both written for: a urlencoded
// POST body carrying Gutenberg's `--` beside a CSS-escaped URL.
func TestR58BothPercentViewStalenessChecks(t *testing.T) {
	m, err := origin.NewMatcher([]origin.Pair{
		{Canonical: origin.MustParse("https://wt-a--acme.ddev.site"),
			Variant: origin.MustParse("https://aaaaaaaaaa.example")},
		{Canonical: origin.MustParse("https://wt-b--acme.ddev.site"),
			Variant: origin.MustParse("https://bb.example")},
	})
	if err != nil {
		t.Fatal(err)
	}
	in := "a=https://wt-a%5Cu002d%5Cu002dacme.ddev.site/x" +
		"&b=https%5C3a%5C2f%5C2fwt-b--acme.ddev.site%5C2fy"
	// The assertion is that this returns at all: with one of the two checks
	// removed it panics inside the third pass.
	out := string(HostLeaksBack(m, []byte(in)))
	if strings.Contains(out, "wt-b--acme.ddev.site") &&
		strings.Contains(out, "wt-a--acme.ddev.site") {
		t.Errorf("neither spelling was read on the way back, so a variant "+
			"hostname goes into the shared database:\n  %s", out)
	}
}

// Every term of MaxMatchLen, one shape each.
//
// Round 57 added `2*maxBraceEsc` — "twice, one for the escaped port colon, one
// for the delimiter after the removable walk" — and its test needed only part of
// that, so three of the window's four terms could be deleted with the suite
// green. Each shape below puts one term's worth of bytes past the host and
// sweeps the read boundary across it; if the window does not cover the term, the
// streamed answer and the whole-buffer answer part company, which is the failure
// RewritePrefix calls "the same document rewrote differently depending on where
// the boundary happened to fall".
func TestR58EveryWindowTermIsCovered(t *testing.T) {
	m := r55Matcher(t)
	const chunk = 32 * 1024
	canon := "https://" + r55Canonical
	longColon := `\u{` + strings.Repeat("0", 60) + `3a}`
	longSlash := `\u{` + strings.Repeat("0", 60) + `2f}`
	for _, c := range []struct{ name, tail string }{
		// The escaped port colon and its digits: the first brace term plus the 16.
		{"escaped colon and port", longColon + "443/x"},
		// The removable walk: maxRemovedRun.
		{"removable run", strings.Repeat(`\t`, 30) + "/x"},
		// The walk *and* a brace-escaped delimiter after it: the second brace term.
		{"removable run then an escaped delimiter", strings.Repeat(`\t`, 30) + longSlash + "x"},
	} {
		t.Run(c.name, func(t *testing.T) {
			for gap := 4; gap <= 260; gap += 8 {
				pad := chunk - gap - len(canon)
				if pad < 0 {
					continue
				}
				body := strings.Repeat("z", pad) + canon + c.tail + strings.Repeat("z", 4096)
				streamed, err := io.ReadAll(NewSweep(strings.NewReader(body), m, nil,
					Options{Stats: NewStats(false)}))
				if err != nil {
					t.Fatal(err)
				}
				whole, _ := m.Rewrite([]byte(body), SurfaceStraggler, false)
				if strings.Contains(string(streamed), r55Variant) !=
					strings.Contains(string(whole), r55Variant) {
					t.Fatalf("host ends %d bytes before the boundary, window %d: "+
						"streamed rewrote=%v, whole buffer rewrote=%v", gap, m.MaxMatchLen(),
						strings.Contains(string(streamed), r55Variant),
						strings.Contains(string(whole), r55Variant))
				}
			}
		})
	}
}

// The CSS gate on all three of its compositions, not just the plain one.
//
// A `Location` is read by the URL parser and nothing else: no CSS tokenizer, no
// character-reference decoder, no form decoder. ada resolves each of these to
// the *base's* host — they are relative paths, not absolute URLs — so a header
// pass that decodes them matches a canonical that is not there and emits a
// redirect to a worktree host, from an engine whose whole contract is to splice
// only a host's own byte range.
//
// Round 58 gated all three compositions and the suite reached one, so the
// percent-over-CSS and reference-over-CSS arms could be reopened with everything
// green.
func TestR58AResponseHeaderDecodesNoCSSEscapeAnySpelling(t *testing.T) {
	m := r55Matcher(t)
	for _, in := range []string{
		`https\3a \2f \2f ` + r55Canonical + `\2f x`,
		`https%5C3a%5C2f%5C2f` + r55Canonical + `%5C2fx`,
		`https&#58;&#47;&#47;` + r55Canonical + `/x`,
	} {
		out, _ := m.Rewrite([]byte(in), SurfaceResponseHeader, false)
		got := string(HostLeaksCounted(m, out, true, NewStats(false), SurfaceResponseHeader, 0))
		if got != in {
			t.Errorf("a browser resolves this Location to the base's own host, so "+
				"nothing may change it:\n  in  %s\n  out %s", in, got)
		}
	}
	// And the same strings on a surface that *does* decode them still rewrite,
	// or the gate has simply turned the views off.
	for _, in := range []string{
		`a{background:url(https\3a \2f \2f ` + r55Canonical + `\2f x)}`,
	} {
		out, _ := m.Rewrite([]byte(in), SurfaceInlineStyle, false)
		got := string(HostLeaksCounted(m, out, false, NewStats(false), SurfaceInlineStyle, 0))
		if !strings.Contains(got, r55Variant) {
			t.Errorf("the CSS view stopped running where a CSS tokenizer does run:\n  %s", got)
		}
	}
}

// `<foreignObject>` is where SVG hands the parser back to HTML.
//
// Inside `<svg>` the parser is in foreign content and decodes character
// references in `<script>` and `<style>` — verified in Chrome, which is why the
// engine decodes them there. `<foreignObject>` is named for the fact that its
// children are *not* foreign: the parser returns to HTML rules, so an HTML
// `<script>` inside it is script data and decodes nothing. Treating it as
// foreign rewrote a JS string literal the browser reads verbatim, which is the
// mirror of the error this whole file guards against.
func TestR60ForeignObjectReturnsToHTMLRules(t *testing.T) {
	m := r55Matcher(t)
	// The control first: inside <svg> and outside <foreignObject>, a reference
	// really is decoded, so this must still be rewritten.
	ctl := `<svg><script>fetch("https:&#47;&#47;` + r55Canonical + `/x")</script></svg>`
	if out := rewriteHTML(t, m, ctl, NewStats(false)); !strings.Contains(out, r55Variant) {
		t.Fatalf("the foreign-content decode regressed, so the case below proves "+
			"nothing:\n  %s", out)
	}
	// And inside foreignObject it is not.
	in := `<svg><foreignObject><script>fetch("https:&#47;&#47;` + r55Canonical +
		`/x")</script></foreignObject></svg>`
	if out := rewriteHTML(t, m, in, NewStats(false)); out != in {
		t.Errorf("a script inside <foreignObject> is HTML script data and decodes "+
			"no references, so this is not a URL and nothing may change:\n  in  %s\n  out %s",
			in, out)
	}
	// ...and the depth is tracked, not just the presence: back outside it, the
	// decode is on again.
	after := `<svg><foreignObject><p>x</p></foreignObject>` +
		`<script>fetch("https:&#47;&#47;` + r55Canonical + `/x")</script></svg>`
	if out := rewriteHTML(t, m, after, NewStats(false)); !strings.Contains(out, r55Variant) {
		t.Errorf("the foreignObject depth was not unwound, so a real decode was "+
			"lost after it closed:\n  %s", out)
	}
}

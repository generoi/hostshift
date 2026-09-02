package rewrite

import (
	"strings"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
)

// Round 71. Three of these are RED on f05d8e1 by design: they are the finding.
//
// Round 70 closed "a bare origin at the end of a line" for a *raw* newline in
// prose, and scoped the fix with this reasoning: "a raw newline cannot appear
// inside a JavaScript or JSON string literal — both forbid it". Both halves of
// that sentence are load-bearing and both are incomplete.
//
//   - JSON does not carry a newline raw; it carries it as `\n`. json.go scans
//     the *still-escaped* bytes of every string value with Matcher.Rewrite, i.e.
//     value semantics, and removedEscLen's escape branch (matcher.go:823) never
//     looks at the new `value` argument at all. So the one spelling the block
//     editor actually puts on the wire still joins the lines.
//   - A JavaScript *template* literal does allow a raw newline, so the new prose
//     rule fires inside an inline script, where the buffer really is parsed as a
//     URL. Tests C and D below.

func r71Rev(t *testing.T) *origin.Matcher {
	t.Helper()
	m, err := origin.NewMatcher([]origin.Pair{{
		Canonical: origin.MustParse("https://wt-a--example.ddev.site"),
		Variant:   origin.MustParse("https://www.example.fi"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// A bare origin at the end of a line inside a JSON string, which is the block
// editor's own save format.
//
// Measured on a real WordPress in round 71, through the proxy, read back out of
// MySQL rather than off a page: publishing a post from the block editor whose
// code block reads
//
//	curl https://wt-a--r71w-a.ddev.site
//	next line here
//
// stored that variant hostname in wp_posts.post_content and in the revision row
// beside it. §4.3, and there is no undo. In the same request a third URL in the
// same field, one with a path, came home to the canonical — so the proxy did
// read the body; it declined this origin specifically.
//
// The outward half is the same bytes: GET /wp-json/wp/v2/posts/<id> served
// content.rendered holding a dereferenceable production origin, which json.go's
// own header calls "a live production link" because Gutenberg injects it into
// the page as HTML.
func TestR71AJSONEscapedNewlineJoinsALineEndOrigin(t *testing.T) {
	rev := r71Rev(t)
	// The request direction: a save posted by the block editor.
	body := `{"content":"<!-- wp:code -->\n<pre class=\"wp-block-code\"><code>curl ` +
		`https://wt-a--example.ddev.site\nnext line here</code></pre>"}`
	out := RewriteJSON([]byte(body), rev, NewStats(false), quiet(), false)
	if strings.Contains(string(out), "wt-a--example.ddev.site") {
		t.Errorf("a variant hostname reached the shared database through the block "+
			"editor's own save path — §4.3, no undo\n  in:  %s\n  out: %s", body, out)
	}

	// The response direction: the same post read back through the REST API.
	fwd := obfMatcher(t)
	resp := `{"content":{"rendered":"<pre>curl https://www.example.fi\nnext line here</pre>"}}`
	got := RewriteJSON([]byte(resp), fwd, NewStats(false), quiet(), false)
	if strings.Contains(string(got), "www.example.fi") {
		t.Errorf("a production origin reached the browser in content.rendered — test 28\n"+
			"  in:  %s\n  out: %s", resp, got)
	}

	// The control that says the body was processed at all: the same string, one
	// URL over, with a path.
	ctl := `{"content":"see https://wt-a--example.ddev.site/deal ok"}`
	if o := RewriteJSON([]byte(ctl), rev, NewStats(false), quiet(), false); strings.Contains(string(o), "ddev.site") {
		t.Fatalf("positive control failed: the JSON arm rewrote nothing at all — %s", o)
	}
}

// The same shape with a literal tab, which round 70 left joined everywhere.
//
// Measured outward on a real WordPress: a page whose post_content held
// `https://www.r71a.example<TAB>after tab.` served that production origin to the
// browser both inside a `<pre>` and inside an ordinary `<p>` — the paragraph
// case does not need `<pre>` at all, because wpautop turns a newline into `<br />`
// but leaves a tab alone. Read out of the rendered page with innerText, and
// `hostshift diff` printed GREEN on it.
//
// The scope is narrower than "make a tab a boundary". An inline `<script>` is
// scanned as prose too (html.go:674 picks RewriteText for every surface except
// SurfaceHTMLAttr), and `fetch("https://www.example.fi\tx")` really does resolve
// to www.example.fix — §5.5, and Node's URL agrees. So a tab may only become a
// boundary on the surfaces where nothing will parse the buffer as a URL:
// SurfaceText, SurfaceComment, SurfaceRequestBody. That is why this test names
// its surfaces instead of asking the matcher in the abstract.
func TestR71BALiteralTabIsABoundaryInProseButNotInAScript(t *testing.T) {
	fwd := obfMatcher(t)
	for _, surface := range []string{SurfaceText, SurfaceComment} {
		in := "Tab: https://www.example.fi\tafter tab."
		out, _ := fwd.RewriteText([]byte(in), surface, false)
		if strings.Contains(string(out), "www.example.fi") {
			t.Errorf("[%s] a production origin the reader sees, and can copy, reached "+
				"the browser — test 28\n  in:  %q\n  out: %q", surface, in, string(out))
		}
	}

	rev := r71Rev(t)
	in := "Tab: https://wt-a--example.ddev.site\tafter tab."
	out, _ := rev.RewriteText([]byte(in), SurfaceRequestBody, false)
	if strings.Contains(string(out), "wt-a--example.ddev.site") {
		t.Errorf("a variant hostname reached the shared database — §4.3\n  in:  %q\n  out: %q",
			in, string(out))
	}

	// And the half that must not move: an inline script is prose by surface, but
	// a raw tab inside a JS string is removed before the fetch.
	js := `<script>fetch("https://www.example.fi` + "\t" + `x")</script>`
	if got := rewriteHTML(t, fwd, js, NewStats(false)); got != js {
		t.Errorf("§5.5: a browser resolves this to www.example.fix, so nothing may "+
			"change\n  in:  %s\n  out: %s", js, got)
	}
}

// Round 70 recorded `EscapeContinuesHost` passing value semantics as an
// equivalent mutant — "I could not construct a case that observes it through any
// shipped surface". It is observable, and this is the case.
//
// The locator, not the byte matcher: give it a separator the automaton cannot
// match (`https:/\`, `https:///`, `http:h`, `%2e` for the dots) so its decision
// is not masked by a rewrite that already happened. Put the host at the end of a
// line of a JavaScript *template* literal — which, unlike a string literal,
// really may hold a raw newline — with an escaped newline before it, so the
// removed-run walk has to cross an escape and then a literal control.
//
// Node's URL resolves every input below to www.example.fix, never to production,
// so declining is correct and `value=true` is the answer that declines. With
// `false` in matcher.go:896 the whole suite still passes and these four rewrite.
func TestR71CTheLocatorsValueFlagIsObservable(t *testing.T) {
	m := obfMatcher(t)
	for _, in := range []string{
		"<script>fetch(`https:/" + `\` + `www.example.fi\n` + "\nx`)</script>",
		"<script>fetch(`https:///www.example.fi" + `\n` + "\nx`)</script>",
		"<script>fetch(`http:www.example.fi" + `\n` + "\nx`)</script>",
		"<script>fetch(`https:/" + `\` + `www%2eexample%2efi\n` + "\nx`)</script>",
	} {
		if got := rewriteHTML(t, m, in, NewStats(false)); got != in {
			t.Errorf("a browser resolves this to www.example.fix, so rewriting it "+
				"changes a value that never pointed at production, and hands the page "+
				"a variant hostname with a letter glued to it that the request "+
				"direction cannot read back\n  in:  %s\n  out: %s", in, got)
		}
	}
}

// The other side of the same premise, and the one thing round 70 moved that it
// should not have: an inline <script> is scanned as prose, and a JavaScript
// *template* literal may hold a raw newline.
//
// So the new rule — a literal newline is a boundary when the buffer is not a
// value — fires inside a script, where the buffer really will be parsed as a
// URL. Node resolves both of these to www.example.fix; neither ever pointed at
// production, and after the rewrite the page holds a variant hostname with a
// letter glued to it that the request direction cannot read back. That is
// R54/§5.5's harm exactly, introduced in the commit whose justification is
// "a raw newline cannot appear inside a JavaScript or JSON string literal".
//
// The remedy this and TestR71B both point at is the same: the distinction that
// decides whether a removed control joins is not value-versus-prose but
// "will anything parse this buffer as a URL", which is a property of the
// surface — SurfaceInlineScript and SurfaceRawText yes, SurfaceText,
// SurfaceComment and SurfaceRequestBody no.
func TestR71DARawNewlineInATemplateLiteralIsNotABoundary(t *testing.T) {
	m := obfMatcher(t)
	for _, in := range []string{
		"<script>fetch(`https://www.example.fi" + "\nx`)</script>",
		"<script>fetch(`https://www.example.fi" + "\n\nx`)</script>",
	} {
		if got := rewriteHTML(t, m, in, NewStats(false)); got != in {
			t.Errorf("a browser resolves this to www.example.fix, so nothing may "+
				"change\n  in:  %s\n  out: %s", in, got)
		}
	}
}

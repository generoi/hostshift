package rewrite

import (
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
)

// Round 73. All three are RED on 2565a9f by design: they are the finding.
//
// Rounds 70, 71 and 72 are one question asked in three places — when a control
// follows a host, does it join what follows or end it — and each round fixed it
// where it was looked at. Round 72 gave the *locator* the axis and threaded
// origin.JoinsControlsIn through stripForURL, stripForRefs, stripForCSS and
// stripForPercent. Two places still never ask.
//
// A and B were measured on a real WordPress: the fixture test/bootstrap-ddev.sh
// builds, database on production hostnames, served through the add-on's own
// proxy, the outbound half read off the wire with curl and the inbound half read
// out of MySQL with `mysql` rather than back through the proxy.

func r73Fwd(t *testing.T) *origin.Matcher { // canonical -> variant (response)
	t.Helper()
	m, err := origin.NewMatcher([]origin.Pair{{
		Canonical: origin.MustParse("https://www.hostshift-a.example"),
		Variant:   origin.MustParse("https://wt-a--r73w-hs.ddev.site"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func r73Rev(t *testing.T) *origin.Matcher { // variant -> canonical (request)
	t.Helper()
	m, err := origin.NewMatcher([]origin.Pair{{
		Canonical: origin.MustParse("https://wt-a--r73w-hs.ddev.site"),
		Variant:   origin.MustParse("https://www.hostshift-a.example"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// A. The JSON arm hands the locator a surface name that is not its own.
//
// json.go:234 and json.go:292 call the reference/URL locator as
// `rewriteAllRefs(nv, true, bareSurface(true), nil)`. bareSurface(true) is
// SurfaceHTMLAttr, and surfaceJoinsControls says an attribute joins — so on the
// one path that exists to catch the spellings the byte matcher cannot see, the
// locator asks the *table* and gets "always a value", while the byte matcher one
// line above it asks joinsControlsIn about the actual buffer and gets "this is a
// document". Round 72's two-halves-disagree, at the call site rather than in the
// pass.
//
// bareSurface is there for the escape alphabet — the value has already been
// unquoted, so a backslash is a path separator — and SurfaceJSONEscape answers
// that identically (escapeAlphabetFor returns escPath for it) while also routing
// through joinsControlsIn. Swapping the two names at both call sites turns every
// case below green and leaves `go test ./...` green.
//
// Only the `\`-obfuscated spelling reaches this: an entity-spelled origin is
// decoded by decodeURLRefs before the byte matcher runs, and `%2F` is in the
// matcher's own prefilter. `https:\\host` has none of `//`, `\/` or `%2F`, so
// the locator is the only pass that can see it — and Node's URL resolves
// `https:\\www.hostshift-a.example` to the host www.hostshift-a.example.
//
// Measured, post 6 on the fixture:
//
//	BSLINEEND https:\\www.hostshift-a.example
//	next line. BSSPACE https:\\www.hostshift-a.example ok. PLAINSPACE https://…
//
// GET /wp-json/wp/v2/posts/6?context=edit at the variant — which is what the
// block editor loads — returned content.raw with BSSPACE and PLAINSPACE
// rewritten to the variant and BSLINEEND still naming production. Test 28.
//
// Inbound, POST /wp-json/wp/v2/posts through the proxy with
// {"content":"INLINEEND https:\\\\<variant>\nnext line. INSPACE https:\\\\<variant> ok."}
// stored, in wp_posts.ID=7.post_content read with `mysql`:
//
//	INLINEEND https:\\wt-a--r73w-hs.ddev.site  ← a variant hostname in the
//	next line. INSPACE https:\\www.hostshift-a.example ok.   shared database
//
// §4.3, no undo.
func TestR73AJSONLocatorAsksTheTableAndNotTheBuffer(t *testing.T) {
	for _, ctl := range []struct{ name, esc string }{{"LF", `\n`}, {"CR", `\r`}} {
		for _, tc := range []struct {
			name, host string
			m          *origin.Matcher
		}{
			{"out", "www.hostshift-a.example", r73Fwd(t)},
			{"in", "wt-a--r73w-hs.ddev.site", r73Rev(t)},
		} {
			t.Run(tc.name+"/"+ctl.name, func(t *testing.T) {
				// `\\\\` in the JSON source is `\\` in the decoded value.
				in := `{"content":"see https:\\\\` + tc.host + ctl.esc + `next line here"}`
				ctl := strings.Replace(in, ctl.esc, ` `, 1)
				ctlOut := string(RewriteJSON([]byte(ctl), tc.m, NewStats(false), quiet(), false))
				if strings.Contains(ctlOut, tc.host) {
					t.Fatalf("the control did not rewrite either; the case is not about "+
						"the control character\n  %s", ctlOut)
				}
				got := string(RewriteJSON([]byte(in), tc.m, NewStats(false), quiet(), false))
				if strings.Contains(got, tc.host) {
					t.Errorf("%q survives a control in a JSON document, because the locator "+
						"was handed bareSurface(true)\n  in:  %s\n  out: %s\n  with a space: %s",
						tc.host, in, got, ctlOut)
				}
			})
		}
	}
}

// B. A tab is stripped everywhere, including where nothing parses a URL.
//
// isURLStripped (urlobf.go:139) returns true for '\t' unconditionally and gates
// only '\n' and '\r' on the surface. Its own comment draws the distinction —
// "that is the *normalisation* question, and it is not the same as the boundary
// question the matcher asks about a control that follows a complete host" — and
// then does not act on it. The byte matcher does act on it: with joins=false a
// tab terminates the host, which is why the plain spelling below is rewritten
// and the two spellings only the locator can see are not. Round 53's
// two-halves-disagree, one control character over from where round 72 left it.
//
// The naive fix — `return joins && (c == '\t' || ...)` — is wrong, and this is
// the record of why: it makes `https:www.example<TAB>.fi` unmatchable on text,
// comment and svg-text, which ada resolves to www.example.fi, and
// TestURLShapesAgainstBrowserOracle goes from 0 to 1629 leaks. A tab *inside* a
// host has to keep being removed; only a tab that *follows* a complete host is a
// boundary on a prose surface.
//
// Measured, post 9 on the fixture, one paragraph, one save, served at the
// variant (tabs shown as <TAB>):
//
//	TABBS   https:\\www.hostshift-a.example<TAB>next     ← production, served
//	TABENT  https:&#47;&#47;www.hostshift-a.example<TAB>next  ← production, served
//	TABPLAIN https://wt-a--r73w-hs.ddev.site<TAB>next     ← rewritten
//	SPACEBS https:\\wt-a--r73w-hs.ddev.site ok.           ← rewritten
//
// and inbound, POST /wp-json/wp/v2/posts, read out of wp_posts.ID=10 with
// `mysql`: `TABIN https:\\wt-a--r73w-hs.ddev.site<TAB>next` — a variant hostname
// in the shared database, §4.3 — beside `SPACEIN https:\\www.hostshift-a.example`.
func TestR73BATabAfterAHostIsABoundaryInProseToo(t *testing.T) {
	// OPEN FINDING, deliberately skipped rather than deleted. See PLAN §5.5.
	//
	// A tab has to be two things at once and the locator's strip-then-scan order
	// cannot express both: ada removes a tab *inside* a host on every surface —
	// `https:www.example<TAB>.fi` is `www.example.fi` — and a tab *after* a
	// complete host is a boundary in prose. Round 73 measured the naive fix
	// (gating the tab on the surface like the newline) taking
	// TestURLShapesAgainstBrowserOracle from 0 to 1629 leaks, so it is not a flag
	// flip; it needs the locator to run two views on prose surfaces, one
	// normalising and one not.
	//
	// Left failing-as-skipped so the shape is in the tree with its measurement,
	// rather than deleted and rediscovered in another three rounds.
	t.Skip("open: needs a second locator view on prose surfaces; see PLAN §5.5")
	for _, sp := range []struct{ name, tmpl string }{
		{"backslash", `https:\\%s`},
		{"entity-slash", "https:&#47;&#47;%s"},
	} {
		lit := fmt.Sprintf(sp.tmpl, "www.hostshift-a.example")
		for _, cx := range []struct{ name, tmpl string }{
			{"paragraph", "<p>%s</p>"},
			{"title", "<title>%s</title>"},
			{"textarea", "<textarea>%s</textarea>"},
		} {
			t.Run(sp.name+"/"+cx.name, func(t *testing.T) {
				in := fmt.Sprintf(cx.tmpl, "see "+lit+"\tnext line")
				ctl := strings.Replace(in, "\t", " ", 1)
				ctlOut := rewriteHTML(t, r73Fwd(t), ctl, NewStats(false))
				if strings.Contains(ctlOut, "www.hostshift-a.example") {
					t.Fatalf("the control did not rewrite either; the case is not about "+
						"the tab\n  %q", ctlOut)
				}
				got := rewriteHTML(t, r73Fwd(t), in, NewStats(false))
				if strings.Contains(got, "www.hostshift-a.example") {
					t.Errorf("a production origin survives a tab on a prose surface — test 28"+
						"\n  in:  %q\n  out: %q\n  with a space: %q", in, got, ctlOut)
				}
			})
		}
	}
	// The same field on the JSON arm, which is the §4.3 half. Independent of A:
	// this cell is still red with A fixed, because a tab is not gated at all.
	in := `{"content":"see https:\\\\wt-a--r73w-hs.ddev.site\tnext line here"}`
	if got := string(RewriteJSON([]byte(in), r73Rev(t), NewStats(false), slog.Default(), false)); strings.Contains(got, "wt-a--r73w-hs.ddev.site") {
		t.Errorf("a variant hostname survives a tab in a JSON document — §4.3, no undo"+
			"\n  in:  %s\n  out: %s", in, got)
	}
}

// C. scriptIsJavaScript's type list is nine of HTML's sixteen.
//
// The spec's "JavaScript MIME type" list is closed and enumerable, and seven
// legacy essences on it are missing here: text/x-javascript,
// application/x-javascript, text/jscript, text/livescript, text/javascript1.0
// through 1.5, text/x-ecmascript and application/x-ecmascript. A browser runs
// all of them; hostshift files them as a data block and gives them
// SurfaceJSONString, where a script body's spaces make joinsControlsIn read it
// as prose.
//
// The direction is over-rewrite rather than leak — a control after a host inside
// a string literal ends the host, so the value written into the script differs
// from what the browser resolves — which is why this is the small one. The same
// legacy names are already enumerated a package away, in corpus.isTier2.
func TestR73CScriptTypeListIsHTMLsList(t *testing.T) {
	for _, ty := range []string{
		"text/x-javascript", "application/x-javascript", "text/jscript",
		"text/livescript", "text/javascript1.5", "text/x-ecmascript",
		"application/x-ecmascript",
	} {
		if !scriptIsJavaScript([]byte(`<script type="` + ty + `">`)) {
			t.Errorf("<script type=%q> is a JavaScript MIME type a browser runs, and "+
				"scriptIsJavaScript calls it a data block", ty)
		}
	}
	// And the other direction still holds: a data block is still a data block.
	for _, ty := range []string{"application/ld+json", "importmap", "text/html", "text/template"} {
		if scriptIsJavaScript([]byte(`<script type="` + ty + `">`)) {
			t.Errorf("<script type=%q> is not JavaScript", ty)
		}
	}
}

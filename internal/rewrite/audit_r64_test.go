package rewrite

import (
	"fmt"
	"net/url"
	"strings"
	"testing"

	"golang.org/x/net/html"

	"github.com/generoi/hostshift/internal/origin"
)

// `<style` is a prefix, not a tag name.
//
// writeRawTextAroundStyles finds the `<style>` element inside an `<svg><title>`
// with bytes.Index(low, "<style") — a substring search with no tag-name
// terminator after it. Every element whose name *begins with* "style" matches:
// `<styled-note>`, `<style-guide>`, `<stylexyz>`. The span from that tag to the
// next `</style` — which `</styled-note>` also matches — is then handed the CSS
// view, and the CSS view is the one surface in this element that does *not*
// decode character references (rewriteValueInner's gate admits SurfaceRawText
// under rcdataElement, and SurfaceInlineStyle only in foreign content, which an
// integration point has left).
//
// So the decoy withholds a decode the parser performs. html.Parse — the oracle
// for parser-state questions — builds a real `<img>` inside the svg title, with
// its `src` decoded to https://www.example.fi/x, and the browser issues that
// request against live production carrying the developer's session. hostshift
// serves the bytes through untouched: test 28.
//
// The same bytes without the decoy *are* rewritten, which is what makes this a
// scope error in round 63's fix rather than a gap it left open.
//
// The fix is a tag-name terminator: `<style` counts only when the next byte is
// one of `>`, `/`, or ASCII whitespace.
func TestR64StylePrefixIsNotAStyleElement(t *testing.T) {
	m := obfMatcher(t)
	const ref = `https:&#47;&#47;www.example.fi/x`
	for _, c := range []struct{ name, doc string }{
		{"img src behind a <styled-note>",
			`<svg><title><styled-note><img src="` + ref + `"></styled-note></title></svg>`},
		{"img src behind an unclosed <style-guide>",
			`<svg><title><style-guide><img src="` + ref + `"></title></svg>`},
		{"anchor behind a <styled-note>",
			`<svg><title><styled-note><a href="` + ref + `">y</a></styled-note></title></svg>`},
		{"prose behind a <stylexyz>",
			`<svg><title>see <stylexyz>` + ref + `</stylexyz> ok</title></svg>`},
	} {
		t.Run(c.name, func(t *testing.T) {
			// Not parserDecodesReferences: it concatenates *text* nodes, and
			// three of these fixtures carry the origin in an attribute, where
			// the parser decodes references unconditionally. The guard has to
			// look where the payload is or it passes on a document that tests
			// nothing — which is how it read before, reporting these as clean.
			if !parserResolvesCanonical(t, c.doc) {
				t.Fatalf("oracle: the parser does not resolve the canonical origin "+
					"here, so this fixture no longer tests the claim:\n  %s", c.doc)
			}
			out := rewriteHTML(t, m, c.doc, nil)
			if !strings.Contains(out, "wt-a--example.ddev.site") {
				t.Errorf("an element whose name merely *starts with* \"style\" was read as\n"+
					"  the <style> element, so its content got the CSS view — the one view\n"+
					"  here that does not decode character references. The parser decodes\n"+
					"  them and resolves https://www.example.fi/x, this map's canonical.\n"+
					"  in:  %s\n  out: %s", c.doc, out)
			}
		})
	}
}

// A nested `<svg>` inside `<svg><title>` is foreign content again, and its
// `<style>` is an SVG stylesheet whose references the parser decodes.
//
// writeRawTextAroundStyles hands every `<style>` span inside an integration
// point the CSS view, which withholds the reference decode. That is right for an
// *HTML* `<style>` — RAWTEXT, where the parser decodes nothing — and wrong the
// moment the document has re-entered foreign content inside the title, because
// there the element is an svg:style and 13.2.6.5 decodes references in it.
//
// html.Parse builds exactly that: `<svg ns=svg> <title ns=svg> <svg ns=svg>
// <style ns=svg>` with the reference already decoded in its text. The `@import`
// is a live production fetch.
//
// hostshift cannot see the nesting at all: x/net/html's tokenizer switches to
// RCDATA on `<title>` regardless of namespace, so the whole subtree arrives as
// one text token, while x/net/html's *parser* suppresses that switch in foreign
// content. Round 63 modelled the inside of that token with a substring search;
// the substring search cannot answer this question.
func TestR64NestedSVGInTitleIsAStylesheet(t *testing.T) {
	m := obfMatcher(t)
	const ref = `https:&#47;&#47;www.example.fi/x`
	doc := `<svg><title><svg><style>@import url("` + ref + `")</style>`
	if !parserDecodesReferences(t, doc) {
		t.Fatalf("oracle: the parser does not decode here:\n  %s", doc)
	}
	// And it is a stylesheet, not prose: the parser puts the <style> in the SVG
	// namespace, which is what makes the @import a fetch.
	if ns := r64StyleNamespace(t, doc); ns != "svg" {
		t.Fatalf("oracle: expected an svg:style, got namespace %q", ns)
	}
	out := rewriteHTML(t, m, doc, nil)
	if !strings.Contains(out, "wt-a--example.ddev.site") {
		t.Errorf("an svg:style inside <svg><title> got the CSS view, which does not\n"+
			"  decode character references — but the parser is back in foreign content\n"+
			"  here and does. The @import resolves to https://www.example.fi/x and the\n"+
			"  browser fetches it.\n  in:  %s\n  out: %s", doc, out)
	}
}

func r64StyleNamespace(t *testing.T, doc string) string {
	t.Helper()
	n, err := html.Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatal(err)
	}
	ns := ""
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "style" && ns == "" {
			ns = n.Namespace
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return ns
}

// The breakout list is consulted for `<br>` and not for `<br/>`.
//
// html.go gates the whole foreign-content bookkeeping on
// `tt == html.StartTagToken`, and x/net/html returns SelfClosingTagToken for
// `<br/>`. 13.2.6.5's breakout rule is a name test on a start tag; the
// self-closing flag is not part of it. So the spelling that is *most* common for
// the void elements on that list — `<br/>`, `<hr/>`, `<img/>`, `<meta/>`,
// `<embed/>` — leaves the model foreign for the rest of the document, which is
// exactly the state round 63 added breakoutNames to leave.
//
// Over-decode, so nothing leaks; what it costs is bytes. A reference-encoded
// string in a later `<script>` that no browser resolves has its host rewritten
// anyway. Measured: 88 of the 91 mismatches against html.Parse over the whole
// 44-name list in both vocabularies are this one gate.
func TestR64BreakoutIgnoresSelfClosingSpelling(t *testing.T) {
	m := obfMatcher(t)
	const ref = `https:&#47;&#47;www.example.fi/x`
	for _, n := range []string{"br", "hr", "img", "meta", "embed", "p", "div"} {
		t.Run(n, func(t *testing.T) {
			doc := `<svg><` + n + `/><script>fetch("` + ref + `")</script>`
			if parserDecodesReferences(t, doc) {
				t.Fatalf("oracle: the parser *does* decode here, so this fixture no "+
					"longer tests the claim:\n  %s", doc)
			}
			out := rewriteHTML(t, m, doc, nil)
			if strings.Contains(out, "wt-a--example.ddev.site") {
				t.Errorf("`<%s/>` ends foreign content just as `<%s>` does, so the parser\n"+
					"  reads this <script> under HTML rules and decodes nothing in it.\n"+
					"  hostshift stayed foreign and rewrote a string no browser resolves.\n"+
					"  in:  %s\n  out: %s", n, n, doc, out)
			}
		})
	}
}

// peelFormField's round-trip guard is written against Go's encoder, not the
// browser's.
//
// The guard is `url.QueryEscape(dec) != val`, and QueryEscape is not the WHATWG
// application/x-www-form-urlencoded serializer every browser uses. They disagree
// on exactly two bytes, and each disagreement is a decline:
//
//   - `*` — the browser leaves it raw (it is in the urlencoded safe set),
//     QueryEscape writes `%2A`.
//   - `~` — the browser writes `%7E`, QueryEscape leaves it raw.
//
// Verified against Node: `new URLSearchParams([["k","*~"]]).toString()` is
// `k=*%7E`.
//
// So a field value carrying either character declines the peel *whole*, and the
// double-encoded variant hostname that round 63 added the peel to read goes
// upstream and into the shared database. `*` and `~` are not exotic in the
// payloads this fires on: they are CSS combinators, and `custom_css` is the
// option round 63's own comment names.
//
// The guard should compare against the serializer the browser actually used, or
// accept a value whose only difference from the re-encoding is one of those two
// spellings.
func TestR64PeelGuardMatchesTheBrowsersEncoder(t *testing.T) {
	mp, err := origin.NewMap([]origin.Site{{Name: "s",
		Canonical: origin.MustParse("https://www.example.fi"),
		Variant:   origin.MustParse("https://wt-a--example.ddev.site")}})
	if err != nil {
		t.Fatal(err)
	}
	rev := mp.Reverse()
	req := func(b []byte) []byte {
		return RepairSerializedFields(b, func(x []byte) []byte {
			nv, _ := rev.Rewrite(x, SurfaceRequestBody, false)
			return HostLeaksBack(rev, nv)
		})
	}
	const dbl = "https%253A%252F%252Fwt-a--example.ddev.site%252Fx"
	for _, c := range []struct{ name, body string }{
		{"asterisk", "opt=" + dbl + "+*"},
		{"tilde", "opt=" + dbl + "+%7E"},
		{"css combinators", "custom_css=a+*+b+%7E+c+" + dbl},
	} {
		t.Run(c.name, func(t *testing.T) {
			// The body really is what a browser sends: re-serialising the decoded
			// pairs the WHATWG way reproduces it byte for byte.
			if got := r64WhatwgEncode(c.body); got != c.body {
				t.Fatalf("fixture is not a browser-shaped body:\n  want %s\n  got  %s", c.body, got)
			}
			out := string(req([]byte(c.body)))
			if strings.Contains(out, "ddev.site") {
				t.Errorf("the peel declined this field because Go's QueryEscape spells\n"+
					"  %q differently from the browser that sent it, so the variant\n"+
					"  hostname went upstream into the shared database — PLAN §4.3.\n"+
					"  in:  %s\n  out: %s", c.name, c.body, out)
			}
		})
	}
}

// r64WhatwgEncode re-serialises a urlencoded body the way the WHATWG
// application/x-www-form-urlencoded serializer does: the safe set is ASCII
// alphanumeric plus `*`, `-`, `.` and `_`, and a space is `+`.
func r64WhatwgEncode(body string) string {
	var out []string
	for _, field := range strings.Split(body, "&") {
		k, v, _ := strings.Cut(field, "=")
		dk, err1 := url.QueryUnescape(k)
		dv, err2 := url.QueryUnescape(v)
		if err1 != nil || err2 != nil {
			return ""
		}
		out = append(out, r64WhatwgEscape(dk)+"="+r64WhatwgEscape(dv))
	}
	return strings.Join(out, "&")
}

func r64WhatwgEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ' ':
			b.WriteByte('+')
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '*', c == '-', c == '.', c == '_':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// parserResolvesCanonical is parserDecodesReferences over text *and* attribute
// values.
//
// Attributes are where a decoded origin is dereferenceable without any script at
// all — an `<img src>` the browser fetches with the developer's session on it —
// and the text-only helper cannot see one, so it silently passes a fixture that
// proves nothing. Round 63 was bitten by the same helper through its hardcoded
// path; this is the other half of it.
func parserResolvesCanonical(t *testing.T, doc string) bool {
	t.Helper()
	n, err := html.Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode && strings.Contains(n.Data, "https://www.example.fi/x") {
			found = true
		}
		for _, a := range n.Attr {
			if strings.Contains(a.Val, "https://www.example.fi/x") {
				found = true
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return found
}

// A field can carry one origin the spellings reach and a second only the peel can.
//
// `?u=https%3A%2F%2Fh%2Fx` inside a URL that is itself form-encoded is an
// ordinary share link, redirect target or `?ref=`. Round 63 offered the peel only
// to a field that came back byte-identical, so rewriting the outer origin was
// exactly what withheld the inner one — and the variant hostname went upstream
// into the shared database. Measured through a real wp-admin save.
//
// The withholding rule exists for round 44's `font-family:"Inter"`, where a
// *repaired serialized length* must not be re-walked. That is the hazard, and it
// is narrower than "anything changed".
func TestR64BothSpellingsInOneFieldComeHome(t *testing.T) {
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
	esc := url.QueryEscape
	inner := "https://wt-a--example.ddev.site/target"
	canonInner := "https://www.example.fi/target"
	for _, c := range []struct{ name, in, want string }{
		{"an encoded URL inside an encoded URL",
			"link=" + esc("https://wt-a--example.ddev.site/go?u="+esc(inner)+"&n=1"),
			"link=" + esc("https://www.example.fi/go?u="+esc(canonInner)+"&n=1")},
		{"the deep one alone still works",
			"v=" + esc(esc(inner)), "v=" + esc(esc(canonInner))},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := string(RepairSerializedFields([]byte(c.in), rw)); got != c.want {
				t.Errorf("an origin the outer spelling reached withheld the peel from "+
					"the inner one, so a variant hostname reached the shared database\n"+
					"  in:   %s\n  got:  %s\n  want: %s", c.in, got, c.want)
			}
		})
	}
}

// A decoy before a real stylesheet must not swallow it.
//
// The gate that decides whether a raw-text token holds a `<style>` uses a
// tag-name terminator, so a document with only `<styled-note>` never reaches the
// span walk at all — which means a prefix search *inside* the walk goes
// unnoticed until a document holds both. Then the walk opens its "stylesheet" at
// the decoy and the real one is inside that span, so the CSS view covers text
// and the reference decode is withheld from where it was needed.
func TestR64ADecoyBeforeARealStylesheet(t *testing.T) {
	m := obfMatcher(t)
	const ref = `https:&#47;&#47;www.example.fi/x`
	const esc = `https\3a \2f \2f www.example.fi/x`
	for _, c := range []struct{ name, doc, want string }{
		{"decoy, then a stylesheet with a CSS-escaped origin",
			`<svg><title><styled-note>n</styled-note><style>@import url(` + esc + `)</style></title></svg>`,
			`<svg><title><styled-note>n</styled-note><style>@import url(https\3a \2f \2f wt-a--example.ddev.site/x)</style></title></svg>`},
		{"decoy holding a reference, then a stylesheet",
			`<svg><title><styled-note>` + ref + `</styled-note><style>a{b:c}</style></title></svg>`,
			`<svg><title><styled-note>https:&#47;&#47;wt-a--example.ddev.site/x</styled-note><style>a{b:c}</style></title></svg>`},
		// The end of the span has to be a real `</style>` too: a `</style-guide>`
		// in the middle ends the stylesheet one tag early, and the CSS after it
		// then gets the raw-text view, where a `\3a \2f \2f` run is not syntax
		// and goes out naming production.
		{"a closing decoy does not end the stylesheet",
			`<svg><title><style>a{b:c}</style-guide>@import url(` + esc + `)</style></title></svg>`,
			`<svg><title><style>a{b:c}</style-guide>@import url(https\3a \2f \2f wt-a--example.ddev.site/x)</style></title></svg>`},
	} {
		t.Run(c.name, func(t *testing.T) {
			if out := rewriteHTML(t, m, c.doc, nil); out != c.want {
				t.Errorf("the span walk opened its stylesheet at a decoy\n"+
					"  in:   %s\n  out:  %s\n  want: %s", c.doc, out, c.want)
			}
		})
	}
}

// The negative twin: a `<style>` in an svg `<title>` with no `<svg>` re-entered
// is an *HTML* style, and the parser decodes nothing in it.
//
// `html.Parse` puts it in the HTML namespace and leaves `&#47;` intact, so an
// `@import` spelled with references resolves to nothing and is not a URL the
// browser fetches. Rewriting it would change bytes that name no origin — and
// marking every stylesheet span in a foreign title as foreign, rather than only
// one with an `<svg>` still open before it, is exactly that mistake.
func TestR64AStyleInAnSVGTitleIsAnHTMLStyle(t *testing.T) {
	m := obfMatcher(t)
	const ref = `https:&#47;&#47;www.example.fi/x`
	for _, c := range []struct{ name, doc string }{
		{"no nesting", `<svg><title><style>@import url("` + ref + `")</style></title></svg>`},
		{"a balanced <svg> before it", `<svg><title><svg></svg><style>@import url("` + ref + `")</style></title></svg>`},
	} {
		t.Run(c.name, func(t *testing.T) {
			if parserResolvesCanonical(t, c.doc) {
				t.Fatalf("oracle: the parser resolves the canonical origin here after "+
					"all, so this fixture no longer tests the claim:\n  %s", c.doc)
			}
			if out := rewriteHTML(t, m, c.doc, nil); out != c.doc {
				t.Errorf("this stylesheet is an HTML one and its references are not "+
					"decoded, so these bytes name no origin\n  in:  %s\n  out: %s",
					c.doc, out)
			}
		})
	}
}

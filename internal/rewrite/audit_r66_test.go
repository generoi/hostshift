package rewrite

import (
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// Round 66: the intra-tag span scanner against the tokenizer that frames it.
//
// attrspan.go reimplements the HTML tag-open / attribute-name / before-value /
// value states, and its header records a validation "against the tokenizer's own
// output over the corpus: 19,953 start tags, 37,280 attributes, 9 divergences".
// That validation is real and its alphabet is *real pages*, which contain no
// malformed start tags — so the one state the scanner does not implement was
// never reached by it. The rule is right and its scope was the corpus.
//
// The missing state is HTML 13.2.5.32, before attribute name:
//
//	U+003D (=): unexpected-equals-sign-before-attribute-name parse error.
//	Start a new attribute. Set that attribute's name to the current input
//	character, and its value to the empty string.
//
// So in `<a =" href="…">` the parser builds an attribute *named* `="` and then
// an ordinary `href`. scanAttrsInto instead reads an empty name, takes the `=`
// as the name/value separator and the `"` as a quote — and its "value" then runs
// to the next quote in the tag, which is the one opening the real href. From
// there every remaining byte, the href's value included, is scanned as a run of
// attribute *names*, and no span is ever handed to the rewriter.
//
// One malformed attribute is enough, and the tag it sits in need not be the one
// that leaks: this is the whole start tag's worth of values lost at once.

// r66TokAttrs is x/net/html's own reading of a start tag — the tokenizer that
// supplies Raw() to the rewriter, and therefore an oracle that shares none of
// attrspan.go's code.
func r66TokAttrs(raw string) [][2]string {
	z := html.NewTokenizer(strings.NewReader(raw))
	if tt := z.Next(); tt != html.StartTagToken && tt != html.SelfClosingTagToken {
		return nil
	}
	var out [][2]string
	for {
		k, v, more := z.TagAttr()
		out = append(out, [2]string{string(k), string(v)})
		if !more {
			return out
		}
	}
}

// r66ScanValues is the byte ranges scanAttrsInto would hand the rewriter.
func r66ScanValues(raw string) []string {
	var out []string
	for _, a := range scanAttrs([]byte(raw)) {
		if a.ValueStart < 0 {
			continue
		}
		out = append(out, raw[a.ValueStart:a.ValueEnd])
	}
	return out
}

func r66Show(s string) string {
	return strings.NewReplacer("\t", `\t`, "\n", `\n`, "\r", `\r`, "\f", `\f`).Replace(s)
}

// A generated differential over the tag-level alphabet, rather than the tags
// someone thought of. Every attribute value the tokenizer reports must sit
// inside some span the scanner located; extra spans are fine, because a span
// that is not an attribute value is only ever an over-rewrite.
func TestStartTagSpansAgainstTheTokenizer(t *testing.T) {
	pieces := []string{"=", `"`, "'", " ", "\t", "/", "<", "`", "a", "b=", `="`, "='"}
	const marker = "MARKERVALUE"

	var lost []string
	checked := 0
	for _, a := range pieces {
		for _, b := range pieces {
			for _, c := range pieces {
				raw := "<a " + a + b + c + ` href="` + marker + `">`
				want := false
				for _, kv := range r66TokAttrs(raw) {
					if kv[1] == marker {
						want = true
					}
				}
				if !want {
					continue // the tokenizer does not put the value in an attribute here
				}
				checked++
				got := false
				for _, v := range r66ScanValues(raw) {
					if strings.Contains(v, marker) {
						got = true
					}
				}
				if got {
					continue
				}
				if len(lost) < 20 {
					lost = append(lost, "  tag      "+r66Show(raw)+
						"\n   tokenizer "+r66Show(strings.TrimSpace(joinKV(r66TokAttrs(raw))))+
						"\n   scanner   "+r66Show(strings.Join(r66ScanValues(raw), " | ")))
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no generated tag carries the value in an attribute, so this asserts nothing")
	}
	if len(lost) > 0 {
		t.Errorf("%d of %d start tags carry an attribute value the tokenizer reports and "+
			"scanAttrsInto never locates.\nEvery value in such a tag is invisible to the "+
			"structured pass — attrspan.go:87 reads a name beginning `=` as an empty name "+
			"plus a separator, where HTML 13.2.5.32 makes it a name.\n%s",
			len(lost), checked, strings.Join(lost, "\n"))
	}
}

func joinKV(kv [][2]string) string {
	var sb strings.Builder
	for _, p := range kv {
		sb.WriteString(p[0] + "=" + p[1] + "  ")
	}
	return sb.String()
}

// And what that costs: test 28, through the whole response pipeline with §4.4's
// straggler sweep armed.
//
// The sweep is a byte matcher, so it rescues the one contiguous spelling and
// nothing else. Every spelling the URL locator exists for — a tab in the host, a
// userinfo, `https:\\`, a percent-encoded letter, an IDN fold, a character
// reference — is a dereferenceable production origin the browser resolves and
// hostshift emits unchanged, which is precisely the set §4.4 says the structured
// pass owns and the backstop cannot cover.
func TestADesyncedStartTagDoesNotLeak(t *testing.T) {
	m := obfMatcher(t)
	// Each of these resolves to https://www.example.fi/x in the WHATWG parser;
	// the corpus oracle asserts as much for the well-formed spelling of the
	// same tag.
	for _, c := range []struct{ name, val string }{
		{"character references", "https:&#47;&#47;" + oracleCanonical + "/x"},
		{"a tab in the host", "https://www.example\t.fi/x"},
		{"backslash separators", `https:\\` + oracleCanonical + `/x`},
		{"userinfo", "https://u@" + oracleCanonical + "/x"},
		{"a percent-encoded letter", "https://www.ex%61mple.fi/x"},
		{"ideographic label separators", "https://www。example。fi/x"},
		{"a soft hyphen in the host", "https://www.exa­mple.fi/x"},
	} {
		t.Run(c.name, func(t *testing.T) {
			clean := `<a href="` + c.val + `">t</a>`
			// One malformed attribute earlier in the same tag. `="` is what a
			// template printing an attribute it did not build emits.
			desynced := `<a =" href="` + c.val + `">t</a>`

			// The well-formed spelling is rewritten, which is this file's
			// evidence that a browser resolves the value to the canonical —
			// TestURLShapesAgainstBrowserOracle establishes that against ada for
			// every one of these shapes.
			if !strings.Contains(rewriteHTML(t, m, clean, nil), oracleVariant) {
				t.Fatalf("the well-formed spelling is not rewritten either, so this "+
					"tests nothing: %s", clean)
			}
			// And html.Parse is the oracle for the *tag*: the browser is handed
			// the same href in both, so the two differ only in what hostshift
			// did with it.
			want, ok := r66Href(t, clean)
			if !ok {
				t.Fatalf("html.Parse finds no href in %s", clean)
			}
			if got, ok := r66Href(t, desynced); !ok || got != want {
				t.Fatalf("the malformed tag does not give the browser the same href "+
					"(%q vs %q), so it is not a leak: %s", got, want, desynced)
			}
			// The assertion is that the rewrite *happened*, not that the string
			// `www.example.fi` is absent. A folded host — `www。example。fi`, a
			// soft hyphen, a fullwidth letter — never spells the canonical, so a
			// search for it is a check that cannot fire on the very spellings
			// foldedHostLeak exists for. This file's first draft did that and
			// reported four of the seven cells clean.
			out := rewriteHTML(t, m, desynced, nil)
			if !strings.Contains(out, oracleVariant) {
				t.Errorf("a dereferenceable production origin reached the browser — test 28.\n"+
					" in:  %s\n out: %s", r66Show(desynced), r66Show(out))
			}
		})
	}
}

// r66Href is the href html.Parse gives the browser, which is the value after
// the parser's own decoding and before any URL parser runs.
func r66Href(t *testing.T, doc string) (string, bool) {
	t.Helper()
	root, err := html.Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatal(err)
	}
	var out string
	found := false
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		for _, a := range n.Attr {
			if a.Key == "href" && !found {
				out, found = a.Val, true
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	return out, found
}

package rewrite

import (
	"fmt"
	"strings"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
)

// The JSON quote has two spellings and the walk reads one of them.
//
// PLAN §"The spellings are a product" models the space as transport (raw,
// percent) crossed with escaping (literal, entity, JSON) — six cells, plus
// `esc_attr(wp_json_encode(…))` as a seventh. That model is short by a member:
// *JSON escaping of a quote* is not one spelling but two. `json_encode` writes
// `\"` by default and `"` under JSON_HEX_QUOT, and both are ordinary
// output of the same function.
//
// JSON_HEX_QUOT is not exotic. WordPress core's Interactivity API writes
// `data-wp-context` with
//
//	wp_json_encode( $context, JSON_HEX_TAG|JSON_HEX_APOS|JSON_HEX_QUOT|JSON_HEX_AMP )
//
// — that flag set is what makes the single-quoted attribute safe without a
// second escaping pass, so every interactive block since 6.5 emits it, and any
// serialized value carried in a block's context is spelled this way. Verified
// against PHP 8.4:
//
//	json_encode(['b' => serialize(['https://www.canon.test/a.png'])],
//	            JSON_HEX_QUOT|JSON_HEX_TAG|JSON_HEX_AMP|JSON_HEX_APOS)
//	=> {"b":"a:1:{i:0;s:28:"https:\/\/www.canon.test\/a.png";}"}
//
// No syntax in repairAt matches `"` as a delimiter — jsonSyntax accepts
// only `\"`, jsonHTMLSyntax only `\&quot;` — so the value is declined. A
// decline is not neutral: the byte matcher still rewrites `https:\/\/` +
// canonical to the variant (that is the encJSON pattern), the data loses three
// bytes, and `s:28:` is re-emitted from nothing. PHP 8.4 `unserialize()` returns
// false on the served bytes.
//
// The detector cannot see it either: BrokenSerialized walks the same seven
// spellings, so the canonical page counts one broken value and the variant
// counts one, and the corpus diff's baseline subtraction cancels them exactly —
// see TestAJSONHexQuotedBlobIsNotAGreenRun in internal/corpus. That is the
// false-GREEN mechanism jsonHTMLSyntax's own comment says it exists to close,
// one escaping-spelling over.
func TestAJSONHexQuotedSerializedValueKeepsItsLength(t *testing.T) {
	m, err := origin.NewMatcher([]origin.Pair{{
		Name:      "main",
		Canonical: origin.MustParse("https://www.canon.test"),
		Variant:   origin.MustParse("https://v.ddev.site"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}

	// Built rather than typed, so no hardcoded `s:N:` can be off by a byte.
	// The two hosts differ in length (14 against 11), so a stale length is
	// distinguishable from a correct one.
	canonURL := "https://www.canon.test/a.png"
	variantURL := "https://v.ddev.site/a.png"
	if len(canonURL) == len(variantURL) {
		t.Fatalf("the fixture's two URLs are the same length, so this asserts nothing")
	}
	// `"` assembled from bytes: a Go escape would be a real quote.
	q := string([]byte{'\\', 'u', '0', '0', '2', '2'})
	esc := strings.ReplaceAll(canonURL, "/", `\/`)
	blob := fmt.Sprintf(`a:1:{i:0;s:%d:%s%s%s;}`, len(canonURL), q, esc, q)
	in := `<div data-wp-context='{"b":"` + blob + `"}'>x</div>`

	// The fixture must really carry the canonical origin in a spelling the
	// engine claims to rewrite, or a passing test would prove nothing.
	if !strings.Contains(in, `https:\/\/www.canon.test`) {
		t.Fatalf("fixture does not carry the JSON-escaped canonical origin: %s", in)
	}

	out := rewriteHTML(t, m, in, NewStats(false))

	if !strings.Contains(out, "v.ddev.site") {
		t.Fatalf("the host was not rewritten at all, so the length claim is moot:\n%s", out)
	}
	want := fmt.Sprintf("s:%d:", len(variantURL))
	stale := fmt.Sprintf("s:%d:", len(canonURL))
	if strings.Contains(out, stale) || !strings.Contains(out, want) {
		t.Errorf("a JSON_HEX_QUOT-escaped serialized value was served with a stale length:\n"+
			"  in   %s\n  out  %s\n  want %q, got %q — PHP unserialize() returns false",
			in, out, want, stale)
	}
}

// The same hole one layer up, for the record: the escaping axis composes with
// itself in orders nobody has covered.
//
// Each row is a real encoder pipeline; each produces a value that repairAt
// declines while the byte matcher still rewrites the host, so the length is
// re-emitted from nothing. They are the same defect as the test above and are
// kept in one place so the product can be read off.
func TestTheEscapingProductIsNotClosed(t *testing.T) {
	m, err := origin.NewMatcher([]origin.Pair{{
		Name:      "main",
		Canonical: origin.MustParse("https://www.canon.test"),
		Variant:   origin.MustParse("https://v.ddev.site"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	canonURL := "https://www.canon.test/a.png"
	variantURL := "https://v.ddev.site/a.png"
	q := string([]byte{'\\', 'u', '0', '0', '2', '2'})
	plain := fmt.Sprintf(`a:1:{i:0;s:%d:"%s";}`, len(canonURL), canonURL)

	// jsonBody is phpJSONEncode's escaping without the surrounding quotes.
	jsonBody := func(s string) string {
		e := phpJSONEncode(s)
		return e[1 : len(e)-1]
	}

	for _, c := range []struct{ name, body string }{
		// wp_json_encode($x, JSON_HEX_QUOT) — WordPress core, Interactivity API.
		{"json_encode with JSON_HEX_QUOT", strings.ReplaceAll(jsonBody(plain), `\"`, q)},
		// json_encode(esc_attr($x)): the value was HTML-escaped when it was
		// stored and JSON-encoded when it was read back out.
		{"json_encode(esc_attr(x))", jsonBody(escAttrNoDouble(plain))},
		// json_encode(json_encode($x)): a JSON document carried as a string
		// inside another one, which is what a block attribute holding JSON is.
		{"json_encode(json_encode(x))", jsonBody(jsonBody(plain))},
	} {
		t.Run(c.name, func(t *testing.T) {
			if !strings.Contains(c.body, "www.canon.test") {
				t.Fatalf("fixture lost the canonical host: %s", c.body)
			}
			out := string(RepairSerialized([]byte(c.body), func(b []byte) []byte {
				nv, _ := m.Rewrite(b, SurfaceHTMLAttr, false)
				return HostLeaks(m, nv, true)
			}))
			if !strings.Contains(out, "v.ddev.site") {
				t.Skipf("the host was not rewritten in this spelling, so no length went stale:\n%s", out)
			}
			stale := fmt.Sprintf("s:%d:", len(canonURL))
			want := fmt.Sprintf("s:%d:", len(variantURL))
			if strings.Contains(out, stale) || !strings.Contains(out, want) {
				t.Errorf("length not re-emitted:\n  in   %s\n  out  %s\n  want %q", c.body, out, want)
			}
		})
	}
}

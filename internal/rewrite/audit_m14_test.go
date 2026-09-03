package rewrite

import (
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
)

// m14Blob is a small options row: a URL and a label, every length measured from
// its own data.
func m14Blob(origin string) string {
	u := origin + "/wp-content/uploads/x.png"
	return `a:2:{` + m12Str("u") + m12Str(u) + m12Str("note") + m12Str("Hej") + `}`
}

// m14PctDecode is `rawurldecode`. Not `url.QueryUnescape`, which also turns a
// `+` into a space — that is the form-body rule, not this one.
func m14PctDecode(t *testing.T, s string) string {
	t.Helper()
	out, err := url.PathUnescape(s)
	if err != nil {
		t.Fatalf("the proxy served bytes that are not percent-encoded any more: %v\n%s", err, s)
	}
	return out
}

// m14HTMLDecode is what a browser does to an attribute value: the inverse of
// `esc_attr`. `&quot;` first, so a literal `&amp;quot;` in the data cannot be
// read as an escaped quote.
func m14HTMLDecode(s string) string {
	return strings.NewReplacer(
		"&quot;", `"`, "&#039;", "'", "&lt;", "<", "&gt;", ">", "&amp;", "&").Replace(s)
}

// m14JSONDecode is `json_decode` over the escapes this fixture can contain:
// `\"`, `\\` and `\/`. It also strips the string's own delimiters, which
// `phpJSONEncode` writes.
func m14JSONDecode(t *testing.T, s string) string {
	t.Helper()
	if len(s) < 2 || s[0] != '"' || s[len(s)-1] != '"' {
		t.Fatalf("the proxy served something that is no longer a JSON string:\n%s", s)
	}
	s = s[1 : len(s)-1]
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			b.WriteByte(s[i+1])
			i++
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// m14Carriers is the product this walk claims to read: the transport a value
// arrives under, crossed with the escaping underneath it.
//
// Round 34 added the fifth of these, `rawurlencode(esc_attr(…))`, because
// nothing read percent-over-entity and a skip is not neutral — the host is
// rewritten and no length re-emitted. The product it closed one cell of has
// another empty one: percent over *JSON*. `stripForPercent` exists for exactly
// that shape — its own comment names `https%3A%5C%2F%5C%2Fhost`, "one `%5C%2F`
// per `\/`", from the `JSON.parse(decodeURIComponent("…"))` blobs plugins
// inline — so the percent view finds the host and rewrites it, while no
// spelling can read the value: percentSyntax wants `%22` where the JSON layer
// put `%5C%22`, and charges the `%5C` of every `\/` a decoded byte that
// `serialize` never counted.
var m14Carriers = []struct {
	name string
	enc  func(string) string
	dec  func(*testing.T, string) string
}{
	{"esc_attr", escAttrNoDouble,
		func(t *testing.T, s string) string { return m14HTMLDecode(s) }},
	{"esc_attr(wp_json_encode)",
		func(s string) string { return escAttrNoDouble(phpJSONEncode(s)) },
		func(t *testing.T, s string) string { return m14JSONDecode(t, m14HTMLDecode(s)) }},
	{"rawurlencode", m12RawURLEncode,
		func(t *testing.T, s string) string { return m14PctDecode(t, s) }},
	{"rawurlencode(esc_attr)",
		func(s string) string { return m12RawURLEncode(escAttrNoDouble(s)) },
		func(t *testing.T, s string) string { return m14HTMLDecode(m14PctDecode(t, s)) }},
	{"rawurlencode(wp_json_encode)",
		func(s string) string { return m12RawURLEncode(phpJSONEncode(s)) },
		func(t *testing.T, s string) string { return m14JSONDecode(t, m14PctDecode(t, s)) }},
}

// m14Maps is TestMapShapes' list: the shapes a real configuration takes. The
// ddev one is the control — it changes only the hostname, so no spelling's
// count of escapes can change and every cell has to pass on it.
var m14Maps = []struct{ name, canonical, variant string }{
	{"the ordinary ddev shape", "https://www.example.fi", "https://wt-a--example.ddev.site"},
	{"a variant with a port", "https://www.example.fi", "http://localhost:8080"},
	{"a variant on the other scheme", "https://www.example.fi", "http://v.ddev.site"},
	{"a canonical with a port", "https://www.example.fi:8443", "https://v.ddev.site"},
}

// The whole product of transport and escaping, through the engine the proxy
// runs, against the whole product of map shapes.
//
// The single cell is not the unit to test here, because a hole in a product
// passes every neighbouring cell for reasons that say nothing about it. Two
// cells are empty at HEAD:
//
//   - `rawurlencode(wp_json_encode(…))` on *every* map, including the plain
//     ddev one. Nothing reads percent-over-JSON, so the host is rewritten and
//     the length left exactly as it arrived. `unserialize` returns false on the
//     served bytes.
//
//   - `rawurlencode(esc_attr(…))` wherever the map changes how many percent
//     escapes the data holds — a port on either side. percentHTMLSyntax has no
//     `dlen`, so emitLen takes the *source* delta, and in a percent-encoded
//     value one decoded byte is three source bytes. That is the same arithmetic
//     emitLen's own comment says `decodedLen` exists to prevent one spelling
//     over: `http%3A%2F%2Flocalhost%3A8080` is one source byte shorter and one
//     decoded byte longer than what replaces it.
//
// The assertion is the decoded value itself, not just its lengths: decoding the
// attribute back through the layers a browser and PHP apply is the only verdict
// that counts, and it catches a mangled origin as well as a stale count.
func TestASerializedPayloadStaysParseableThroughEveryTransportAndEscaping(t *testing.T) {
	for _, shape := range m14Maps {
		for _, carry := range m14Carriers {
			t.Run(shape.name+"/"+carry.name, func(t *testing.T) {
				canon, variant := origin.MustParse(shape.canonical), origin.MustParse(shape.variant)
				m, err := origin.NewMatcher([]origin.Pair{{Canonical: canon, Variant: variant}})
				if err != nil {
					t.Fatal(err)
				}
				in := `<div data-x="` + carry.enc(m14Blob(canon.String())) + `">y</div>`
				served := m14Attr(t, rewriteHTML(t, m, in, NewStats(false)))
				got := carry.dec(t, served)

				// The control: a stub or an encoder that rewrote nothing would
				// leave the canonical host here and every assertion below would
				// pass on untouched bytes.
				if strings.Contains(got, canon.Host) {
					t.Fatalf("the rewrite never reached the payload, so this asserts nothing:\n%s", got)
				}
				want := m14Blob(variant.String())
				if got != want {
					t.Errorf("the browser was served a value PHP refuses:\n got  %s\n want %s\n wire %s",
						got, want, served)
				}
				assertEveryLength(t, got)
			})
		}
	}
}

// m14Attr is the value of the `data-x` attribute in a served page. Every
// carrier writes a value with no bare `"` in it, which is what lets it sit in a
// double-quoted attribute at all.
func m14Attr(t *testing.T, page string) string {
	t.Helper()
	const open = `data-x="`
	i := strings.Index(page, open)
	if i < 0 {
		t.Fatalf("the attribute did not survive the rewrite:\n%s", page)
	}
	i += len(open)
	j := strings.IndexByte(page[i:], '"')
	if j < 0 {
		t.Fatalf("the attribute was never closed:\n%s", page)
	}
	return page[i : i+j]
}

// A length re-emitted in the percent-over-entity spelling counts decoded bytes.
//
// emitLen measures with `syn.dlen` where a spelling has one reading and takes a
// *source* delta where a character reference makes it ambiguous. Its own
// comment says why the delta is wrong for a percent-encoded value — a separator
// is three source bytes for one decoded one, so a map that changes the scheme or
// drops a port moves the two counts in opposite directions — and percentSyntax
// carries `dlen: decodedLen` for it. percentHTMLSyntax, added a round later,
// carries none, so the delta is back.
//
// The two spellings are run side by side over the same payload and the same
// map. They differ in one thing only: whether an entity layer sits under the
// percent one. Both must re-emit the same number, because both describe the same
// decoded data.
func TestThePercentEntitySpellingCountsALengthInDecodedBytes(t *testing.T) {
	// A port on the variant, so the rewrite adds a `%3A` the canonical did not
	// have: the source grows by one byte while the decoded data shrinks by one.
	canon, variant := origin.MustParse("https://www.example.fi"), origin.MustParse("http://localhost:8080")
	m, err := origin.NewMatcher([]origin.Pair{{Canonical: canon, Variant: variant}})
	if err != nil {
		t.Fatal(err)
	}
	u := variant.String() + "/wp-content/uploads/x.png"
	want := strconv.Itoa(len(u))

	raw := m14Blob(canon.String())
	for _, c := range []struct {
		name string
		enc  func(string) string
		dec  func(*testing.T, string) string
	}{
		{"rawurlencode", m12RawURLEncode,
			func(t *testing.T, s string) string { return m14PctDecode(t, s) }},
		{"rawurlencode(esc_attr)",
			func(s string) string { return m12RawURLEncode(escAttrNoDouble(s)) },
			func(t *testing.T, s string) string { return m14HTMLDecode(m14PctDecode(t, s)) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := `<div data-x="` + c.enc(raw) + `">y</div>`
			got := c.dec(t, m14Attr(t, rewriteHTML(t, m, in, NewStats(false))))
			if !strings.Contains(got, u) {
				t.Fatalf("the rewrite never reached the URL, so this asserts nothing:\n%s", got)
			}
			if !strings.Contains(got, `s:`+want+`:"`+u+`"`) {
				t.Errorf("the length counts source bytes, not the decoded ones PHP counts:"+
					"\n got  %s\n want s:%s:%q", got, want, u)
			}
		})
	}
}

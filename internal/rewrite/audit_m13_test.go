package rewrite

import (
	"log/slog"
	"strconv"
	"strings"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
)

const (
	m13Canonical = "https://www.example.fi"
	m13HostOnly  = "www.example.fi"
	m13Variant   = "https://wt-a--x.ddev.site"
)

func m13Matcher(t *testing.T) *origin.Matcher {
	t.Helper()
	m, err := origin.NewMatcher([]origin.Pair{{
		Canonical: origin.MustParse(m13Canonical),
		Variant:   origin.MustParse(m13Variant),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// m13Str is `s:LEN:"DATA";` with LEN measured, never written by hand.
func m13Str(v string) string { return "s:" + strconv.Itoa(len(v)) + `:"` + v + `";` }

// m13JSONString is the raw span a JSON string value occupies, escaped the way
// jsontext.AppendQuote writes it: `"` and `\` escaped, `/` escaped as
// wp_json_encode writes it, everything else literal.
func m13JSONString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '/':
			b.WriteString(`\/`)
		default:
			b.WriteByte(s[i])
		}
	}
	b.WriteByte('"')
	return b.String()
}

// decodeJSONLeak splices the decoder views *outside* the repair, so an origin
// only those views can see has its host replaced with the length left stale.
//
// json.go's decodeJSONLeak runs
//
//	out := RepairSerialized(dec, m.Rewrite)
//	out = hostsFor(m).rewriteAllRefs(out, true, SurfaceHTMLAttr, nil)
//
// and the second line is not inside the walk. rewriteAllRefs is the URL-parser
// locator, the CSS view, the percent view and the reference views — every
// spelling the byte matcher cannot see — and it splices a host of a different
// length into a `s:NN:"…"` that nothing then re-emits. This is the corruption
// serialized.go's header is about, at the one call site that reaches for the
// views after the walk has finished rather than from inside it.
//
// `{"opt":"a:1:{s:3:\"url\";s:24:\"https:\\\\www.example.fi\/x\";}"}` is served
// with `s:24:` over 27 bytes; `unserialize()` on the served value returns false
// with "Error at offset 45 of 51 bytes". Every one of these spellings is one the
// project already documents as live — an obfuscated separator, a scheme with no
// slashes, userinfo, a CSS escape — and a REST body is where Gutenberg reads its
// block attributes and writes them back.
func TestTheDecoderViewsInsideASerializedJSONValueKeepTheirLengths(t *testing.T) {
	m := m13Matcher(t)
	for _, tc := range []struct{ name, url string }{
		// The control: a spelling the byte matcher can see, repaired correctly.
		{"plain", m13Canonical + "/x"},
		{"backslash separator", `https:\\www.example.fi/x`},
		{"scheme with no slashes", "http:www.example.fi/x"},
		{"userinfo", "https://u@www.example.fi/x"},
		{"css escaped", `https\3a \2f \2f www.example.fi/x`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blob := `a:1:{` + m13Str("url") + m13Str(tc.url) + `}`
			in := `{"opt":` + m13JSONString(blob) + `}`
			// The fixture is sound before the proxy sees it, so a failure below
			// is the rewrite and not a hand-written length.
			if n := BrokenSerialized([]byte(in)); n != 0 {
				t.Fatalf("the fixture is already broken (%d): %s", n, in)
			}
			got := string(RewriteJSON([]byte(in), m, NewStats(false), slog.Default(), false))
			if n := BrokenSerialized([]byte(got)); n != 0 {
				t.Errorf("served %d value(s) PHP refuses:\n in  %s\n out %s", n, in, got)
			}
		})
	}
}

// A JSON string value that carries a serialized payload is routed away from the
// decoder views entirely, so every obfuscated origin in it reaches the browser.
//
// RewriteJSON skips decodeJSONLeak when serializedJSONValue reported a payload:
//
//	if dv, ok := decodeJSONLeak(m, nv); ok && !serialized {
//
// The reason given is that the escape pass "would decode the value and rewrite
// it again, which on a serialized payload re-breaks the lengths just repaired" —
// which is true, and is the bug above. But serializedJSONValue runs `m.Rewrite`
// alone: no URL-parser locator, no IDNA fold, no CSS view, no reference view. So
// the moment a payload has one origin the byte matcher *can* see, every origin
// in the same payload that it cannot is passed through untouched.
//
// A production origin reaches the browser (test 28) and BrokenSerialized is
// zero, because a value nobody rewrote still parses — the shape of blindness the
// detector was added for. In the request direction the same value carries the
// variant hostname upstream instead.
func TestASerializedJSONValueStillGetsTheDecoderViews(t *testing.T) {
	m := m13Matcher(t)
	plain := m13Canonical + "/a"
	// A spelling a browser resolves to the canonical origin and the byte matcher
	// cannot see — urlobf.go's own opening list.
	obfuscated := `https:\\www.example.fi/b`

	blob := `a:2:{` + m13Str("a") + m13Str(plain) +
		m13Str("b") + m13Str(obfuscated) + `}`
	in := `{"opt":` + m13JSONString(blob) + `}`
	if n := BrokenSerialized([]byte(in)); n != 0 {
		t.Fatalf("the fixture is already broken (%d): %s", n, in)
	}
	got := string(RewriteJSON([]byte(in), m, NewStats(false), slog.Default(), false))

	if strings.Contains(got, "www.example.fi") {
		t.Errorf("a canonical origin reached the browser:\n in  %s\n out %s", in, got)
	}
	if n := BrokenSerialized([]byte(got)); n != 0 {
		t.Errorf("served %d value(s) PHP refuses:\n out %s", n, got)
	}
}

// The decoder views run inside decodeJSONLeak's walk, not after it.
//
// That path is reached only when serializedJSONValue found nothing, so the
// payload has to be invisible until decodeURLRefs has run — a header whose type
// letter is written `&#115;`. Contrived as a fixture, and the exact shape test
// 28 exists for: an obfuscated spelling the byte matcher cannot see, in the one
// surface that reached for the views *after* the walk instead of from within
// it.
//
// Spliced afterwards, rewriteAllRefs replaces a host of a different length
// inside an `s:NN:"…"` that nothing then re-emits. Here that is `s:24:` over 27
// bytes, and PHP refuses it.
func TestDecodeJSONLeakRunsTheViewsInsideItsWalk(t *testing.T) {
	m := m13Matcher(t)
	// A separator spelling the byte matcher cannot see, so the views are what
	// rewrite it — and a header the walk cannot see until the refs decode.
	host := `https:\\` + m13HostOnly + `/x`
	blob := `a:1:{` + m13Str("u") + m13Str(host) + `}`
	hidden := strings.Replace(blob, `s:`+strconv.Itoa(len(host)), `&#115;:`+strconv.Itoa(len(host)), 1)
	if hidden == blob {
		t.Fatal("the fixture did not hide the header, so it proves nothing")
	}
	in := `{"opt":` + m13JSONString(hidden) + `}`

	got := string(RewriteJSON([]byte(in), m, NewStats(false), slog.Default(), false))
	if strings.Contains(got, m13HostOnly) {
		t.Errorf("the origin survived every view:\n %s", got)
	}
	if n := BrokenSerialized([]byte(got)); n != 0 {
		t.Errorf("served %d value(s) PHP refuses:\n in  %s\n out %s", n, in, got)
	}
}

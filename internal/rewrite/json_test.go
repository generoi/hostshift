package rewrite

import (
	"encoding/json"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
)

func rwJSON(t *testing.T, m *origin.Matcher, in string) string {
	t.Helper()
	return string(RewriteJSON([]byte(in), m, NewStats(false), quiet(), false))
}

// TestJSONStringValues is acceptance test 4: a JSON-escaped origin in a REST
// response. WordPress stores unescaped slashes in the database and escapes them
// on the way out, so https:\/\/host\/ is the form that actually arrives.
func TestJSONStringValues(t *testing.T) {
	m := pairMatcher(t, "https://c.example", "https://v.example")
	cases := []struct{ name, in, want string }{
		{
			"escaped slashes",
			`{"link":"https:\/\/c.example\/2026\/08\/post\/"}`,
			`{"link":"https:\/\/v.example\/2026\/08\/post\/"}`,
		},
		{
			"unescaped slashes are equally valid JSON",
			`{"link":"https://c.example/x"}`,
			`{"link":"https://v.example/x"}`,
		},
		{
			"nested objects and arrays",
			`{"_links":{"self":[{"href":"https:\/\/c.example\/wp-json\/wp\/v2\/posts\/1"}]}}`,
			`{"_links":{"self":[{"href":"https:\/\/v.example\/wp-json\/wp\/v2\/posts\/1"}]}}`,
		},
		{
			// Test 22: content.rendered is a full HTML blob. It needs no HTML
			// rewriter and no decoding: the origins appear literally in the raw
			// JSON string, in the escaped form the automaton already carries.
			// Decoding and re-encoding would be re-serialisation, which §5.2
			// forbids.
			"HTML-in-JSON",
			`{"content":{"rendered":"<p>see <a href=\"https:\/\/c.example\/x\" class=\"k\">this<\/a><\/p>"}}`,
			`{"content":{"rendered":"<p>see <a href=\"https:\/\/v.example\/x\" class=\"k\">this<\/a><\/p>"}}`,
		},
		{
			// JSON-LD, as Yoast emits it. It reaches the JSON rewriter only when
			// served as a response; inside a <script> tag the HTML raw-text scan
			// handles it. Either way the answer is the same.
			"JSON-LD graph",
			`{"@context":"https://schema.org","@graph":[{"@type":"WebSite","@id":"https://c.example/#website","url":"https://c.example/"}]}`,
			`{"@context":"https://schema.org","@graph":[{"@type":"WebSite","@id":"https://v.example/#website","url":"https://v.example/"}]}`,
		},
		{
			// Keys are left alone: an origin in a key is not a link the browser
			// will dereference, and rewriting one changes the document's shape
			// rather than its links.
			"keys are not rewritten",
			`{"https://c.example/":"value"}`,
			`{"https://c.example/":"value"}`,
		},
		{
			"third-party hosts untouched",
			`{"a":"https://cdn.jsdelivr.net/x.js","b":"https://c.example/y"}`,
			`{"a":"https://cdn.jsdelivr.net/x.js","b":"https://v.example/y"}`,
		},
		{
			"numbers, booleans and null are untouched",
			`{"n":1.5e3,"b":true,"z":null,"u":"https://c.example/"}`,
			`{"n":1.5e3,"b":true,"z":null,"u":"https://v.example/"}`,
		},
		{
			"a bare hostname in a JSON string is not a URL",
			`{"note":"visit c.example for details"}`,
			`{"note":"visit c.example for details"}`,
		},
		{
			"top-level array",
			`[{"href":"https://c.example/a"},{"href":"https://c.example/b"}]`,
			`[{"href":"https://v.example/a"},{"href":"https://v.example/b"}]`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := rwJSON(t, m, c.in)
			if got != c.want {
				t.Errorf("\n got %s\nwant %s", got, c.want)
			}
			if !json.Valid([]byte(got)) {
				t.Errorf("output is not valid JSON: %s", got)
			}
		})
	}
}

// TestJSONPreservesEverythingElse is test 24's property on the JSON surface:
// whitespace, key order, number formatting and escape style all survive,
// because only origin spans are replaced.
func TestJSONPreservesEverythingElse(t *testing.T) {
	m := pairMatcher(t, "https://c.example", "https://v.example")
	in := "{\n  \"b\" : 1.50,\n  \"a\"\t: \"https:\\/\\/c.example\\/x\",\n  \"c\": [ ]\n}"
	want := "{\n  \"b\" : 1.50,\n  \"a\"\t: \"https:\\/\\/v.example\\/x\",\n  \"c\": [ ]\n}"
	if got := rwJSON(t, m, in); got != want {
		t.Errorf("\n got %q\nwant %q", got, want)
	}
}

// TestJSONIdentityMap: test 24 on the JSON surface.
func TestJSONIdentityMap(t *testing.T) {
	m := pairMatcher(t, "https://c.example", "https://c.example")
	for _, in := range []string{
		`{"a":"https:\/\/c.example\/x","b":[1,{"c":"//c.example/y"}]}`,
		`{"content":{"rendered":"<a href=\"https:\/\/c.example\/x\">k<\/a>"}}`,
	} {
		out := RewriteJSON([]byte(in), m, NewStats(false), quiet(), false)
		if string(out) != in {
			t.Errorf("identity map changed the JSON:\n got %s\nwant %s", out, in)
		}
		if &out[0] != &[]byte(in)[0] && len(out) > 0 {
			// Not required, but the matcher returns the input slice untouched
			// and RewriteJSON should not copy either.
			if string(out) != in {
				t.Errorf("identity map copied instead of returning the input")
			}
		}
	}
}

// TestJSONIdempotent is test 7 on the JSON surface.
func TestJSONIdempotent(t *testing.T) {
	m := pairMatcher(t, "https://c.example", "https://wt-a--c.example")
	in := `{"a":"https:\/\/c.example\/x","b":"//c.example/y"}`
	once := rwJSON(t, m, in)
	twice := rwJSON(t, m, once)
	if once != twice {
		t.Errorf("not a fixed point:\n once %s\ntwice %s", once, twice)
	}
}

// TestMalformedJSONUnchanged: a half-rewritten body is worse than an
// unrewritten one, so a parse failure returns the input as it came.
func TestMalformedJSONUnchanged(t *testing.T) {
	m := pairMatcher(t, "https://c.example", "https://v.example")
	for _, in := range []string{
		`{"a":"https://c.example/x"`,          // truncated
		`{"a":"https://c.example/x",}`,        // trailing comma
		`not json at all https://c.example/`,  // not JSON
		"\x89PNG\r\n\x1a\nhttps://c.example/", // binary mislabelled as JSON
	} {
		if got := rwJSON(t, m, in); got != in {
			t.Errorf("malformed input was modified:\n got %s\nwant %s", got, in)
		}
	}
}

// TestJSONExplainCarriesAPointer: --explain has to say *where* in the document,
// or a REST response with two hundred URLs is no easier to diagnose than before.
func TestJSONExplainCarriesAPointer(t *testing.T) {
	m := pairMatcher(t, "https://c.example", "https://v.example")
	st := NewStats(true)
	RewriteJSON([]byte(`{"_links":{"self":[{"href":"https://c.example/x"}]}}`), m, st, quiet(), true)

	ev := st.Events()
	if len(ev) != 1 {
		t.Fatalf("%d events, want 1: %+v", len(ev), ev)
	}
	if ev[0].Path != "/_links/self/0/href" {
		t.Errorf("path is %q, want the RFC 6901 pointer /_links/self/0/href", ev[0].Path)
	}
	if ev[0].Surface != SurfaceJSONString {
		t.Errorf("surface is %q, want %q", ev[0].Surface, SurfaceJSONString)
	}
	// 27 is the opening quote of the value; the origin starts one byte later.
	// --explain points at the origin, not at the string that contains it.
	if ev[0].Offset != 28 {
		t.Errorf("offset is %d, want 28 — the span must point at the origin in the input", ev[0].Offset)
	}
}

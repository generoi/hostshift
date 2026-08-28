package rewrite

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
)

// runJSON is the full JSON response pipeline the proxy runs: the span scanner,
// then §4.4's backstop.
func runJSON(t *testing.T, m *origin.Matcher, in string, st *Stats) string {
	t.Helper()
	out := RewriteJSON([]byte(in), m, st, quiet(), st.Explain())
	return string(SweepBytes(out, m, st, quiet()))
}

// TestJSONMalformedIsReportedNotSilent is the difference between a policy and a
// bug. Passing an unparseable body through untouched is the right call; doing it
// with no log line, no skip counter, and the counters still claiming the
// rewrites that were rolled back is not.
//
// A duplicate object member is legal JSON that jsontext rejects by default, so
// this is reachable from any upstream that emits one — as are invalid UTF-8, a
// lone surrogate escape, a BOM, trailing garbage, and a body that arrived gzipped
// despite the forced Accept-Encoding: identity.
func TestJSONMalformedIsReportedNotSilent(t *testing.T) {
	m := pairMatcher(t, "https://c.example", "https://v.example")
	for _, in := range []string{
		`{"link":"https://c.example/a","link":"https://c.example/b"}`,
		`{"link":"https://c.example/a"} trailing garbage`,
		"\ufeff" + `{"link":"https://c.example/a"}`,
		`{"link":"https://c.example/a"`,
	} {
		st := NewStats(false)
		out := string(RewriteJSON([]byte(in), m, st, quiet(), false))
		if out != in {
			t.Errorf("a body that does not parse must pass through untouched\n in  %s\n out %s", in, out)
		}
		if n := st.Rewrites(SurfaceJSONString); n != 0 {
			t.Errorf("%s: counted %d rewrites that were rolled back", in, n)
		}
		if st.Snapshot().Skips[origin.ReasonNotDecodable] != 1 {
			t.Errorf("%s: the skip was not counted, so --json reports nothing happened", in)
		}
	}
}

// TestJSONHasAStragglerSweep is §4.4 applied to the surface M4 added.
//
// The HTML path wires the backstop; the JSON path wired nothing, so every miss
// was a silent test 28 leak. The same post body went out clean as text/html and
// carrying a production origin as application/json, with no WARN and no
// non-zero counter — the inverse of "each straggler is a bug to fix".
func TestJSONHasAStragglerSweep(t *testing.T) {
	m := pairMatcher(t, "https://c.example", "https://v.example")
	// A duplicate member: the scanner bails, so only the sweep can catch this.
	in := `{"link":"https://c.example/a","link":"https://c.example/b"}`

	st := NewStats(false)
	out := runJSON(t, m, in, st)
	if strings.Contains(out, "c.example") {
		t.Errorf("a production origin reached the browser:\n%s", out)
	}
	if st.Rewrites(SurfaceStraggler) != 2 {
		t.Errorf("stragglers = %d, want 2 — the operator gets no signal",
			st.Rewrites(SurfaceStraggler))
	}
}

// TestJSONEscapedOriginIsRewritten covers the escape spellings a raw scan over
// the still-quoted bytes cannot see. Each one is test 28, which §7 marks
// safety-critical.
func TestJSONEscapedOriginIsRewritten(t *testing.T) {
	for _, c := range []struct{ name, canonical, in string }{
		{
			// PHP's json_encode escapes every non-ASCII rune unless
			// JSON_UNESCAPED_UNICODE is passed, and wp_json_encode does not
			// pass it. §5.5 calls IDN real for .fi client domains, so on such a
			// site the page rewrites and the REST API does not.
			"idn escaped by json_encode",
			"https://hämeen.fi",
			`{"link":"https:\/\/h\u00e4meen.fi\/x"}`,
		},
		{
			// The class M3 closed for attribute values. The identical post body
			// is clean as text/html and leaks as application/json.
			"html character reference inside content.rendered",
			"https://c.example",
			`{"content":{"rendered":"<a href=\"https:&#47;&#47;c.example&#47;x\">y<\/a>"}}`,
		},
		{
			// A block attribute holding JSON, serialised into JSON again.
			"double-escaped json in json",
			"https://c.example",
			`{"attrs":"{\\\"url\\\":\\\"https:\\/\\/c.example\\/x\\\"}"}`,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := pairMatcher(t, c.canonical, "https://v.example")
			st := NewStats(false)
			out := runJSON(t, m, c.in, st)

			host := strings.TrimPrefix(c.canonical, "https://")
			esc := strings.ReplaceAll(host, "\u00e4", `\u00e4`)
			if strings.Contains(out, host) || strings.Contains(out, esc) {
				t.Errorf("a dereferenceable production origin survived\n in  %s\n out %s", c.in, out)
			}
			if !strings.Contains(out, "v.example") {
				t.Errorf("not rewritten to the variant\n in  %s\n out %s", c.in, out)
			}
			var v any
			if err := json.Unmarshal([]byte(out), &v); err != nil {
				t.Errorf("the splice produced invalid JSON: %v\n%s", err, out)
			}
			// The carve-out is what caught it, not the sweep — which cannot,
			// since it scans raw bytes and these origins are not raw.
			if st.Rewrites(SurfaceJSONEscape) != 1 {
				t.Errorf("json-escape = %d, want 1: this went through some other path",
					st.Rewrites(SurfaceJSONEscape))
			}
			if n := st.Rewrites(SurfaceStraggler); n != 0 {
				t.Errorf("the backstop fired %d times; the scanner should have handled it", n)
			}
		})
	}
}

// TestJSONEscapeCarveOutIsNarrow is what keeps the carve-out honest. Unquoting
// and re-encoding a string is the re-serialisation §5.2 forbids, so it may only
// happen to a string that would otherwise leak — never to one the raw scan
// already handled, and never to one with no origin in it at all.
func TestJSONEscapeCarveOutIsNarrow(t *testing.T) {
	m := pairMatcher(t, "https://c.example", "https://v.example")
	for _, c := range []struct{ in, want string }{
		// No origin: byte-identical, escapes and all.
		{`{"a":"ä\/x","b":"café"}`, `{"a":"ä\/x","b":"café"}`},
		// The raw scan handles it, so the escape spelling at the match site
		// survives exactly as it arrived.
		{`{"link":"https:\/\/c.example\/x"}`, `{"link":"https:\/\/v.example\/x"}`},
		{`{"link":"https://c.example/x"}`, `{"link":"https://v.example/x"}`},
		// Unrelated escapes elsewhere in a string that does get rewritten by
		// the raw scan are still untouched.
		{`{"link":"café https:\/\/c.example\/x"}`, `{"link":"café https:\/\/v.example\/x"}`},
	} {
		st := NewStats(false)
		if got := runJSON(t, m, c.in, st); got != c.want {
			t.Errorf("in   %s\ngot  %s\nwant %s", c.in, got, c.want)
		}
		if n := st.Rewrites(SurfaceJSONEscape); n != 0 {
			t.Errorf("%s: took the re-encode path %d times, it should not have", c.in, n)
		}
	}
}

// TestJSONIdentityStillByteIdentical is test 24 for everything above. The
// carve-out and the sweep both have to be no-ops under an identity map, or the
// guard rail is gone.
func TestJSONIdentityStillByteIdentical(t *testing.T) {
	m := pairMatcher(t, "https://c.example", "https://c.example")
	for _, in := range []string{
		`{"link":"https:\/\/c.example\/x"}`,
		`{"link":"https:\/\/hämeen.fi\/x"}`,
		`{"content":{"rendered":"<a href=\"https:&#47;&#47;c.example&#47;x\">y<\/a>"}}`,
		`{"link":"https://c.example/a","link":"https://c.example/b"}`,
		`{"a":[1,2.50,-0,1e400,null,true],"b":{"c":"😀"}}`,
	} {
		st := NewStats(false)
		if got := runJSON(t, m, in, st); got != in {
			t.Errorf("identity map changed the JSON:\n got %s\nwant %s", got, in)
		}
	}
}

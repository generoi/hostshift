package rewrite

import (
	"strconv"
	"strings"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
)

// A generated round trip for the request direction, which is where rounds 63,
// 64 and 65 each put a variant hostname into the shared database.
//
// The property is §4.3 stated as an equality: **what the app stores after a save
// must be exactly what was in the database before it.** Serve a value, let a
// browser post it back, and the bytes PHP receives have to be the canonical ones
// again — not merely "no variant hostname", which is what the leak checks
// assert, but the original value.
//
// Equality is at the *decoded* level on purpose. The peel splices, so a value
// may come back spelled differently — `%zz` between two rewrites becomes `%25zz`
// — and PHP decodes both to the same bytes. Demanding byte-identical encoding
// would forbid the fix that made the peel work at all; demanding decode-identical
// content is the thing that actually matters to the database.
//
// The grid crosses the spellings a page can carry with the encoders a browser
// can post them in. There is no one urlencoded encoder — `URLSearchParams`, form
// submission and jQuery's `encodeURIComponent`+`%20`→`+` disagree on `!'()~*` —
// and two consecutive rounds shipped a leak by guessing which one was in use.
//
// Measured, by reverting each fix in a copy of the tree: no form-layer peel at
// all fails 12 of these, round 63's "withhold whenever anything changed" fails 3,
// and round 64's byte-identical re-encode guard fails 5 — but only once a stored
// value is *both* percent-spelled and holds a paren, which is the Customizer's
// actual shape and what the first version of this grid was missing.
//
// Re-encoding the whole value rather than splicing it is deliberately *not*
// caught: it is decode-identical, so it does not change what the database holds.
// Splicing is for anything that hashes the raw body, which this property does not
// speak to.

// requestEncoders are the ways a real client spells a form body. Each takes a
// decoded value and returns the field's encoded form.
var requestEncoders = []struct {
	name string
	enc  func(string) string
}{
	{"whatwg-form", formEncode},
	// jQuery, which is what WordPress core posts from admin-ajax.php and the
	// Customizer: encodeURIComponent, then %20 to +. It leaves ! ' ( ) ~ raw.
	{"jquery", func(s string) string {
		out := formEncode(s)
		for _, r := range []struct{ pct, raw string }{
			{"%21", "!"}, {"%27", "'"}, {"%28", "("}, {"%29", ")"}, {"%7E", "~"},
		} {
			out = strings.ReplaceAll(out, r.pct, r.raw)
		}
		return out
	}},
	// A client that spells spaces %20 rather than +, which round 63's guard
	// declined outright.
	{"pct-space", func(s string) string {
		return strings.ReplaceAll(formEncode(s), "+", "%20")
	}},
	// No transport layer at all: the value as-is.
	{"none", func(s string) string { return s }},
}

func TestRequestDirectionRestoresTheStoredValue(t *testing.T) {
	fwd := obfMatcher(t) // canonical -> variant, the response direction
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

	const canon = "https://www.example.fi/x"
	blob := func(v string) string {
		return `a:1:{s:1:"k";s:` + strconv.Itoa(len(v)) + `:"` + v + `";}`
	}
	// The spellings a page can carry an origin in, as they would sit in the
	// database before a save.
	stored := []struct{ name, val string }{
		{"raw", canon},
		{"json-escaped", `https:\/\/www.example.fi\/x`},
		{"percent-encoded", "https%3A%2F%2Fwww.example.fi%2Fx"},
		{"serialized", blob(canon)},
		{"serialized around a percent origin", blob("https%3A%2F%2Fwww.example.fi%2Fx")},
		{"two origins", canon + " and " + canon},
		{"with parens", ".hero{background:url(" + canon + ")}"},
		// The Customizer's actual shape, and the one that separates the encoders:
		// a *percent-spelled* origin, which needs the peel, inside `url()`, whose
		// parens jQuery leaves raw. Round 64's guard declined exactly this.
		{"a percent origin inside url() parens",
			".hero{background:url(https%3A%2F%2Fwww.example.fi%2Fbg.png)}"},
		{"a percent origin beside an apostrophe",
			"it's https%3A%2F%2Fwww.example.fi%2Fx"},
		{"a percent origin beside a tilde and a bang",
			"a ~ b ! c https%3A%2F%2Fwww.example.fi%2Fx"},
		{"with an apostrophe", "it's " + canon},
		{"with a stray percent", "50% off " + canon},
		{"with a plus and a tilde", "a + b ~ c " + canon},
		{"a query parameter carrying an origin",
			"https://www.example.fi/go?u=https%3A%2F%2Fwww.example.fi%2Ftarget&n=1"},
	}

	var bad []string
	checked := 0
	for _, s := range stored {
		// What the response direction serves for this value.
		servedB, _ := fwd.RewriteText([]byte(s.val), SurfaceHTMLAttr, false)
		served := string(servedB)
		if served == s.val {
			t.Errorf("%s: the response direction did not rewrite this, so the "+
				"round trip asserts nothing:\n  %s", s.name, s.val)
			continue
		}
		if strings.Contains(served, "www.example.fi") {
			t.Errorf("%s: the response direction left a canonical origin:\n  %s",
				s.name, served)
			continue
		}
		for _, e := range requestEncoders {
			body := "field=" + e.enc(served)
			got := string(RepairSerializedFields([]byte(body), rw))
			eq := strings.IndexByte(got, '=')
			if eq < 0 {
				bad = append(bad, s.name+" / "+e.name+": field separator lost")
				continue
			}
			dec, _, ok := formDecodeSpans(got[eq+1:])
			if !ok {
				bad = append(bad, s.name+" / "+e.name+": result does not decode")
				continue
			}
			if e.name == "none" {
				// No transport layer, so no decode on the way in either.
				dec = got[eq+1:]
			}
			checked++
			if dec != s.val {
				bad = append(bad, "  "+s.name+" / "+e.name+
					"\n    stored: "+s.val+"\n    served: "+served+
					"\n    posted: "+body+"\n    app got: "+dec)
			}
		}
	}
	if checked == 0 {
		t.Fatal("nothing was checked, so this asserts nothing")
	}
	if len(bad) > 0 {
		t.Errorf("%d of %d round trips did not restore the stored value. Each is a "+
			"save that changes production's database — §4.3, with no undo.\n%s",
			len(bad), checked, strings.Join(bad, "\n"))
	}
	t.Logf("%d round trips restored the stored value exactly", checked)
}

package rewrite

import (
	"strconv"
	"strings"
	"testing"
)

// m12Enc is the five spellings a serialized payload reaches this package in,
// each wrapped in the vehicle that carries it: a raw field, a form body, an
// `esc_attr` attribute, a JSON string, and `esc_attr(wp_json_encode(…))`.
//
// The empty and space-terminated labels in the matrices below are the controls:
// if any of these encoders or the rewrite stub were wrong — a stub that
// substitutes an origin the encoded spelling never contains rewrites nothing,
// and the test then passes on untouched bytes — those rows would fail too.
var m12Enc = map[string]func(string) string{
	"raw":  func(s string) string { return s },
	"form": func(s string) string { return "option_value=" + m12RawURLEncode(s) },
	"attr": func(s string) string { return `<input value="` + escAttrNoDouble(s) + `">` },
	"json": func(s string) string { return `{"blob":` + phpJSONEncode(s) + `}` },
	"jsonattr": func(s string) string {
		return `<div data-settings="` + escAttrNoDouble(phpJSONEncode(s)) + `">y</div>`
	},
}

// m12RawURLEncode is PHP's rawurlencode: everything outside the unreserved set.
func m12RawURLEncode(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&0xf])
	}
	return b.String()
}

// m12Rewrite is the map, in every spelling the host is written in: literally,
// with `json_encode`'s escaped solidus, and percent-encoded.
func m12Rewrite(canon, variant string) func([]byte) []byte {
	sol := func(s string) string { return strings.ReplaceAll(s, "/", `\/`) }
	pct := func(s string) string {
		return strings.NewReplacer(":", "%3A", "/", "%2F").Replace(s)
	}
	return func(b []byte) []byte {
		s := strings.ReplaceAll(string(b), canon, variant)
		s = strings.ReplaceAll(s, sol(canon), sol(variant))
		return []byte(strings.ReplaceAll(s, pct(canon), pct(variant)))
	}
}

// m12Str is `s:LEN:"DATA";`, measuring its own data.
func m12Str(v string) string { return `s:` + strconv.Itoa(len(v)) + `:"` + v + `";` }

// TestAStringPayloadGluedToTheTextBeforeItKeepsAStaleLength. `valueStart` gates
// the scan on the byte *before* a value, and the set it accepts is punctuation
// and whitespace. A letter, a digit or a `]` is not in it, so a payload written
// with no separator between it and the text in front of it is never looked at.
//
// Round 31 set out to close exactly this, and its commit message says so: "with
// fragments repaired where they are found, a label needs no trailing separator.
// Round 30's matrix passed for the wrong reason, because every prefix in it
// ended in a space, a colon or a quote. `x`, `Åtgärd` and `v2` are in it now."
//
// That is true only for a payload whose *children* are separator-preceded. An
// array or an object declares an arity, not a byte count, so when its header is
// skipped the members inside it are still found — after a `{` or a `;` — and
// repaired one at a time, and the arity that was never re-emitted was never
// wrong. `TestANestedPayloadBehindALabel` uses an array, which is why the three
// new labels pass there.
//
// A string declares a byte count. When its header is skipped there is no child
// to fall back on: nothing is repaired, `repairField` reports `found == false`,
// and the whole field is handed to `rw` — which rewrites the host and re-emits
// no length at all. `unserialize()` returns `false` on the value that comes back.
//
// And it is invisible. `BrokenSerialized` gates on the same `valueStart`, so it
// reports zero on the served bytes; the count is host-independent, so the
// canonical page reports zero too and `internal/corpus/diff.go` subtracts one
// from the other. GREEN, on bytes PHP refuses — the shape rounds 28 to 31 each
// turned out to be.
//
// Verified against PHP 8.4 on the served bytes: for the label `v2`, the outer
// array unserializes, and `unserialize(substr($a["f"], 2))` is `false`, against
// the un-labelled control where it is the URL.
func TestAStringPayloadGluedToTheTextBeforeItKeepsAStaleLength(t *testing.T) {
	canon, variant := "https://mz32a.ddev.site", "https://wt-a--mz32a.ddev.site"
	rw := m12Rewrite(canon, variant)

	// An options row whose first field holds a label and then a payload — the
	// shape `TestANestedPayloadBehindALabel` uses, with a string where it uses
	// an array.
	blob := func(host, label string) string {
		inner := label + m12Str(host+"/inner/x")
		return `a:2:{` + m12Str("f") + m12Str(inner) + m12Str("home") + m12Str(host) + `}`
	}
	labels := map[string]string{
		// Controls: a separator the gate already accepts.
		"none": "", "space": " ", "prose": "Obs: ", "quote": `say "hi" `,
		// The three labels round 31 added, and a bracket. None of these bytes
		// is in the separator set, so the payload behind them is invisible.
		"bare letter": "x", "word": "Åtgärd", "digit": "v2", "bracket": "]",
	}
	for sname, enc := range m12Enc {
		for lname, label := range labels {
			in := enc(blob(canon, label))
			want := enc(blob(variant, label))
			got := string(RepairSerialized([]byte(in), rw))
			if got != want {
				t.Errorf("%s / %s:\n got  %s\n want %s", sname, lname, got, want)
			}
			if n := BrokenSerialized([]byte(got)); n != 0 {
				t.Errorf("%s / %s: served %d value(s) PHP refuses:\n %s",
					sname, lname, n, got)
			}
		}
	}
}

// TestAFragmentRepairedUnderASkippedHeaderBreaksItsEnclosingLength. The sharper
// half of the same defect, and the one round 31 introduced rather than left.
//
// The payload here is `serialize(serialize(['link' => $url]))` — WordPress's own
// double-serialization, one `maybe_serialize` inside another — sitting behind a
// label. The outer `s:LEN:"…"` header is glued to the label, so `valueStart`
// skips it. The array *inside* it is preceded by a `"`, which is a separator, so
// the nested walk finds it, repairs its members and re-emits their lengths.
//
// Before round 31 that could not happen: `runsToTheEnd` refused a nested value
// with anything after it, and the residue here is the enclosing `";`, so the
// whole field declined — both lengths were left alone and the two directions
// round-tripped. Removing that check made the walk repair a *fragment* whose
// enclosing structure it never parsed: `s:LEN:` is now stale by exactly the
// number of bytes the map added inside it, on a value that arrived correct.
//
// Verified against PHP 8.4 on the served bytes: the outer array unserializes,
// and `unserialize(substr($a["f"], 2))` on the field is `false` where the
// un-labelled control returns the array.
func TestAFragmentRepairedUnderASkippedHeaderBreaksItsEnclosingLength(t *testing.T) {
	canon, variant := "https://mz32b.ddev.site", "https://wt-a--mz32b.ddev.site"
	rw := m12Rewrite(canon, variant)

	blob := func(host, label string) string {
		payload := `a:1:{` + m12Str("link") + m12Str(host+"/inner/x") + `}`
		inner := label + m12Str(payload)
		return `a:2:{` + m12Str("f") + m12Str(inner) + m12Str("home") + m12Str(host) + `}`
	}
	for sname, enc := range m12Enc {
		for lname, label := range map[string]string{
			"none": "", "space": " ", // controls
			"digit": "v2", "bare letter": "x",
		} {
			in := enc(blob(canon, label))
			want := enc(blob(variant, label))
			got := string(RepairSerialized([]byte(in), rw))
			if got != want {
				t.Errorf("%s / %s:\n got  %s\n want %s", sname, lname, got, want)
			}
			if n := BrokenSerialized([]byte(got)); n != 0 {
				t.Errorf("%s / %s: served %d value(s) PHP refuses:\n %s",
					sname, lname, n, got)
			}
		}
	}
}

// The detector half, stated on bytes alone so it holds however the stale length
// got there.
//
// `BrokenSerialized` is PLAN §7's answer to five rounds of silent corruption: a
// parse assertion on the served bytes, so a wrong length is counted rather than
// argued about. It walks with `valueStart`, whose position half skips a value
// glued to the text in front of it.
//
// Inside a string it now sees them, because the nested walk gates on shape
// alone — and inside a string is where a real page carries one: a `wp_options`
// row, an ACF field, an attribute value holding a blob behind its label.
//
// At the top level it does not, and cannot. `Åtgärds:28:"…";` and the
// `https://x/s:3:"a"` in an ordinary URL path are the same thing to this walk:
// a header after a non-separator that commits and then fails to close. Counting
// one counts the other, and a check that goes red on a healthy page is the
// failure this file's history says is worse. The repair covers the gap from the
// other side — a glued value that parses with nothing after it is repaired, so
// hostshift does not produce these bytes; what stays invisible is a stale
// length that arrived that way.
func TestTheDetectorSeesAGluedValueInsideAString(t *testing.T) {
	url := "https://wt-a--mz32c.ddev.site/inner/x"
	stale := `s:` + strconv.Itoa(len(url)-9) + `:"` + url + `";`
	// The control: the same stale value with nothing in front of it.
	if n := BrokenSerialized([]byte(stale)); n == 0 {
		t.Fatalf("the control was not seen either, so this fixture proves nothing:\n %s", stale)
	}
	for _, label := range []string{"", "x", "v2", "Åtgärd", "]"} {
		data := label + stale
		field := `a:1:{s:1:"f";s:` + strconv.Itoa(len(data)) + `:"` + data + `";}`
		if n := BrokenSerialized([]byte(field)); n == 0 {
			t.Errorf("a length that does not describe its data was reported clean "+
				"because %q stands in front of it:\n %s", label, field)
		}
	}
}

// And the documented blind spot, pinned so it is a decision rather than a
// surprise: bare at the top level, a glued stale value is not counted. If this
// starts failing, the walk has become able to tell it from a URL path holding
// `s:3:"a"` — which would be an improvement worth taking, and the sibling test
// in diff_test.go says what must keep passing alongside it.
func TestAGluedValueBareAtTheTopLevelIsNotCounted(t *testing.T) {
	url := "https://wt-a--mz32c.ddev.site/inner/x"
	stale := `s:` + strconv.Itoa(len(url)-9) + `:"` + url + `";`
	for _, label := range []string{"x", "v2", "Åtgärd", "]"} {
		if n := BrokenSerialized([]byte(label + stale)); n != 0 {
			t.Errorf("counted %d for %q — see the comment above; if this is real, "+
				"check TestEscapedSerializedContentIsNotBroken still passes", n, label)
		}
	}
}

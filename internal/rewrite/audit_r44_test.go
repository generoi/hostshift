package rewrite

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
)

// Round 44, on 7cb756c ("Ask the size question in bytes; stop reports asserting
// what they skipped").

// pctAll percent-encodes the way a form encoder does, so a serialized value
// appears in the spelling `options.php` and every JSON-in-a-form field actually
// send: `s%3A51%3A%22…`. It is the spelling 7cb756c added the `pctQuote` opener
// for.
func pctAll(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '.' || c == '-' || c == '_' || c == '~' {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

// TestTheCustomCSSRowStillComesHomeWhenTheFormEncodedIt.
//
// This is TestAnOrdinaryCustomCSSOptionSurvivesARoundTrip — the measured
// `custom_css` failure that the whole "occupies its field" rule exists to stop
// — written in the percent-encoded spelling. It is the same bytes, the same
// stale length, the same false `";}` boundary; only the transport differs.
//
// 7cb756c widened `occupiesItsField`'s trailing scan to skip encoded whitespace
// in *two* openers:
//
//	if open == ownField || open == pctQuote {
//		if b[i] == '+' { continue }
//		if b[i] == '%' && i+2 < len(b) && pctWhitespace(b[i+1], b[i+2]) { i += 2; continue }
//	}
//
// The round-43 defect it was written for is the `ownField` half: a value posted
// as `o=<blob>%0A`, where the trailing newline is not part of any field's
// closing delimiter. The `pctQuote` half is different in kind, and it walks
// straight into the trap the rule was built from.
//
// In a `%22`-quoted field, the value's own internal quotes are `%22` too. The
// residue of a parse that stopped short inside a string is, in this file's own
// words, "the tail of the true string" — for `custom_css` that tail is
// `\n\n\n";}`, which percent-encodes to `%0A%0A%0A%22%3B%7D`. Before this
// commit the scan met `%0A`, asked `pctIs(b, i, '"')`, got no, and declined.
// Now it skips the three encoded newlines as whitespace and meets `%22` — which
// the `pctQuote` arm accepts as the field's closing quote, though it is the
// string's own. The parse is believed, the prefix length is re-emitted, and the
// row loses the six bytes past the false boundary.
//
// Six bytes per view-and-save, parsing cleanly every time, on the request
// direction: a write into the shared production database with no undo. That is
// §4.3's failure, restored for every value that reaches the proxy through a
// percent-encoded quote — which is the exact shape `pctQuote` was added for.
//
// The literal spelling of the same value still comes home (asserted below), so
// the two transports now disagree about the same bytes.
func TestTheCustomCSSRowStillComesHomeWhenTheFormEncodedIt(t *testing.T) {
	canon, variant := "https://jz25.ddev.site", "https://wt-a--jz25.ddev.site"
	css := `.hero{background:url(` + canon + `/wp-content/uploads/bg.png);` +
		`font-family:"Inter";}` + "\n\n\n"
	seed := `a:1:{s:10:"custom_css";s:` + strconv.Itoa(len(css)) + `:"` + css + `";}`
	assertEveryLength(t, seed)

	// What a streamed arm serves: hosts swapped, no length re-emitted. The
	// declared length now lands on the `"` of `font-family:"Inter"`, so the walk
	// can close the string there, close the array on the next `}`, and be
	// structurally complete six bytes short.
	wire := strings.ReplaceAll(seed, canon, variant)
	stale := strings.Index(wire, `s:`+strconv.Itoa(len(css))+`:"`) + len(`s:`+strconv.Itoa(len(css))+`:"`)
	if wire[stale+len(css)] != '"' {
		t.Fatalf("fixture: no quote at the stale offset; it has %q", wire[stale+len(css)])
	}

	// The real reverse matcher, not a hand-rolled substitution: the percent
	// spelling is in its token set, which is what makes `options.php` work at
	// all, and using it means this test cannot be dismissed as an artefact of
	// the stand-in.
	rev := reverseRewriter(t, canon, variant)

	// Premise: in its literal spelling this row comes home, so anything that
	// follows is about the encoding and not about the value.
	if got := string(RepairSerialized([]byte(wire), func(b []byte) []byte {
		return []byte(strings.ReplaceAll(string(b), variant, canon))
	})); got != seed {
		t.Fatalf("fixture: the literal spelling does not round-trip either:\n%q", got)
	}

	// The same row inside a percent-encoded field: a JSON string posted through
	// a form, which is the shape `pctQuote` exists for. This is the request
	// direction — RepairSerializedFields is what the proxy runs on an
	// `application/x-www-form-urlencoded` body (internal/proxy/proxy.go).
	body := "o=%22" + pctAll(wire) + "%22"
	back := string(RepairSerializedFields([]byte(body), rev))
	want := "o=%22" + pctAll(seed) + "%22"
	if back != want {
		t.Errorf("the row did not come home through a percent-encoded quote:\n"+
			" got  %s\n want %s", back, want)
	}
}

// TestWhatPHPReadsBackIsTheWholeCSS names the harm in the row's own terms.
//
// The buffer above is not corrupt in the sense `BrokenSerialized` asks about —
// the re-emitted `s:89:` describes its 89 bytes exactly, so it parses cleanly.
// That is the whole reason the original went unreported for five rounds. What
// changed is what `unserialize()` hands WordPress: the value stored was 95
// bytes of CSS, and the value read back is the 89 before the false boundary.
// The Customizer then re-serialises what it was given, and the six bytes past
// it — `Inter";}` closing the font stack, and the newlines — are gone from the
// shared database for good.
func TestWhatPHPReadsBackIsTheWholeCSS(t *testing.T) {
	canon, variant := "https://jz25.ddev.site", "https://wt-a--jz25.ddev.site"
	css := `.hero{background:url(` + canon + `/wp-content/uploads/bg.png);` +
		`font-family:"Inter";}` + "\n\n\n"
	seed := `a:1:{s:10:"custom_css";s:` + strconv.Itoa(len(css)) + `:"` + css + `";}`
	wire := strings.ReplaceAll(seed, canon, variant)

	back := string(RepairSerializedFields([]byte("o=%22"+pctAll(wire)+"%22"),
		reverseRewriter(t, canon, variant)))

	// What unserialize() reads: the declared length of the `custom_css` value,
	// and that many bytes after the opening quote.
	dec := pctDecode(t, strings.TrimSuffix(strings.TrimPrefix(back, "o=%22"), "%22"))
	head := `s:10:"custom_css";s:`
	j := strings.Index(dec, head)
	if j < 0 {
		t.Fatalf("fixture: no custom_css member in\n%s", dec)
	}
	j += len(head)
	k := strings.Index(dec[j:], `:"`)
	n, err := strconv.Atoi(dec[j : j+k])
	if err != nil {
		t.Fatal(err)
	}
	got := dec[j+k+2 : j+k+2+n]
	if got != css {
		t.Errorf("PHP reads back %d bytes of a %d-byte option, and the Customizer\n"+
			"saves what it was given:\n got  %q\n want %q", len(got), len(css), got, css)
	}
}

// reverseRewriter is the proxy's own request-direction matcher for one site,
// which is what internal/proxy hands RepairSerializedFields for an
// `application/x-www-form-urlencoded` body.
func reverseRewriter(t *testing.T, canonical, variant string) func([]byte) []byte {
	t.Helper()
	c, err := origin.Parse(canonical)
	if err != nil {
		t.Fatal(err)
	}
	v, err := origin.Parse(variant)
	if err != nil {
		t.Fatal(err)
	}
	m, err := origin.NewMap([]origin.Site{{Name: "s", Canonical: c, Variant: v}})
	if err != nil {
		t.Fatal(err)
	}
	rev := m.Reverse()
	return func(b []byte) []byte {
		out, _ := rev.Rewrite(b, SurfaceRequestBody, false)
		return out
	}
}

func pctDecode(t *testing.T, s string) string {
	t.Helper()
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			var v int
			if _, err := fmt.Sscanf(s[i+1:i+3], "%02x", &v); err == nil {
				b.WriteByte(byte(v))
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

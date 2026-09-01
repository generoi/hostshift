package rewrite

import (
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// Round 43. A serialized value posted as a urlencoded field, with a trailing
// newline, went into the database with a stale length.
//
// `onlySpaceAfter` decides whether the candidate occupies its whole field, and
// it knew only the literal spellings of whitespace. Through a form the trailing
// newline is `%0A`, which read as "something else follows" — so the repair
// declined, and a decline is not neutral: the generic rewriter still replaces
// the host and re-emits no length. `s:30:` then describes a string that is no
// longer 30 bytes, and PHP returns false for it or truncates.
//
// This is the request direction, so it is a write into the shared production
// database with no undo — and it is masked on the way back, because read through
// the proxy the length matches the variant spelling again. It breaks only for
// whatever reads the row without the proxy in front of it.
func TestATrailingEncodedNewlineDoesNotDefeatTheRepair(t *testing.T) {
	const variant = "https://wt-a--acme.ddev.site/x"
	const canonical = "https://www.acme.fi/x"
	// Both spellings, as the real rewriter does: inside a urlencoded field the
	// URL's own delimiters are percent-encoded, so a rewriter that only knew the
	// literal form would never fire and the test would prove nothing.
	pct := func(u string) string {
		return strings.NewReplacer(":", "%3A", "/", "%2F").Replace(u)
	}
	rw := func(b []byte) []byte {
		s := strings.ReplaceAll(string(b), pct(variant), pct(canonical))
		return []byte(strings.ReplaceAll(s, variant, canonical))
	}
	value := `a:1:{s:3:"url";s:` + strconv.Itoa(len(variant)) + `:"` + variant + `";}`
	want := `s:` + strconv.Itoa(len(canonical)) + `:"` + canonical + `"`

	// Every spelling of "and then nothing but whitespace" a form can produce.
	for _, trailer := range []string{"", "\n", "%0A", "%0a", "%20", "+", "%09%0D"} {
		body := "o=" + url.QueryEscape(value) + trailer
		out := string(RepairSerializedFields([]byte(body), rw))

		// Compare what PHP would be handed: the field, percent-decoded.
		field := strings.TrimPrefix(out, "o=")
		if i := strings.IndexAny(field, "&"); i >= 0 {
			field = field[:i]
		}
		got, err := url.QueryUnescape(field)
		if err != nil {
			t.Errorf("trailer %q: field is not decodable: %v", trailer, err)
			continue
		}
		if !strings.Contains(got, canonical) {
			t.Errorf("trailer %q: the host was not mapped back at all:\n%s", trailer, got)
			continue
		}
		if !strings.Contains(got, want) {
			t.Errorf("trailer %q: length not re-emitted for the rewritten string\n got: %s\nwant substring: %s",
				trailer, got, want)
		}
	}
}

// TestALeadingEncodedNewlineDoesNotDefeatTheRepair: the mirror of the test
// above, and the same defect on the other side of the same function.
//
// `occupiesItsField` scans backwards for the delimiter that opened the value and
// skipped leading whitespace — the raw bytes only. No browser sends those: a
// urlencoded body spells a leading newline `%0D%0A` and a leading space `+` or
// `%20`. So an option whose value begins with a newline, which is what a
// `<textarea>` produces, failed the gate on the way *in*: the repair declined,
// the generic rewriter replaced the host and left the old length, and the row
// went to the shared database with `s:35:` over 30 bytes. PHP returns false.
//
// The response direction repaired the same value correctly on the way out, so a
// developer sees valid data in the browser and posts back a broken row.
func TestALeadingEncodedNewlineDoesNotDefeatTheRepair(t *testing.T) {
	const variant = "https://wt-a--acme.ddev.site/x"
	const canonical = "https://www.acme.fi/x"
	pct := func(u string) string {
		return strings.NewReplacer(":", "%3A", "/", "%2F").Replace(u)
	}
	rw := func(b []byte) []byte {
		s := strings.ReplaceAll(string(b), pct(variant), pct(canonical))
		return []byte(strings.ReplaceAll(s, variant, canonical))
	}
	value := `a:1:{s:3:"url";s:` + strconv.Itoa(len(variant)) + `:"` + variant + `";}`
	want := `s:` + strconv.Itoa(len(canonical)) + `:"` + canonical + `"`

	for _, lead := range []string{"", " ", "+", "%20", "%09", "%0A", "%0D%0A"} {
		body := "data=" + lead + url.QueryEscape(value)
		out := string(RepairSerializedFields([]byte(body), rw))

		field := strings.TrimPrefix(out, "data=")
		if i := strings.IndexAny(field, "&"); i >= 0 {
			field = field[:i]
		}
		got, err := url.QueryUnescape(field)
		if err != nil {
			t.Errorf("lead %q: field is not decodable: %v", lead, err)
			continue
		}
		if !strings.Contains(got, canonical) {
			t.Errorf("lead %q: the host was not mapped back at all:\n%s", lead, got)
			continue
		}
		if !strings.Contains(got, want) {
			t.Errorf("lead %q: length not re-emitted\n got: %s\nwant substring: %s",
				lead, got, want)
		}
	}
}

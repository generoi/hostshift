package rewrite

import (
	"strings"
	"testing"
)

// An unreadable value the rewrite never touched is not a page to look at.
//
// `UnreadSerialized` is documented as asking "did we change bytes we could not
// account for", and the note it raises says "a serialized value here was
// rewritten in an encoding this build cannot read, so no length was re-emitted".
// Both are claims about one value. The implementation makes neither: the only
// "was it touched" test is `bytes.Equal(rw(b), b)` over the *whole page*, and
// the loop then returns true for the first header-shaped span anywhere in that
// page that no spelling parses — whether or not the rewrite came near it.
//
// So any page that carries a serialized value in a composition outside the nine
// spellings goes red as soon as anything else on it is rewritten, and a
// canonical `<link>` is on every WordPress page there is. That is the same
// failure the commit before this one was written to close — a signal red on
// pages that are fine is a signal a developer learns to ignore — reintroduced
// with a different gate.
//
// The spelling here is the one this file's own test already names as unread:
// JSON_HEX_QUOT carried inside a second JSON string, which is what a nested
// block attribute produces. The blob holds no origin at all, so nothing in it
// can have been rewritten.
func TestAnUntouchedUnreadableValueIsNotAPageToLookAt(t *testing.T) {
	canon, variant := "www.mz38a.test", "v38.ddev.site"
	rw := func(b []byte) []byte {
		return []byte(strings.ReplaceAll(string(b), canon, variant))
	}
	blob := `a:2:{s:5:"label";s:5:"Hello";s:4:"size";s:5:"large";}`
	wire := strings.ReplaceAll(blob, `"`, `\\u0022`)
	page := `<link rel="canonical" href="https://` + canon + `/x/">` +
		`<div data-wp-context="` + wire + `">x</div>`

	got := string(rw([]byte(page)))
	// Both halves of the premise, so this cannot pass vacuously: the page was
	// rewritten somewhere, and the serialized value was not one of the places.
	if got == page {
		t.Fatalf("fixture: the rewrite changed nothing, so the gate never opens")
	}
	if !strings.Contains(got, wire) {
		t.Fatalf("fixture: the rewrite touched the blob, so it is a real report:\n %s", got)
	}

	if UnreadSerialized([]byte(page), rw) {
		t.Errorf("a page whose serialized value the rewrite never touched was "+
			"reported as one rewritten in an unreadable encoding:\n %s", page)
	}
}

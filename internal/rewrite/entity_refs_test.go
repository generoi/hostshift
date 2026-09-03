package rewrite

import (
	"fmt"
	"html"
	"testing"
)

// Every entry in urlNamedRefs must decode the way a browser decodes it.
//
// "hyphen" did not: HTML5 defines &hyphen; as U+2010 HYPHEN, and this table
// had it as U+002D. A decoded value is spliced back whole whenever any origin
// inside it rewrote, so that silently rewrote a neighbouring hostname from one
// that does not resolve on production into one that does — content inert on
// production, made live by the rewriter, which is test 28 exactly.
//
// html.UnescapeString is the HTML5 named table; comparing against it is the
// only way this stays honest as entries are added.
func TestNamedRefsMatchHTML5(t *testing.T) {
	for name, want := range urlNamedRefs {
		got := html.UnescapeString(fmt.Sprintf("&%s;", name))
		if got != string(rune(want)) {
			t.Errorf("&%s; decodes to %q (%U) in HTML5, but this table says %q",
				name, got, []rune(got), string(rune(want)))
		}
	}
}

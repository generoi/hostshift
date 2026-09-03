package rewrite

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
)

// Round 50, on fe69d42.

func r50Map(t *testing.T, canon, variant string) *origin.Map {
	t.Helper()
	m, err := origin.NewMap([]origin.Site{{
		Name:      "main",
		Canonical: origin.MustParse(canon),
		Variant:   origin.MustParse(variant),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func r50HTML(t *testing.T, m *origin.Matcher, in string) string {
	t.Helper()
	out, err := io.ReadAll(NewResponseBody(bytes.NewReader([]byte(in)), m, nil, Options{}))
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// TestR50TheSchemeArmSplicesARawSeparatorIntoAnEncodedValue.
//
// `locateHostIn`'s `needScheme` arm — the line fe69d42 rewrote — builds its
// replacement as `to.Scheme + "://" + to.DisplayHostPort()`. The `://` is
// literal, and the arm replaces the source bytes *from the scheme through the
// port*, whatever they were. Where the separator was written percent-encoded,
// those source bytes are `%3A%2F%2F`, and a raw `://` goes in over them.
//
// matcher.go already knows this is wrong: `encoding.schemeSep()` exists so that
// the byte matcher emits `%3A%2F%2F` for a percent-encoded match and `:\/\/` for
// a JSON-escaped one. The locator has no such notion, so the two spellings of
// the same URL disagree — the model error this file's own comments call a
// defect wherever else it appears.
//
// The consequence is not cosmetic. `%2F` inside a path segment is what keeps it
// one segment. Turning it into a real `/` splits one segment into three:
//
//	/go/http%3A%2F%2F%2Fwww.example.fi%2Fbar   ->  /go/https://<variant>%2Fbar
//
// The link 404s in the worktree where it worked on production, and — because
// `HostLeaksBackCounted` runs the same arm over the request line, the request
// path and every request body — a save writes `/go/https://www.example.fi%2Fbar`
// into the shared production database, where it 404s for real visitors. That is
// §4.3's one failure, reached without an IDN, without a mixed-scheme map and
// with nothing more exotic than the shapes this file's opening comment lists as
// the reason the locator exists.
//
// The fix is the one matcher.go already made: spell the separator in the
// encoding the match site used. Reading it off the source width of the scheme's
// own `:` (three bytes means `%3A`, and a two-byte slash after it means `\/`)
// costs no new view and leaves TestAllocationStaysBounded at 382x/172x/118x/104x.
func TestR50TheSchemeArmSplicesARawSeparatorIntoAnEncodedValue(t *testing.T) {
	m := r50Map(t, "https://www.example.fi", "https://wt-a--example.ddev.site")

	// Every one of these writes `http:` where the variant is https, so
	// `needScheme` fires; and every one is a shape the byte matcher's token set
	// does not carry, so the locator is what rewrites it.
	for _, in := range []string{
		`<a href="/go/http%3A%2F%2F%2Fwww.example.fi%2Fbar">x</a>`,
		`<a href="/go/http%3Awww.example.fi%2Fbar">x</a>`,
		`<a href="/go/http%3A%2F%2Fu@www.example.fi%2Fbar">x</a>`,
		`<a href="/go/http%3A%5C%2F%5C%2Fwww.example.fi%2Fbar">x</a>`,
	} {
		out := r50HTML(t, m.Forward(), in)
		if !strings.Contains(out, "wt-a--example.ddev.site") {
			t.Errorf("not rewritten at all, so this fixture no longer tests the arm:\n  %s", in)
			continue
		}
		if n, w := strings.Count(in, "/"), strings.Count(out, "/"); n != w {
			t.Errorf("the scheme arm spliced a raw separator over an encoded one, "+
				"turning %d path separators into %d:\n  in:  %s\n  out: %s", n, w, in, out)
		}
	}
}

// TestR50OctalIsTheOneJSEscapeTheViewNoLongerReads.
//
// fe69d42 took legacy octal back out of `jsEscAt`, together with the
// `hasJSONEsc` scan that armed the whole view on `\` before a digit. The scan
// had to go: `\3a` is a CSS colon, and gating on it measured 287x the body.
//
// But the two are separable, and only the gate was expensive. `hasJSONEsc` still
// arms on `\u` and `\x`, and a WordPress inline script carries one of those in
// essentially every `wp_localize_script` blob and every block delimiter. With
// the view already built, decoding `\056` inside it costs nothing at all —
// TestAllocationStaysBounded stays at 382x/172x/118x/104x with the arm restored.
//
// So today the same URL is rewritten when it is spelled `\x2e` or `\u{2e}` and
// reaches the browser intact when it is spelled `\056`, on Tier 1, with the
// JavaScript parser decoding all three identically. Round 48 read it; round 49
// stopped.
//
// The pure-octal value — a script with no `\u` and no `\x` anywhere — stays
// unread, and closing that one does need the gate this test does not ask for.
func TestR50OctalIsTheOneJSEscapeTheViewNoLongerReads(t *testing.T) {
	// The variant shares no label with the canonical, so "example" surviving in
	// the output can only be the canonical.
	m := r50Map(t, "https://www.example.fi", "https://wt-a--site.ddev.site")

	// The three spellings of the same two dots, each beside the `\u002d` that
	// Gutenberg writes into every block delimiter and every `--` it serialises.
	for _, in := range []string{
		`<script>var a="\u002d";fetch("https://www\x2eexample\x2efi/a")</script>`,
		`<script>var a="\u002d";fetch("https://www\u{2e}example\u{2e}fi/a")</script>`,
		`<script>var a="\u002d";fetch("https://www\056example\056fi/a")</script>`,
	} {
		if out := r50HTML(t, m.Forward(), in); strings.Contains(out, "example") {
			t.Errorf("a dereferenceable production origin reached the browser:\n  in:  %s\n  out: %s", in, out)
		}
	}

	if out := r50HTML(t, m.Forward(), `<script>fetch("https://www\056example\056fi/a")</script>`); strings.Contains(out, "example") {
		t.Logf("still open, and not what this test asks for: with no `\\u` or `\\x` " +
			"in the value the JSON view is never built, so a purely octal-escaped " +
			"origin is unread. Closing that needs the gate fe69d42 removed for cost.")
	}
}

// TestR50TheFoldedHostAnchorHasNoTestOfItsOwn.
//
// This one passes at HEAD. It is here because the thing it asserts was
// revertible in silence: deleting `!tokenBoundary(n.b, i)` from
// `foldedHostLeak`'s skip condition leaves `go test ./...` green, and the
// comment three lines above it says what that costs —
//
//	Without it this walked every `//` in the buffer, so
//	`https://cdn.other/p//ｗｗｗ.example.fi/q` — where the run is a path
//	separator, not an authority — had its path segment rewritten, while the
//	plain ASCII spelling of the same URL was correctly left alone.
//
// Two spellings of one URL disagreeing is the model error the oracle's second
// half calls a false positive. The ASCII half is pinned by
// `slashRunStarts`'s own anchor and by the oracle; the folded half was not
// pinned by anything.
func TestR50TheFoldedHostAnchorHasNoTestOfItsOwn(t *testing.T) {
	m := r50Map(t, "https://www.example.fi", "https://wt-a--example.ddev.site")

	// `//` after a path segment is a path separator on every surface, whether
	// the label that follows is ASCII or folds onto the canonical.
	for _, in := range []string{
		`<p>https://cdn.other/p//ｗｗｗ.example.fi/q</p>`,
		`<a href="https://cdn.other/p//ｗｗｗ.example.fi/q">x</a>`,
		`<script>u="https://cdn.other/p//ｗｗｗ.example.fi/q"</script>`,
		`<!-- https://cdn.other/p//ｗｗｗ.example.fi/q -->`,
	} {
		ascii := strings.ReplaceAll(in, "ｗｗｗ", "www")
		if got := r50HTML(t, m.Forward(), ascii); got != ascii {
			t.Fatalf("the ASCII half of the pair moved, so this fixture is stale:\n  %s\n  %s", ascii, got)
		}
		if got := r50HTML(t, m.Forward(), in); got != in {
			t.Errorf("a path segment was rewritten as an authority, and the plain "+
				"ASCII spelling of the same URL was not:\n  in:  %s\n  out: %s", in, got)
		}
	}
}

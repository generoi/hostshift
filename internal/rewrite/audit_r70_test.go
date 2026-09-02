package rewrite

import (
	"strings"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
)

// A bare origin at the end of a line was left alone, in both directions.
//
// `removedEscLen` treats tab, LF and CR as characters the URL parser deletes, and
// `hostTerminated` walks past them before asking whether the host ended. That is
// right for a *value*: `href="https://www.example.fi&#10;x"` really is the host
// `www.example.fix` and must not be rewritten. It is wrong for prose, where a
// newline is a hard boundary — a text node, a `<pre>`, a decoded JSON string, a
// multipart part body, a `text/plain` body. There the walk joined the lines,
// decided the host was `…example.finext`, found no map entry and declined.
//
// Round 70 measured both halves on a real WordPress: five variant hostnames
// written into a shared database through ordinary admin saves (`options.php`
// posted as multipart, a block-editor REST save, a REST widget create), and a
// production origin served to the browser inside a `<pre>` block and in
// site-health's copy-to-clipboard attribute. `check` exited 0 and `diff` printed
// GREEN on the leaking page, because both ask this same code.
//
// The trigger is narrow and worth stating: a *bare* origin — no path — then a run
// of control bytes, then a byte that can continue a hostname. A path, a `<`, a
// space, or end-of-buffer all terminated correctly, which is why this survived
// ten rounds of auditing.
func TestR70ABareOriginAtALineEndIsRewrittenInProse(t *testing.T) {
	m := obfMatcher(t)
	const canon = "https://www.example.fi"
	const variant = "https://wt-a--example.ddev.site"
	for _, c := range []struct{ name, in, want string }{
		{"newline then a word", "a " + canon + "\nnext", "a " + variant + "\nnext"},
		{"CRLF then a word", "a " + canon + "\r\nnext", "a " + variant + "\r\nnext"},
		{"newline then a digit", "a " + canon + "\n2nd", "a " + variant + "\n2nd"},
		{"newline then a hyphen", "a " + canon + "\n-x", "a " + variant + "\n-x"},
		{"newline then a dot", "a " + canon + "\n.x", "a " + variant + "\n.x"},
		{"two origins on adjacent lines", canon + "\n" + canon + "/x",
			variant + "\n" + variant + "/x"},
		{"a bare CR", "a " + canon + "\rnext", "a " + variant + "\rnext"},
		// These already worked, and are here so a fix cannot quietly narrow them.
		{"a path terminates the host", "a " + canon + "/x\nnext", "a " + variant + "/x\nnext"},
		{"a tag terminates it", "a " + canon + "\n<b>", "a " + variant + "\n<b>"},
		{"a space terminates it", "a " + canon + "\n next", "a " + variant + "\n next"},
		{"end of buffer", "a " + canon, "a " + variant},
	} {
		t.Run(c.name, func(t *testing.T) {
			out, _ := m.RewriteText([]byte(c.in), SurfaceText, false)
			if string(out) != c.want {
				t.Errorf("a bare origin at a line end was left alone — outward it "+
					"reaches the browser, inward it reaches the shared database\n"+
					"  in:   %q\n  got:  %q\n  want: %q", c.in, string(out), c.want)
			}
		})
	}
}

// A control keeps joining wherever something will parse the buffer as a URL.
//
// `https://www.example.fi<TAB>x` is the host `www.example.fix` to a URL parser,
// which is §5.5's case. Round 70 drew the line at value-vs-prose and kept a tab
// joining *everywhere*, on the reasoning that a raw tab is legal inside a
// JavaScript string. Round 71 showed that reasoning belongs to the script
// surface and not to prose: a literal tab between a bare origin and a word in a
// text node is an ordinary column separator, `wpautop` leaves it alone so it
// does not even need a `<pre>`, and the origin before it was shipped to the
// browser and written into the database. The line is drawn by surface now — see
// origin.surfaceJoinsControls — so this case moved to TestR71B and what is left
// here is the half that must keep joining.
func TestR70AControlThatTheURLParserRemovesStillJoins(t *testing.T) {
	m := obfMatcher(t)
	const canon = "https://www.example.fi"
	for _, c := range []struct {
		name, in string
		value    bool
	}{
		{"a tab in a value", canon + "\tx", true},
		{"a newline in a value", canon + "\nx", true},
		{"a CR in a value", canon + "\rx", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			var out []byte
			if c.value {
				out, _ = m.Rewrite([]byte(c.in), SurfaceHTMLAttr, false)
			} else {
				out, _ = m.RewriteText([]byte(c.in), SurfaceText, false)
			}
			if string(out) != c.in {
				t.Errorf("this is a host the URL parser joins across the control, so "+
					"it is not the mapped origin and rewriting it changes a value no "+
					"browser resolves\n  in:  %q\n  out: %q", c.in, string(out))
			}
		})
	}
}

// And the request direction, which is where the measured damage was.
func TestR70ALineEndOriginComesHome(t *testing.T) {
	rev, err := origin.NewMatcher([]origin.Pair{{
		Canonical: origin.MustParse("https://wt-a--example.ddev.site"),
		Variant:   origin.MustParse("https://www.example.fi"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	// The shape round 70 read out of wp_options: a textarea posted as multipart,
	// three URLs, of which only the one with a path came home.
	body := "Read more:\nhttps://wt-a--example.ddev.site\nand " +
		"https://wt-a--example.ddev.site/deal"
	out, _ := rev.RewriteText([]byte(body), SurfaceRequestBody, false)
	if strings.Contains(string(out), "wt-a--example.ddev.site") {
		t.Errorf("a variant hostname reached the shared database — §4.3, no undo\n"+
			"  in:  %q\n  out: %q", body, string(out))
	}
}

// A JSON string that begins with a URL and keeps going is still prose.
//
// `joinsControlsIn` asks a JSON string about itself, because round 54's case (a
// field holding one URL) and round 71's (the block editor posting a document)
// arrive on the same surface needing opposite answers. The discriminant is a
// literal space — and the round-71 fixture happens to begin with `<!-- wp:code`,
// so it is decided by the scheme test alone and never exercises the space test.
// A caption or an excerpt that opens with a link does exercise it, and without it
// the round-71 leak reopens for exactly those values.
func TestR70AJSONDocumentThatOpensWithAURLIsProse(t *testing.T) {
	rev, err := origin.NewMatcher([]origin.Pair{{
		Canonical: origin.MustParse("https://wt-a--example.ddev.site"),
		Variant:   origin.MustParse("https://www.example.fi"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	// Opens with the origin, ends the line on it, and carries a space later on.
	// The space is what makes this a document; the line-end origin is what the
	// reading then decides. With the scheme test alone this is a "lone URL", the
	// `\n` joins, the host reads as `…ddev.sitenext` and the origin is declined.
	in := `{"excerpt":"https://wt-a--example.ddev.site\nnext line here"}`
	out := string(RewriteJSON([]byte(in), rev, NewStats(false), quiet(), false))
	if strings.Contains(out, "wt-a--example.ddev.site") {
		t.Errorf("a variant hostname reached the shared database through a JSON "+
			"document that happens to open with a URL — §4.3, no undo\n"+
			"  in:  %s\n  out: %s", in, out)
	}
	// And the lone-URL field it must not be confused with still declines: a
	// browser resolves this to www.example.fix, so nothing may change.
	lone := `{"u":"https://wt-a--example.ddev.site\tx"}`
	if got := string(RewriteJSON([]byte(lone), rev, NewStats(false), quiet(), false)); got != lone {
		t.Errorf("a field holding one URL was rewritten, and the browser resolves "+
			"it to a host this map does not name\n  in:  %s\n  out: %s", lone, got)
	}
}

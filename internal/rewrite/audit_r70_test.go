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

// A literal tab keeps joining, in prose and in a value, and so does a newline
// inside a value. This is the half a plain revert would break.
//
// `https://www.example.fi<TAB>x` is the host `www.example.fix` to a URL parser,
// which is §5.5's case, and a raw tab is legal inside a JavaScript string, so an
// inline `fetch("https://www.example.fi<TAB>x")` really does resolve that way. A
// raw *newline* cannot appear inside a JS or JSON string literal — both forbid
// it — which is what makes the prose rule above safe to apply and stops the two
// halves from being the same question.
func TestR70AControlThatTheURLParserRemovesStillJoins(t *testing.T) {
	m := obfMatcher(t)
	const canon = "https://www.example.fi"
	for _, c := range []struct {
		name, in string
		value    bool
	}{
		{"a tab in prose", "a " + canon + "\tx", false},
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

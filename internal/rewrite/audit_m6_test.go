package rewrite

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// TestPrefilterIsNotNarrowerThanTheAutomaton.
//
// M6 put a bytes.Contains prefilter in front of the automaton, testing for
// "//", "\/" and "%2F". The automaton is built AsciiCaseInsensitive, so every
// lowercase percent spelling it would have matched was short-circuited away
// before it ran — and the sweep could not catch them either, because the sweep
// runs through the same filter. A redirect_to= carrying a lowercase-hex
// production origin went out dereferenceable with the straggler counter at
// zero, which is the failure mode §4.4 built the sweep to make impossible.
func TestPrefilterIsNotNarrowerThanTheAutomaton(t *testing.T) {
	m := realMatcher(t)
	for _, in := range []string{
		`https%3A%2F%2Fwww.canon.test%2Fwp-admin`,
		`https%3a%2f%2fwww.canon.test%2fwp-admin`,
		`https%3A%2f%2Fwww.canon.test%2fwp-admin`,
		`%2f%2fwww.canon.test/x`,
		`%2F%2Fwww.canon.test/x`,
	} {
		out, _ := m.Rewrite([]byte(in), SurfaceHTMLAttr, false)
		if bytes.Contains(out, []byte("canon.test")) {
			t.Errorf("a production origin survived the prefilter\n in  %s\n out %s", in, out)
		}
	}

	// And end to end, where the sweep is the last line of defence.
	in := `<a href="/wp-login.php?redirect_to=https%3a%2f%2fwww.canon.test%2fwp-admin%2f">in</a>`
	got := runPipeline(t, m, []byte(in))
	if strings.Contains(string(got), "canon.test") {
		t.Errorf("test 28: the sweep did not catch it either:\n%s", got)
	}
}

// TestPipelineIsIdempotentWithMixedHexCase is acceptance test 7. The prefilter
// ran over the whole buffer, so whether a lowercase origin was rewritten
// depended on whether some unrelated "//" happened to land in the same chunk —
// making the proxy's output depend on its own previous output.
func TestPipelineIsIdempotentWithMixedHexCase(t *testing.T) {
	m := realMatcher(t)
	for _, in := range []string{
		`<!-- c --></textarea>https://www.canon.test </p>https%3a%2f%2fwww.canon.test%2fhttps%3a%2f%2fwww.canon.test%2f<title>`,
		`https%3a%2f%2fwww.canon.test%2f`,
		`<p>x</p>https%3a%2f%2fwww.canon.test%2f`,
	} {
		one := runPipeline(t, m, []byte(in))
		two := runPipeline(t, m, one)
		if !bytes.Equal(one, two) {
			t.Errorf("not a fixed point\n  in %q\n  p1 %q\n  p2 %q", in, one, two)
		}
		if bytes.Contains(one, []byte("canon.test")) {
			t.Errorf("a production origin survived:\n%s", one)
		}
	}
}

// TestPrefilterKeepsTheCarryOverWindow. RewritePrefix promises the caller that
// it may retain b[consumed:]; the prefilter's early return handed back len(b)
// even when limit was smaller, discarding §4.4's window whenever a chunk held
// no separator. An origin straddling a read boundary then went out unrewritten
// — "depending on where the boundary fell", which §4.4 spends two paragraphs
// forbidding.
func TestPrefilterKeepsTheCarryOverWindow(t *testing.T) {
	m := realMatcher(t)
	in := []byte(strings.Repeat("z", 300) + " https://www.canon.test/wp-admin/ " + strings.Repeat("y", 50))

	whole, err := io.ReadAll(NewSweep(bytes.NewReader(in), m, nil, Options{Log: quiet()}))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(whole, []byte("canon.test")) {
		t.Fatalf("unbuffered sweep missed it, so this test proves nothing:\n%s", whole)
	}

	// Every split, including the ones that cut the origin in half and the ones
	// that put no separator byte in the first chunk at all.
	for _, n := range []int{1, 7, 64, 301, 305, 310, 320, 349} {
		src := &chunked{b: append([]byte(nil), in...), n: n}
		got, err := io.ReadAll(NewSweep(src, m, nil, Options{Log: quiet()}))
		if err != nil {
			t.Fatalf("chunk=%d: %v", n, err)
		}
		if bytes.Contains(got, []byte("canon.test")) {
			t.Errorf("chunk=%d: test 28 leak — the origin straddled a read boundary", n)
		}
		if !bytes.Equal(got, whole) {
			t.Errorf("chunk=%d: output depends on the read boundary", n)
		}
	}
}

// TestTrailingRootDotInProse is the shape the positional heuristic could not
// see: a text node that *starts* with the origin. A list of links, or a
// paragraph opening with its own URL, is exactly the privacy-policy prose M6
// added SurfaceText for.
func TestTrailingRootDotInProse(t *testing.T) {
	m := realMatcher(t)
	for _, c := range []struct{ in, want string }{
		{`<p>https://www.canon.test.</p>`, `<p>http://v.ddev.site:8443.</p>`},
		{`<li>https://www.canon.test.</li>`, `<li>http://v.ddev.site:8443.</li>`},
		{`<p>Read more at https://www.canon.test.</p>`, `<p>Read more at http://v.ddev.site:8443.</p>`},
		{`<!-- https://www.canon.test. -->`, `<!-- http://v.ddev.site:8443. -->`},
		// An attribute value is a value: there the dot is the root label, and
		// M0 counted five of those in acmecorp' database.
		{`<a href="https://www.canon.test.">x</a>`, `<a href="http://v.ddev.site:8443">x</a>`},
		{`<a href="https://www.canon.test./p">x</a>`, `<a href="http://v.ddev.site:8443/p">x</a>`},
	} {
		if got := string(runPipeline(t, m, []byte(c.in))); got != c.want {
			t.Errorf("in   %s\ngot  %s\nwant %s", c.in, got, c.want)
		}
	}
}

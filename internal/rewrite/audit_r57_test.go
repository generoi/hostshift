package rewrite

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
)

// Round 57 audits the *decision window*: every byte a single match's
// accept-or-reject decision may read past the pattern it matched, crossed with
// the escape alphabet that spells those bytes, and with the two independent
// implementations of that alphabet — origin's (braceEscAt, escColonLen,
// removedEscLen, escTerminates) and this package's view (jsEscAt, jsonEscRune,
// octalRemovable).

func r57map(t *testing.T) *origin.Map {
	t.Helper()
	m, err := origin.NewMap([]origin.Site{{
		Name:      "main",
		Canonical: origin.MustParse("https://www.example.fi"),
		Variant:   origin.MustParse("https://wt--www.example.fi"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// r57chunks reads b out in fixed-size pieces, which is what the tokenizer above
// the sweep does: it hands back one small token at a time, so the carry-over
// boundary lands at essentially every offset of a real page.
type r57chunks struct {
	b []byte
	n int
}

func (c *r57chunks) Read(p []byte) (int, error) {
	if len(c.b) == 0 {
		return 0, io.EOF
	}
	n := min(min(c.n, len(c.b)), len(p))
	copy(p, c.b[:n])
	c.b = c.b[n:]
	return n, nil
}

// TestR57StreamedSweepAgreesWithWholeBuffer holds the invariant maxRemovedRun's
// comment claims: "Both paths stop at the same byte now, so both give the same
// answer". MaxMatchLen sizes the sweep's carry-over window as maxPat + 16 + 64,
// where the 16 is "a trailing root dot, `:port` and the delimiter". Round 56
// raised what an escaped port colon can cost from ten bytes to maxBraceEsc (72)
// — `braceEscAt` reads unlimited leading zeros — and left the 16 alone. So the
// decision now reads up to 1 + 72 + 5 + 64 + 3 bytes past the host while the
// window promises 80, and the sweep decides a match on bytes that have not
// arrived.
//
// ada, on the decoded JavaScript string
// `https://www.example.fi` + 27 tabs + `:` : host www.example.fi, port "". That
// is the canonical origin, so it must be rewritten. Whole-buffer does; streamed
// does not, at exactly the offsets where the construct straddles the window —
// a live production origin in an inline <script> reaching the browser (test 28),
// non-deterministically by read boundary, and with no straggler WARN either,
// because the sweep is the thing that reports them.
func TestR57StreamedSweepAgreesWithWholeBuffer(t *testing.T) {
	m := r57map(t).Forward()
	// `\u{` + 60 zeros + `3a}` — 66 bytes spelling a colon. JavaScript admits
	// leading zeros without limit, which is what round 56 taught braceEscAt.
	esc := `\u{` + strings.Repeat("0", 60) + `3a}`
	frag := `fetch("https://www.example.fi` + strings.Repeat(`\t`, 27) + esc + `")`

	// An ordinary bytes.Reader, so the sweep takes its default 32 KiB reads and
	// the window boundary falls at len(pending)-MaxMatchLen.
	for _, off := range []int{32640, 32650} {
		body := strings.Repeat("x", off) + frag + strings.Repeat("y", 4000)
		want, _ := m.Rewrite([]byte(body), SurfaceStraggler, false)
		if !bytes.Contains(want, []byte("wt--")) {
			t.Fatalf("off=%d: the whole-buffer path did not rewrite; fixture is wrong", off)
		}
		sw := NewSweep(bytes.NewReader([]byte(body)), m, nil, Options{Stats: NewStats(false)})
		got, err := io.ReadAll(sw)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("off=%d: the streamed sweep disagrees with the whole-buffer one.\n"+
				"ada resolves the decoded string to www.example.fi, so this is a live\n"+
				"production origin the stream left on the page (test 28).\n"+
				" whole: rewrote=%v\n stream: rewrote=%v",
				off, bytes.Contains(want, []byte("wt--")), bytes.Contains(got, []byte("wt--")))
		}
	}

	// And the same construct at every chunk size, which is what the tokenizer
	// actually produces.
	body := strings.Repeat("x", 300) + frag + strings.Repeat("y", 300)
	want, _ := m.Rewrite([]byte(body), SurfaceStraggler, false)
	for _, cs := range []int{1, 5, 17, 64, 128, 4096} {
		sw := NewSweep(&r57chunks{b: []byte(body), n: cs}, m, nil, Options{Stats: NewStats(false)})
		got, err := io.ReadAll(sw)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("chunk=%d: streamed sweep disagrees with the whole-buffer one", cs)
		}
	}
}

// TestR57BraceEscapeInsideHostIsDecodedByTheView is the same escape read by the
// other implementation of the alphabet.
//
// Round 56's own commit says it: "`\u{...}` stopped after six hex digits on the
// argument that six is every code point there is. Six is every code point
// *value*; an escape is a different length from the number it spells". It fixed
// braceEscAt, in origin. The view in this package still reads `\u{...}` through
// jsEscAt, which pads to four hex digits and refuses anything longer — so a
// fifth digit, leading zero or not, is an escape the view cannot read.
//
// It matters where the escape is *inside* the host, because there the byte
// matcher cannot match at all and only the view can see the origin. ada:
// `https://www.example\u{0002e}fi/p` is host www.example.fi, the canonical.
// Four spellings of the same dot, two rewritten and three not.
func TestR57BraceEscapeInsideHostIsDecodedByTheView(t *testing.T) {
	m := r57map(t).Forward()
	for _, esc := range []string{`\u{2e}`, `\u{002e}`, `\u{0002e}`, `\u{0000002e}`, `\u{000000000000002e}`} {
		doc := `<html><body><script>fetch("https://www.example` + esc + `fi/p")</script></body></html>`
		out, err := io.ReadAll(NewHTML(strings.NewReader(doc), m, nil, Options{Stats: NewStats(false)}))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(out, []byte("wt--")) {
			t.Errorf("%s: not rewritten — ada resolves this to www.example.fi, so a live\n"+
				"production origin went to the browser inside <script> (test 28).\n  out: %s",
				esc, out)
		}
	}
}

// TestR57PercentViewIsRebuiltWhenBytesMove checks round 56's shared percent
// view. stripForPercent is built once in rewriteAll and handed to the two
// composed passes through a closure whose `stale` flag is set only when a splice
// changed the buffer's *total length*.
//
// Total length is a proxy for the property the view needs, which is that no byte
// moved — and it is not a sound one. Two splices with cancelling deltas move
// every byte between them and leave the length alone, so the composed pass then
// splices at offsets into a buffer that is no longer the one the view was built
// over. The map below is an ordinary two-site --from/--to map (PLAN §5.3
// layer 3), and this is a *request* body: the corruption goes upstream into the
// database §4.3 says stays byte-identical to production.
func TestR57PercentViewIsRebuiltWhenBytesMove(t *testing.T) {
	m, err := origin.NewMap([]origin.Site{
		{Name: "a", Canonical: origin.MustParse("https://aaaaaa.fi"), Variant: origin.MustParse("https://bb.fi")},
		{Name: "b", Canonical: origin.MustParse("https://cc.fi"), Variant: origin.MustParse("https://dddddd.fi")},
	})
	if err != nil {
		t.Fatal(err)
	}
	h := hostsFor(m.Forward())
	body := []byte(`a=https%3A%2F%2Faaaaaa.fi%2Fx&b=https%3A%2F%2Fcc.fi%2Fy&c=https%5C3a+%5C2f+%5C2f+aaaaaa.fi%2Fz`)
	want := []byte(`a=https%3A%2F%2Fbb.fi%2Fx&b=https%3A%2F%2Fdddddd.fi%2Fy&c=https%5C3a+%5C2f+%5C2f+bb.fi%2Fz`)
	got := h.rewriteAll(append([]byte(nil), body...), true, SurfaceRequestBody, nil)
	if !bytes.Equal(got, want) {
		t.Errorf("the shared percent view spliced at stale offsets.\n in:   %s\n want: %s\n got:  %s",
			body, want, got)
	}
}

// ---------------------------------------------------------------------------
// Fix-side coverage for round 57: the mutations that survived on arrival.
// ---------------------------------------------------------------------------

// Every octal spelling of a character the URL parser removes, at both widths.
//
// Mutations M17 and M18 survived: dropping LF and CR from octalRemovable, and
// narrowing it from three octal digits to two. The commit that added it uses
// `\011` as its example and the only test exercised `\11`, so the three-digit
// form — the one in the commit message — was unpinned, and two of the three
// characters were never exercised at all. ada removes all three.
func TestR57EveryOctalRemovableIsRemoved(t *testing.T) {
	m := r55Matcher(t)
	for _, esc := range []string{`\11`, `\011`, `\12`, `\012`, `\15`, `\015`} {
		in := `<script>fetch("https://www.example` + esc + `.fi/x")</script>`
		if out := rewriteHTML(t, m, in, NewStats(false)); !strings.Contains(out, r55Variant) {
			t.Errorf("%s spells a character a browser deletes, so this is %s and it "+
				"was served live:\n  %s", esc, r55Canonical, out)
		}
	}
	// And an octal escape that is not one of the three must stay: `\101` is `A`,
	// so the host is www.exampleA.fi and this map does not contain it.
	in := `<script>fetch("https://www.example\101.fi/x")</script>`
	if out := rewriteHTML(t, m, in, NewStats(false)); out != in {
		t.Errorf("a browser resolves this to www.exampleA.fi, so nothing may change:\n  %s", out)
	}
}

// The percent-CSS gate reads both spellings of a percent escape.
//
// Mutation M11 — keeping only the uppercase `%5C` — survived, because every test
// spelled the gate uppercase. A form encoder is free to emit either, and the
// prefilter must not be narrower than the thing it filters for.
func TestR57ThePercentCSSGateIsCaseBlind(t *testing.T) {
	rev := r55Reverse(t)
	for _, spelled := range []string{
		`u=https%5C3a+%5C2f+%5C2f+` + r55Variant + `%5C2f+x`,
		`u=https%5c3a+%5c2f+%5c2f+` + r55Variant + `%5c2f+x`,
	} {
		back := r55Back(rev, spelled)
		if strings.Contains(back, r55Variant) {
			t.Errorf("the variant hostname survived the request direction and goes "+
				"into the shared database:\n  in  %s\n  out %s", spelled, back)
		}
	}
}

// A CSS escape's terminator set, pinned against widening as well as narrowing.
//
// Mutation M26 — letting `_` terminate an escape too — survived. The set is
// exactly the whitespace CSS consumes after a hex escape, plus the `+` a form
// encoding writes for a space; anything else is a literal character that the
// view must keep, because the offsets it maps are what the splice replaces.
func TestR57TheCSSEscapeTerminatorSetIsExact(t *testing.T) {
	for _, c := range []struct {
		after    string
		consumed bool
	}{
		{" ", true}, {"+", true}, {"\t", true}, {"\n", true}, {"\r", true},
		{"_", false}, {"-", false}, {"x", false}, {".", false},
	} {
		v := []byte(`\3a` + c.after + `y`)
		got := string(stripForCSS(v, true).b)
		want := ":" + c.after + "y"
		if c.consumed {
			want = ":y"
		}
		if got != want {
			t.Errorf("`\\3a` followed by %q decoded to %q, want %q", c.after, got, want)
		}
	}
}

// The census reports dotted hostnames only.
//
// Mutation M19 — letting a single label through plausibleHost — survived. The
// dot rule is what keeps `&` and its neighbours out of a list `check` prints and
// tells the developer to paste into hostshift.yaml.
func TestR57TheCensusWantsADot(t *testing.T) {
	got := HostsIn([]byte(`<a href="https://localhost/x">a</a><a href="https://c.example/y">b</a>`))
	if _, ok := got["localhost"]; ok {
		t.Errorf("a single label reached the census: %v", got)
	}
	if got["c.example"] != 1 {
		t.Errorf("the dotted host was dropped with it: %v", got)
	}
}

// A removable control between the host and a delimiter, in every spelling.
//
// Mutation m6b — narrowing origin.isRemovedRune to tab alone — survived. Every
// existing case put an ordinary character after the control, where skipping it
// and not skipping it reach the same verdict by different routes. The two
// readings only diverge when what follows *is* a delimiter: skipped, the host
// ends at the `/` and is this map's; not skipped, the escape is read as a
// non-delimiter and the whole origin is declined and served live. ada removes
// all three characters before it reads the host.
func TestR57ARemovedControlBeforeADelimiter(t *testing.T) {
	m := r55Matcher(t)
	spellings := append([]string{"\t", "\n", "\r"},
		`\t`, `\n`, `\r`,
		`\x09`, `\x0a`, `\x0d`,
		`\u{9}`, `\u{a}`, `\u{0000000d}`,
		`\11`, `\012`, `\015`)
	for _, esc := range spellings {
		in := `<script>fetch("https://` + r55Canonical + esc + `/x")</script>`
		if out := rewriteHTML(t, m, in, NewStats(false)); !strings.Contains(out, r55Variant) {
			t.Errorf("a browser deletes %q before reading the host, so this is %s "+
				"and it was served live:\n  %s", esc, r55Canonical, out)
		}
	}
}

// The same three characters through the *byte matcher*, where no view runs.
//
// Mutation m6b survived the test above too: on an HTML surface the escape view
// removes these escapes before the locator ever asks, so origin.isRemovedRune is
// unobservable there. The straggler sweep has no view — it is the raw-bytes
// backstop — and that is the path the predicate actually serves.
func TestR57TheMatcherRemovesThemWithoutAView(t *testing.T) {
	m := r55Matcher(t)
	spellings := append([]string{"\t", "\n", "\r"},
		`\t`, `\n`, `\r`,
		`\x09`, `\x0a`, `\x0d`,
		`\u0009`, `\u000a`, `\u000d`,
		`\u{9}`, `\u{0000000a}`,
		`\11`, `\012`, `\015`)
	for _, esc := range spellings {
		in := "https://" + r55Canonical + esc + "/x"
		out, _ := m.Rewrite([]byte(in), SurfaceStraggler, false)
		if !strings.Contains(string(out), r55Variant) {
			t.Errorf("a browser deletes %q before reading the host, so this is %s "+
				"and the sweep left it live:\n  %s", esc, r55Canonical, string(out))
		}
	}
}

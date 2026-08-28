package rewrite

import (
	"bytes"
	"io"

	"os"
	"strings"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
)

// realMatcher is a non-identity map, for the tests that need the rewriter to
// actually change bytes.
func realMatcher(t *testing.T) *origin.Matcher {
	t.Helper()
	m, err := origin.NewMatcher([]origin.Pair{{
		Name:      "main",
		Canonical: origin.MustParse("https://www.canon.test"),
		Variant:   origin.MustParse("http://v.ddev.site:8443"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	return m
}

// chunked feeds n bytes at a time, so the sweep's flush boundaries land where
// the test wants them rather than where a 32 KiB read puts them.
type chunked struct {
	b []byte
	n int
}

func (c *chunked) Read(p []byte) (int, error) {
	if len(c.b) == 0 {
		return 0, io.EOF
	}
	k := min(min(c.n, len(p)), len(c.b))
	copy(p, c.b[:k])
	c.b = c.b[k:]
	return k, nil
}

// TestSweepKeepsLeftContextAcrossFlush is the left half of §4.4's window.
//
// The carry-over window stops a match being decided on bytes that have not
// arrived on the right. Nothing did the same on the left, and a
// protocol-relative "//host" is decided entirely by the byte before it: after
// "z" it is a path segment, at the start of a value it is an origin. When
// compaction happened to leave the match at pending[0] the guard read "start of
// stream", so the same document rewrote differently depending on where the read
// boundary fell — and a path segment ".../cache//www.canon.test/..." silently
// became the variant host.
func TestSweepKeepsLeftContextAcrossFlush(t *testing.T) {
	m := realMatcher(t)
	in := []byte(strings.Repeat("z", 200) + "//www.canon.test/x" + strings.Repeat("y", 100))

	whole, err := io.ReadAll(NewSweep(bytes.NewReader(in), m, nil, Options{Log: quiet()}))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(whole, in) {
		t.Fatalf("unbuffered: a path segment was rewritten:\n%s", whole)
	}

	// The chunk size is chosen so compaction lands the match at pending[0].
	src := &chunked{b: append([]byte(nil), in...), n: 200 + m.MaxMatchLen()}
	got, err := io.ReadAll(NewSweep(src, m, nil, Options{Log: quiet()}))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, in) {
		t.Fatalf("streamed: a path segment was rewritten at the flush boundary:\n got %s\nwant %s", got, in)
	}
}

// TestOutputDoesNotDependOnReadBoundary is the same defect stated as the
// property that actually matters: content rewrites the same way wherever it
// lands in the stream. Without it a bug reproduces only at one page length.
func TestOutputDoesNotDependOnReadBoundary(t *testing.T) {
	m := realMatcher(t)
	const body = `<style>zzzzzza//www.canon.test/xqqqqqqqqq</style>`

	var first []byte
	for pad := 32600; pad < 32800; pad++ {
		in := []byte("<p>" + strings.Repeat("p", pad) + "</p>" + body)
		out, err := io.ReadAll(NewResponseBody(bytes.NewReader(in), m, nil, Options{Log: quiet()}))
		if err != nil {
			t.Fatal(err)
		}
		tail := out[len(out)-len(body):]
		if first == nil {
			first = append([]byte(nil), tail...)
			continue
		}
		if !bytes.Equal(tail, first) {
			t.Fatalf("pad=%d changed the result:\n got %s\nwant %s", pad, tail, first)
		}
	}
	if !bytes.Equal(first, []byte(body)) {
		t.Fatalf("a path segment was rewritten: %s", first)
	}
}

// TestPipelineIsAFixedPointOnHazards is tests 7 and 29: feeding the proxy's own
// output back through it changes nothing. The boundary bug broke it — pass 1
// changed the token lengths, so pass 2's flush fell on a "//" that pass 1 had
// correctly left alone.
func TestPipelineIsAFixedPointOnHazards(t *testing.T) {
	m := realMatcher(t)
	cases := []string{
		`x//www.canon.test<a href="">//www.canon.test`,
		`<a href="https://www.canon.test/a">https://www.canon.test</a>`,
		`<script>var u="//www.canon.test/x",p="a//www.canon.test/y"</script>`,
	}
	for _, in := range cases {
		one := runPipeline(t, m, []byte(in))
		two := runPipeline(t, m, one)
		if !bytes.Equal(one, two) {
			t.Errorf("not a fixed point\n  in %q\n  p1 %q\n  p2 %q", in, one, two)
		}
	}
}

func runPipeline(t *testing.T, m *origin.Matcher, in []byte) []byte {
	t.Helper()
	out, err := io.ReadAll(NewResponseBody(bytes.NewReader(in), m, nil, Options{Log: quiet()}))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestDryRunReportsNoStragglers guards §5.8's promise that --dry-run is the
// mode you point at a live canonical checkout to assess a site.
//
// The sweep re-scans *rewritten* output; under --dry-run the structured pass
// emits the input unchanged, so the sweep found every origin on the page and
// reported each as a bug in the structured pass. One origin in, one WARN and a
// doubled counter out — and on a corpus page, about a thousand.
func TestDryRunReportsNoStragglers(t *testing.T) {
	m := realMatcher(t)
	in := []byte(`<a href="https://www.canon.test/a">x</a>`)

	st := NewStats(false)
	out, err := io.ReadAll(NewResponseBody(bytes.NewReader(in), m, nil, Options{DryRun: true, Stats: st, Log: quiet()}))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, in) {
		t.Fatalf("--dry-run changed the body:\n got %s\nwant %s", out, in)
	}
	if n := st.Rewrites(SurfaceStraggler); n != 0 {
		t.Errorf("--dry-run reported %d stragglers, want 0", n)
	}
	if n := st.Rewrites(SurfaceHTMLAttr); n != 1 {
		t.Errorf("html-attr = %d, want 1", n)
	}

	var report bytes.Buffer
	st.WriteReport(&report)
	if !strings.Contains(report.String(), "straggler sweep: not run") {
		t.Errorf("the report prints no straggler count and does not say why:\n%s", report.String())
	}
}

// TestTrailingRootDot covers both halves of a rule that has to cut two ways.
//
// M0 found five "https://www.acmecorp.fi." in the database: without the rule
// the dot makes the host look truncated, the origin is rejected as not-a-URL,
// and it reaches the browser dereferenceable — test 28. With the rule applied
// to any delimiter, it ate the full stop in ordinary prose, which is a rendered
// difference from canonical and shows up as a corpus diff.
func TestTrailingRootDot(t *testing.T) {
	m := realMatcher(t)
	for _, c := range []struct{ in, want string }{
		// Real URL structure follows: the dot is the root label, absorbed,
		// because the variant is written root-less.
		{`<a href="https://www.canon.test./p">x</a>`, `<a href="http://v.ddev.site:8443/p">x</a>`},
		{`<a href="https://www.canon.test.">x</a>`, `<a href="http://v.ddev.site:8443">x</a>`},
		{`<a href="https://www.canon.test.?q=1">x</a>`, `<a href="http://v.ddev.site:8443?q=1">x</a>`},
		// Prose: the dot is a full stop. Rewritten, and the stop kept.
		{`<p>Read more at https://www.canon.test. Thanks.</p>`, `<p>Read more at http://v.ddev.site:8443. Thanks.</p>`},
		{`<p>see https://www.canon.test.</p>`, `<p>see http://v.ddev.site:8443.</p>`},
		// Still not a URL: a longer host that merely starts the same way.
		{`<a href="https://www.canon.test.evil.example/p">x</a>`, `<a href="https://www.canon.test.evil.example/p">x</a>`},
	} {
		got := runPipeline(t, m, []byte(c.in))
		if string(got) != c.want {
			t.Errorf("in   %s\ngot  %s\nwant %s", c.in, got, c.want)
		}
	}
}

// TestEntityEncodedOriginIsRewritten is test 28 for the encoding the matcher
// does not model. A browser decodes character references in an attribute value
// before it resolves the URL, so href="https:&#47;&#47;www.canon.test/x"
// navigates to production — and §7 calls that safety-critical, because an agent
// following the link issues writes against the live site.
func TestEntityEncodedOriginIsRewritten(t *testing.T) {
	m := realMatcher(t)
	for _, in := range []string{
		`<a href="https:&#47;&#47;www.canon.test/secret">x</a>`,
		`<a href="https:&#x2F;&#x2f;www.canon.test/secret">x</a>`,
		`<a href="https:&sol;&sol;www.canon.test/secret">x</a>`,
		`<a href="https:&#047;&#047;www.canon.test/secret">x</a>`,
		`<a href="https://www&period;canon&period;test/secret">x</a>`,
		`<a href="https://&#119;&#119;&#119;.canon.test/secret">x</a>`,
	} {
		got := string(runPipeline(t, m, []byte(in)))
		if strings.Contains(got, "canon.test") {
			t.Errorf("a dereferenceable production origin survived\n  in  %s\n  out %s", in, got)
		}
		if !strings.Contains(got, "v.ddev.site:8443/secret") {
			t.Errorf("not rewritten to the variant\n  in  %s\n  out %s", in, got)
		}
	}
}

// TestEntityDecodeLeavesInnocentValuesAlone is the constraint that makes the
// decode safe to run at all. html.UnescapeString over a whole value would also
// apply the legacy no-semicolon forms, turning a query string's "&copy=1" into
// "©=1" — a link broken on a page that had nothing wrong with it. Only
// references that could form part of an origin are decoded, and only when the
// decoded value actually carries one.
func TestEntityDecodeLeavesInnocentValuesAlone(t *testing.T) {
	m := realMatcher(t)
	for _, in := range []string{
		`<a href="/x?a=1&copy=1&b=2">x</a>`,
		`<a href="/x?a=1&amp;b=2">x</a>`,
		`<a href="https://www.canon.test/x?a=1&copy=1">x</a>`,
		`<img alt="Tom &amp; Jerry &lt;3" src="/a.png">`,
		`<a href="&#47;local/path">x</a>`,
	} {
		got := string(runPipeline(t, m, []byte(in)))
		want := strings.ReplaceAll(in, "https://www.canon.test", "http://v.ddev.site:8443")
		if got != want {
			t.Errorf("value was mangled\n  in   %s\n  got  %s\n  want %s", in, got, want)
		}
	}
}

// TestStragglerOffsetsAreInputOffsets is §4.4's requirement that --explain
// offsets are cumulative *input*-stream offsets, "so they stay stable across a
// length-changing rewrite".
//
// The sweep runs downstream of that rewrite and counts the stream it scans, so
// its offsets drifted by the total length change so far. On a page with 1000
// rewrites of a nine-byte-longer variant that is 9000 bytes — useless for
// locating the gap, and silently mixed into the same event list as the
// structured pass's genuine input offsets.
func TestStragglerOffsetsAreInputOffsets(t *testing.T) {
	m := realMatcher(t)
	// Two rewrites first, so the streams are out of step; then an origin in a
	// doctype, which is the one token kind the structured pass still passes
	// through verbatim — so only the sweep sees it.
	in := `<a href="https://www.canon.test/"></a><a href="https://www.canon.test/"></a>` +
		`<!DOCTYPE html SYSTEM "https://www.canon.test/z">`
	want := strings.Index(in, "https://www.canon.test/z")

	st := NewStats(true)
	if _, err := io.ReadAll(NewResponseBody(strings.NewReader(in), m, nil, Options{Stats: st, Log: quiet()})); err != nil {
		t.Fatal(err)
	}

	var found []int
	for _, e := range st.Events() {
		if e.Surface == SurfaceStraggler && e.Action == origin.ActionRewrote {
			found = append(found, e.Offset)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one straggler, got %v", found)
	}
	if found[0] != want {
		t.Errorf("straggler reported at %d, its input offset is %d (drift %+d)",
			found[0], want, found[0]-want)
	}
}

// TestInputOffsetIsMonotonicOverCorpus exercises the offset map at scale: every
// straggler reported over the corpus must land inside the source document.
func TestInputOffsetIsMonotonicOverCorpus(t *testing.T) {
	m := realMatcher(t)
	for _, f := range corpusFiles(t) {
		in := readFile(t, f)
		st := NewStats(true)
		if _, err := io.ReadAll(NewResponseBody(bytes.NewReader(in), m, nil, Options{Stats: st, Log: quiet()})); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		last := -1
		for _, e := range st.Events() {
			if e.Surface != SurfaceStraggler {
				continue
			}
			if e.Offset < 0 || e.Offset > len(in) {
				t.Errorf("%s: offset %d outside a %d-byte document", f, e.Offset, len(in))
			}
			if e.Offset < last {
				t.Errorf("%s: offsets went backwards, %d after %d", f, e.Offset, last)
			}
			last = e.Offset
		}
	}
}

func TestParseURLRef(t *testing.T) {
	for _, c := range []struct {
		in   string
		want string
		n    int
	}{
		{"&#47;", "/", 5},
		{"&#047;", "/", 6},
		{"&#x2F;", "/", 6},
		{"&#X2f;", "/", 6},
		{"&#47", "/", 4}, // browsers accept a numeric ref without the semicolon
		{"&sol;", "/", 5},
		{"&percnt;", "%", 8},
		{"&copy;", "", 0},     // not URL structure: left alone
		{"&#38;", "", 0},      // '&' excluded: decoding it could splice a new ref
		{"&#x110000;", "", 0}, // out of range
		{"&#;", "", 0},
		{"&sol", "", 0}, // named refs require the semicolon
	} {
		c2, n := parseURLRef([]byte(c.in))
		got := ""
		if n > 0 {
			got = string(c2)
		}
		if got != c.want || n != c.n {
			t.Errorf("parseURLRef(%q) = %q,%d want %q,%d", c.in, got, n, c.want, c.n)
		}
	}
}

func TestDecodeURLRefsPreservesPositions(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"nothing here", "nothing here"},
		{"a&copy=1&b", "a&copy=1&b"},
		{"https:&#47;&#47;h/x", "https://h/x"},
		{"?a=1&amp;b=&#47;", "?a=1&amp;b=/"},
	} {
		got, changed := decodeURLRefs([]byte(c.in))
		if string(got) != c.want {
			t.Errorf("decodeURLRefs(%q) = %q want %q", c.in, got, c.want)
		}
		if changed != (c.in != c.want) {
			t.Errorf("decodeURLRefs(%q) changed = %v", c.in, changed)
		}
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

package rewrite

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
)

// corpusFiles returns spike/corpus (15 real pages, 5,940,172 bytes) and
// spike/adv (36 adversarial fixtures).
func corpusFiles(t *testing.T) []string {
	t.Helper()
	var files []string
	for _, dir := range []string{"../../spike/corpus", "../../spike/adv"} {
		got, err := filepath.Glob(filepath.Join(dir, "*.html"))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, got...)
	}
	if len(files) < 50 {
		t.Fatalf("expected the spike corpus and fixtures, found %d files", len(files))
	}
	return files
}

func identityMatcher(t *testing.T) *origin.Matcher {
	t.Helper()
	pairs := []origin.Pair{
		{Name: "main", Canonical: origin.MustParse("https://www.herrfors.fi"), Variant: origin.MustParse("https://www.herrfors.fi")},
		{Name: "nat", Canonical: origin.MustParse("https://www.herrforsnat.fi"), Variant: origin.MustParse("https://www.herrforsnat.fi")},
		{Name: "ddev", Canonical: origin.MustParse("https://herrfors.ddev.site"), Variant: origin.MustParse("https://herrfors.ddev.site")},
	}
	m, err := origin.NewMatcher(pairs)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	return m
}

func runHTML(t *testing.T, in []byte, m *origin.Matcher, opt Options) []byte {
	t.Helper()
	out, err := io.ReadAll(NewHTML(bytes.NewReader(in), m, nil, opt))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestIdentityMapByteIdentity is acceptance test 24, and the guard rail for
// everything after it: with canonical == variant, output must equal input byte
// for byte over the whole corpus and every adversarial fixture.
//
// It holds by construction rather than by luck — the matcher never splices when
// a pair maps to itself — but it is asserted, because every splice and offset
// defect shows up here first.
func TestIdentityMapByteIdentity(t *testing.T) {
	m := identityMatcher(t)
	var total int
	for _, f := range corpusFiles(t) {
		in, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		out := runHTML(t, in, m, Options{})
		if !bytes.Equal(in, out) {
			t.Errorf("%s: identity map changed the bytes (%d in, %d out)", f, len(in), len(out))
			for i := 0; i < len(in) && i < len(out); i++ {
				if in[i] != out[i] {
					lo := max(0, i-60)
					t.Errorf("  first divergence at offset %d\n   in: %q\n  out: %q", i, in[lo:min(len(in), i+60)], out[lo:min(len(out), i+60)])
					break
				}
			}
		}
		total += len(in)
	}
	t.Logf("identity map byte-identical over %d files, %d bytes", len(corpusFiles(t)), total)
}

// TestIdentityHoldsAtEveryChunkSize feeds the corpus a byte at a time. The
// tokenizer's partition guarantee has to survive an adversarial reader, or the
// streaming proxy path is not the same as the filter path.
func TestIdentityHoldsAtEveryChunkSize(t *testing.T) {
	m := identityMatcher(t)
	for _, f := range corpusFiles(t)[:20] {
		in, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, chunk := range []int{1, 7, 4096} {
			r := NewHTML(&chunkReader{b: in, n: chunk}, m, nil, Options{})
			out, err := io.ReadAll(r)
			if err != nil {
				t.Fatalf("%s chunk=%d: %v", f, chunk, err)
			}
			if !bytes.Equal(in, out) {
				t.Errorf("%s chunk=%d: not byte-identical (%d in, %d out)", f, chunk, len(in), len(out))
			}
		}
	}
}

// chunkReader hands out at most n bytes per Read.
type chunkReader struct {
	b []byte
	n int
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if len(c.b) == 0 {
		return 0, io.EOF
	}
	n := min(min(len(p), c.n), len(c.b))
	copy(p, c.b[:n])
	c.b = c.b[n:]
	return n, nil
}

// TestRewriteChangesOnlyOrigins is the other half of test 24: where URLs *did*
// change, everything else is byte-identical. Splicing never re-serialises, so
// attribute order, quoting and whitespace all survive inside modified start
// tags. Asserted by rewriting, then rewriting back, and requiring the original.
func TestRewriteChangesOnlyOrigins(t *testing.T) {
	fwd := pairMatcher(t, "https://www.herrfors.fi", "https://wt-a--herrfors.ddev.site")
	back := pairMatcher(t, "https://wt-a--herrfors.ddev.site", "https://www.herrfors.fi")

	for _, f := range corpusFiles(t) {
		in, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		mid := runHTML(t, in, fwd, Options{})
		out := runHTML(t, mid, back, Options{})
		if !bytes.Equal(in, out) {
			t.Errorf("%s: round trip is not byte-identical (%d -> %d -> %d)", f, len(in), len(mid), len(out))
		}
		// Line count is preserved because no whitespace is ever rebuilt.
		if a, b := bytes.Count(in, []byte("\n")), bytes.Count(mid, []byte("\n")); a != b {
			t.Errorf("%s: rewriting changed the line count %d -> %d", f, a, b)
		}
	}
}

// TestIdempotencyFixedPoint is acceptance test 7: output re-fed through the
// rewriter is unchanged. The spike's e2e discarded this result, so it was never
// actually covered.
//
// It is the anchoring property doing the work: "//herrfors.ddev.site" does not
// occur inside "//wt-a--herrfors.ddev.site", so a second pass finds nothing.
func TestIdempotencyFixedPoint(t *testing.T) {
	m := pairMatcher(t, "https://herrfors.ddev.site", "https://wt-a--herrfors.ddev.site")
	for _, f := range corpusFiles(t) {
		in, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		once := runHTML(t, in, m, Options{})
		twice := runHTML(t, once, m, Options{})
		if !bytes.Equal(once, twice) {
			t.Errorf("%s: not a fixed point (%d -> %d on the second pass)", f, len(once), len(twice))
		}
	}
}

// TestDryRunEmitsInputUnchanged: --dry-run counts what it would do and changes
// nothing (PLAN §5.8), so it is safe to point at a live canonical checkout.
func TestDryRunEmitsInputUnchanged(t *testing.T) {
	m := pairMatcher(t, "https://www.herrfors.fi", "https://wt-a--herrfors.ddev.site")
	in := []byte(`<a href="https://www.herrfors.fi/x" class="k">t</a>`)

	st := NewStats(false)
	out := runHTML(t, in, m, Options{DryRun: true, Stats: st})
	if !bytes.Equal(in, out) {
		t.Errorf("--dry-run changed the output:\n got %q\nwant %q", out, in)
	}
	if got := st.Rewrites(SurfaceHTMLAttr); got != 1 {
		t.Errorf("--dry-run counted %d html-attr rewrites, want 1", got)
	}
}

func pairMatcher(t *testing.T, from, to string) *origin.Matcher {
	t.Helper()
	m, err := origin.NewMatcher([]origin.Pair{{
		Name: "main", Canonical: origin.MustParse(from), Variant: origin.MustParse(to),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	return m
}

package rewrite

import (
	"bytes"
	"io"
	"os"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
)

// corpusBytes is the largest real page, which is what the throughput numbers
// should be read against.
func corpusBytes(b *testing.B) []byte {
	b.Helper()
	in, err := os.ReadFile("../../spike/corpus/page1.html")
	if err != nil {
		b.Fatal(err)
	}
	return in
}

func benchMatcher(b *testing.B, from, to string) *origin.Matcher {
	b.Helper()
	m, err := origin.NewMatcher([]origin.Pair{{
		Name: "main", Canonical: origin.MustParse(from), Variant: origin.MustParse(to),
	}})
	if err != nil {
		b.Fatal(err)
	}
	return m
}

// BenchmarkPassthrough is the case that dominates in practice: a page with no
// canonical origin in it at all. Nothing should be allocated per token.
func BenchmarkPassthrough(b *testing.B) {
	in := corpusBytes(b)
	m := benchMatcher(b, "https://absent.example", "https://v.example")
	b.SetBytes(int64(len(in)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := io.Copy(io.Discard, NewHTML(bytes.NewReader(in), m, nil, Options{})); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkIdentity is test 24's path: every origin matches and none is
// rewritten.
func BenchmarkIdentity(b *testing.B) {
	in := corpusBytes(b)
	m := benchMatcher(b, "https://www.acmecorp.fi", "https://www.acmecorp.fi")
	b.SetBytes(int64(len(in)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := io.Copy(io.Discard, NewHTML(bytes.NewReader(in), m, nil, Options{})); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRewrite is the real workload: ~1,100 origins spliced on a 500 KB
// page.
func BenchmarkRewrite(b *testing.B) {
	in := corpusBytes(b)
	m := benchMatcher(b, "https://www.acmecorp.fi", "https://wt-a--acmecorp.ddev.site")
	b.SetBytes(int64(len(in)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := io.Copy(io.Discard, NewHTML(bytes.NewReader(in), m, nil, Options{})); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRewriteWithSweep adds §4.4's backstop, which re-scans the whole
// output. This is what the proxy actually runs.
func BenchmarkRewriteWithSweep(b *testing.B) {
	in := corpusBytes(b)
	m := benchMatcher(b, "https://www.acmecorp.fi", "https://wt-a--acmecorp.ddev.site")
	b.SetBytes(int64(len(in)))
	b.ReportAllocs()
	for b.Loop() {
		r := NewResponseBody(bytes.NewReader(in), m, nil, Options{Log: quiet()})
		if _, err := io.Copy(io.Discard, r); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkMatcherValue is the innermost loop: one attribute value.
func BenchmarkMatcherValue(b *testing.B) {
	m := benchMatcher(b, "https://www.acmecorp.fi", "https://wt-a--acmecorp.ddev.site")
	hit := []byte("https://www.acmecorp.fi/app/uploads/2025/07/image-1024x768.jpg")
	miss := []byte("https://cdn.jsdelivr.net/npm/some-package@1.2.3/dist/bundle.min.js")

	b.Run("hit", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			m.Rewrite(hit, "bench", false)
		}
	})
	b.Run("miss", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			m.Rewrite(miss, "bench", false)
		}
	})
}

// BenchmarkMatcherBuild matters because a map is built once per process, but a
// nine-blog site builds 81 patterns and startup should not be noticeable.
func BenchmarkMatcherBuild(b *testing.B) {
	var pairs []origin.Pair
	for i := 0; i < 9; i++ {
		pairs = append(pairs, origin.Pair{
			Name:      string(rune('a' + i)),
			Canonical: origin.MustParse("https://site" + string(rune('a'+i)) + ".example"),
			Variant:   origin.MustParse("https://wt-a--site" + string(rune('a'+i)) + ".ddev.site"),
		})
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := origin.NewMatcher(pairs); err != nil {
			b.Fatal(err)
		}
	}
}

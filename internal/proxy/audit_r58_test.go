package proxy

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
	"github.com/generoi/hostshift/internal/rewrite"
)

// A response header has no CSS tokenizer in front of it, and rewriteAll runs one
// anyway.
//
// `escView(surface)` is surface-aware — rounds 54 to 57 are the story of making
// it so, and origin.escapeAlphabetFor is where the answer lives. `stripForCSS`,
// spliced in eleven lines above it in the same function, never got the same
// treatment: it fires on any buffer holding a backslash, whatever surface the
// caller named. The HTML arm gates it correctly (`cssEscapeLeak` runs for
// `inline-style` and for a `style` attribute and nowhere else); the standalone
// arm the proxy runs over every Tier 1 response header does not gate it at all.
//
// ada, with the variant origin as base:
//
//	new URL("https:/\\awww.example.fi/x", base).host === "awww.example.fi"
//	new URL("https://\\awww.example.fi/x", base).host === "awww.example.fi"
//
// — a third party, not this map's canonical. `\a` is a CSS hex escape for U+000A
// and nothing decodes it in a `Location`, a `Link`, a `Refresh` or a
// `Content-Location`. modifyResponse rewrites all four to the variant, so a
// browser that should have been redirected off-site is sent to a worktree
// hostname that does not resolve, and the census records a rewrite that had no
// origin under it.
//
// The engine's own two entry points disagree about the same value, which is what
// the oracle's second half calls a model error: the identical string in an
// `<a href>` is correctly left alone.
func TestR58AResponseHeaderDecodesNoCSSEscape(t *testing.T) {
	shapes := loadR58Shapes(t)
	px := map[string]*Proxy{
		"https": r58Proxy(t, "https"),
		"http":  r58Proxy(t, "http"),
	}
	var bad int
	for _, sh := range shapes {
		if sh.enc != "raw" || sh.resolvedHost() == r58Canonical {
			continue // the leak half is what the oracle already holds
		}
		if !headerSafe(sh.candidate) || sh.knownRelativePortLimit() {
			continue // never a header value, or PLAN §5.5's recorded limitation
		}
		out := r58Header(t, px[sh.base], "Location", sh.candidate)
		if out == sh.candidate {
			continue
		}
		bad++
		if bad <= 8 {
			t.Errorf("a browser resolves this Location to %q, so nothing may change it:\n in  %q\n out %q",
				sh.resolvedHost(), sh.candidate, out)
		}
	}
	if bad > 8 {
		t.Errorf("%d Location values were rewritten that point somewhere else", bad)
	}
}

// The same question asked as a disagreement rather than as an oracle lookup: one
// value, two of the engine's own arms, one document.
func TestR58TheLocationAndTheHrefAgreeAboutOneValue(t *testing.T) {
	p := r58Proxy(t, "https")
	fwd := p.Map.Forward()
	for _, v := range []string{
		`https://\awww.example.fi/x`,
		`https:/\awww.example.fi/x`,
		`//\awww.example.fi/x`,
	} {
		hdr := r58Header(t, p, "Location", v)
		body, err := io.ReadAll(rewrite.NewResponseBody(
			strings.NewReader(`<a href="`+v+`">x</a>`), fwd, nil, rewrite.Options{}))
		if err != nil {
			t.Fatal(err)
		}
		inHref := strings.Contains(string(body), r58Variant)
		inHdr := strings.Contains(hdr, r58Variant)
		if inHref != inHdr {
			t.Errorf("the href arm and the header arm disagree about %q:\n href rewrote=%v (%s)\n hdr  rewrote=%v (%q)",
				v, inHref, body, inHdr, hdr)
		}
	}
}

const (
	r58Canonical = "www.example.fi"
	r58Variant   = "wt-a--example.ddev.site"
)

func r58Proxy(t *testing.T, base string) *Proxy {
	t.Helper()
	m, err := origin.NewMap([]origin.Site{{
		Name:      "main",
		Canonical: origin.MustParse("https://" + r58Canonical),
		Variant:   origin.MustParse(base + "://" + r58Variant),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return &Proxy{Map: m, Stats: rewrite.NewStats(false)}
}

// r58Header runs the real response-header pass over one header value.
func r58Header(t *testing.T, p *Proxy, name, val string) string {
	t.Helper()
	req := httptest.NewRequest("GET", "https://"+r58Variant+"/page", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader("")),
		Request:    req,
	}
	resp.Header.Set(name, val)
	resp.Header.Set("Content-Type", "text/html")
	if err := p.modifyResponse(resp); err != nil {
		t.Fatal(err)
	}
	return resp.Header.Get(name)
}

// headerSafe reports whether a value can be carried in a header field at all.
func headerSafe(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}

type r58Shape struct{ base, enc, candidate, resolved string }

// resolvedHost is ada's answer with the trailing root dot absorbed, the way
// PLAN §4.4 specifies and the rewrite oracle already does.
func (s r58Shape) resolvedHost() string { return strings.TrimSuffix(s.resolved, ".") }

// knownRelativePortLimit is PLAN §5.5's recorded limitation, restated here so
// this test reports the defect it is about and not that one: a scheme-relative
// reference with an explicit port that is the *other* scheme's default is
// resolved by the byte matcher under the canonical's scheme, because the hot
// path has no document to consult.
func (s r58Shape) knownRelativePortLimit() bool {
	if s.base != "http" || s.candidate == "" {
		return false
	}
	if c := s.candidate[0]; c != '/' && c != '\\' {
		return false
	}
	return strings.HasSuffix(s.resolved, ":443")
}

func loadR58Shapes(t *testing.T) []r58Shape {
	t.Helper()
	f, err := os.Open("../../testdata/url-shapes.tsv.gz")
	if err != nil {
		t.Fatalf("%v (regenerate: node test/gen-url-corpus.js | gzip -9 > testdata/url-shapes.tsv.gz)", err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	var out []r58Shape
	sc := bufio.NewScanner(zr)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		fs := strings.Split(sc.Text(), "\t")
		if len(fs) != 4 {
			continue
		}
		var cand string
		if err := json.Unmarshal([]byte(fs[2]), &cand); err != nil {
			t.Fatalf("corpus line %q: %v", sc.Text(), err)
		}
		out = append(out, r58Shape{fs[0], fs[1], cand, fs[3]})
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatal("empty corpus")
	}
	return out
}

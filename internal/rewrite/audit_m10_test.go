package rewrite

import (
	"io"
	"strings"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
	"golang.org/x/net/html"
)

// TestEntityDecodeCannotCreateMarkup. decodeEntityLeak decodes character
// references in an attribute value and splices the decoded text back *between
// the attribute's quotes*, without re-encoding. Every character that is
// structural there — the quotes, the angle brackets, '=' — therefore ended the
// attribute, or the tag, and everything after it became markup:
//
//	href="…&#34;&#62;&#60;script&#62;alert(1)&#60;/script&#62;"
//	  ->  …"><script>alert(1)</script>"
//
// The upstream page is safe: WordPress escaped that content correctly, and it is
// inert on production. hostshift is what made it execute, on the developer's
// variant host, which is where their admin session lives.
func TestEntityDecodeCannotCreateMarkup(t *testing.T) {
	c := origin.MustParse("https://www.example.fi")
	v := origin.MustParse("https://wt-a--ex.ddev.site")
	m, err := origin.NewMap([]origin.Site{{Name: "s", Canonical: c, Variant: v}})
	if err != nil {
		t.Fatal(err)
	}

	// Counts elements and attributes the way a browser's parser would, so the
	// assertion is about parse shape rather than about bytes.
	shape := func(s string) (elems map[string]int, attrs map[string]int) {
		elems, attrs = map[string]int{}, map[string]int{}
		z := html.NewTokenizer(strings.NewReader(s))
		for {
			switch z.Next() {
			case html.ErrorToken:
				return
			case html.StartTagToken, html.SelfClosingTagToken:
				name, hasAttr := z.TagName()
				elems[string(name)]++
				for hasAttr {
					var k []byte
					k, _, hasAttr = z.TagAttr()
					attrs[string(k)]++
				}
			}
		}
	}

	for _, in := range []string{
		// the attribute's own quote, in both quotings
		`<a href="https:&#47;&#47;www.example.fi/x&#34;&#62;&#60;script&#62;alert(1)&#60;/script&#62;">x</a>`,
		`<a href='https:&#47;&#47;www.example.fi/?s=&#039; onmouseover=alert(1) x=&#039;'>s</a>`,
		// '=' and '/' making a new attribute inside an unterminated one
		`<img src="https:&#47;&#47;www.example.fi/x&#34;&#47;onerror&#61;alert(1)">`,
		// a raw newline, which the named table used to supply past the
		// printable-ASCII guard
		`<a href="https:&#47;&#47;www.example.fi/x&NewLine;onerror=alert(1)">x</a>`,
	} {
		out, e := io.ReadAll(NewResponseBody(strings.NewReader(in), m.Forward(), nil,
			Options{Stats: NewStats(false)}))
		if e != nil {
			t.Fatal(e)
		}
		wantE, wantA := shape(in)
		gotE, gotA := shape(string(out))
		for k, n := range gotE {
			if n > wantE[k] {
				t.Errorf("rewriting created <%s>:\n in: %s\nout: %s", k, in, out)
			}
		}
		for k, n := range gotA {
			if n > wantA[k] {
				t.Errorf("rewriting created attribute %q:\n in: %s\nout: %s", k, in, out)
			}
		}
	}
}

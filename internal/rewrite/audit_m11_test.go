package rewrite

import (
	"io"
	"strings"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
	"golang.org/x/net/html"
)

// TestLayeredReferencesAreDeclined. The entity decoder refuses to *emit* a
// structural character, which stops it building markup directly. It does not
// stop it emitting a digit that fuses with an adjacent fragment it did not
// consume: "&#6" is below the printable range so it passes through literally,
// then "&#48;" decodes to '0', then the next literal byte is ';' — and together
// they spell "&#60;". Harmless in an href; a real '<' in an attribute the
// browser decodes a second time.
//
// The value is declined whole rather than half-decoded. That leaves an
// entity-spelled canonical origin in place, which is a test 28 gap and is
// recorded as one in PLAN §5.3 — the trade is deliberate, because the
// alternative was creating executable markup on the developer's own host.
func TestLayeredReferencesAreDeclined(t *testing.T) {
	c := origin.MustParse("https://www.example.fi")
	v := origin.MustParse("https://wt-a--ex.ddev.site")
	m, _ := origin.NewMap([]origin.Site{{Name: "s", Canonical: c, Variant: v}})
	// Every position of a reference that this decoder can supply: the body
	// (&#6 + 0 + ;), the terminator (&#60 + ;), a named fragment (&lt + ;) and
	// a hex one (&#x3c + ;).
	for _, in := range []string{
		`<iframe srcdoc="&#6&#48;;p&#6&#50;;see https:&#47;&#47;www.example.fi/x&#6&#48;;script&#6&#50;;alert(1)&#6&#48;;/script&#6&#50;;"></iframe>`,
		`<iframe srcdoc="see https:&#47;&#47;www.example.fi/x &#60&#59;script&#62&#59;alert(1)&#60&#59;/script&#62&#59;"></iframe>`,
		`<iframe srcdoc="see https:&#47;&#47;www.example.fi/x &lt&#59;script&gt&#59;alert(1)&lt&#59;/script&gt&#59;"></iframe>`,
		`<iframe srcdoc="see https:&#47;&#47;www.example.fi/x &#x3c&#59;script&#x3e&#59;alert(1)&#x3c&#59;/script&#x3e&#59;"></iframe>`,
	} {
		checkNoNestedScript(t, m, in)
	}
}

func checkNoNestedScript(t *testing.T, m *origin.Map, in string) {
	t.Helper()
	out, _ := io.ReadAll(NewResponseBody(strings.NewReader(in), m.Forward(), nil, Options{Stats: NewStats(false)}))
	t.Logf("out: %s", out)
	// what the browser sees when it parses the srcdoc document
	inner := func(s string) string {
		z := html.NewTokenizer(strings.NewReader(s))
		for z.Next() != html.ErrorToken {
			_, hasAttr := z.TagName()
			for hasAttr {
				var k, val []byte
				k, val, hasAttr = z.TagAttr()
				if string(k) == "srcdoc" {
					return string(val)
				}
			}
		}
		return ""
	}
	scripts := func(s string) int {
		n, z := 0, html.NewTokenizer(strings.NewReader(s))
		for z.Next() != html.ErrorToken {
			if name, _ := z.TagName(); string(name) == "script" {
				n++
			}
		}
		return n
	}
	a, b := scripts(inner(in)), scripts(inner(string(out)))
	t.Logf("srcdoc in : %q", inner(in))
	t.Logf("srcdoc out: %q", inner(string(out)))
	if b > a {
		t.Errorf("nested parse gained %d script element(s) (was %d)", b, a)
	}
}

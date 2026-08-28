package rewrite

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// TestAttrSpansAgainstTokenizer validates the span scanner against the
// tokenizer's own output over the whole corpus.
//
// PLAN §5.2: "the committed validator compares only attribute counts and names,
// never ValueStart/ValueEnd. Land the value-span assertion before M1." This is
// that assertion. Splicing is only correct if the spans are, and a count-and-name
// check would not have caught an off-by-one in a value offset.
//
// Comparing values needs care for two reasons, neither of which is a defect in
// the scanner:
//
//   - html.UnescapeString is more aggressive than the tokenizer's attribute mode
//     on legacy entities without a trailing semicolon: it turns "&noticeType"
//     into "¬iceType" and "&gtm" into ">m", which the tokenizer leaves alone.
//     For such a value the raw span already *equals* the tokenizer's value.
//   - the tokenizer normalises CRLF inside attribute values; raw bytes do not.
//
// So the span is accepted if it matches the tokenizer's value either raw or
// unescaped. Unescaping both sides would be wrong in the other direction —
// "&amp;raquo;" decodes once to "&raquo;", and a second pass would turn that
// into "»". A span pointing at the wrong bytes still fails both comparisons.
//
// Duplicate attribute names are *not* excluded on the count check: the scanner
// reports every physical position, and dropping duplicates would lose a splice
// site. TagAttr collapses them, so the counts legitimately differ there.
func TestAttrSpansAgainstTokenizer(t *testing.T) {
	var tags, attrs, dupSkipped int

	for _, f := range corpusFiles(t) {
		in, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		z := html.NewTokenizer(bytes.NewReader(in))
		for {
			tt := z.Next()
			if tt == html.ErrorToken {
				break
			}
			if tt != html.StartTagToken && tt != html.SelfClosingTagToken {
				continue
			}
			raw := append([]byte(nil), z.Raw()...)
			got := scanAttrs(raw)

			// Ground truth from the tokenizer.
			var want [][2]string
			z.TagName()
			for {
				k, v, more := z.TagAttr()
				if k != nil {
					want = append(want, [2]string{string(k), string(v)})
				}
				if !more {
					break
				}
			}
			tags++

			if len(got) != len(want) {
				// The only legitimate divergence is a duplicate attribute name,
				// which the tokenizer collapses and the scanner does not.
				if hasDuplicateName(raw, got) {
					dupSkipped++
					continue
				}
				t.Errorf("%s: attribute count %d, tokenizer %d, raw=%q", f, len(got), len(want), raw)
				continue
			}

			for i, a := range got {
				attrs++
				name := strings.ToLower(string(raw[a.NameStart:a.NameEnd]))
				if name != want[i][0] {
					t.Errorf("%s: attr %d name %q, tokenizer %q, raw=%q", f, i, name, want[i][0], raw)
					continue
				}
				// The value-span assertion §5.2 asks for: the bytes this
				// scanner points at must unescape to the value the tokenizer
				// reports.
				if a.ValueStart < 0 {
					if want[i][1] != "" {
						t.Errorf("%s: attr %q has no value span but tokenizer reports %q, raw=%q", f, name, want[i][1], raw)
					}
					continue
				}
				if a.ValueStart > a.ValueEnd || a.ValueEnd > len(raw) {
					t.Fatalf("%s: attr %q span [%d,%d) is out of range for %d raw bytes", f, name, a.ValueStart, a.ValueEnd, len(raw))
				}
				span := string(raw[a.ValueStart:a.ValueEnd])
				wantVal := nl(want[i][1])
				if nl(span) != wantVal && nl(html.UnescapeString(span)) != wantVal {
					t.Errorf("%s: attr %q value span is %q, tokenizer reports %q, raw=%q", f, name, span, want[i][1], raw)
				}
			}
		}
	}
	t.Logf("scanned %d start tags, %d attribute values; %d tags skipped for duplicate names", tags, attrs, dupSkipped)
	if tags < 19000 || attrs < 30000 {
		t.Errorf("corpus coverage collapsed: %d tags, %d attributes (expected ~19,953 and ~37,280)", tags, attrs)
	}
}

func hasDuplicateName(raw []byte, attrs []Attr) bool {
	seen := map[string]bool{}
	for _, a := range attrs {
		n := strings.ToLower(string(raw[a.NameStart:a.NameEnd]))
		if seen[n] {
			return true
		}
		seen[n] = true
	}
	return false
}

// nl applies the tokenizer's newline normalisation inside attribute values.
func nl(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

// TestAttrSpansSpliceExactly checks the spans on hand-built tags where the
// answer is obvious, including the shapes the corpus does not happen to contain.
func TestAttrSpansSpliceExactly(t *testing.T) {
	cases := []struct {
		raw  string
		want []string // value bytes, in order; "-" for a valueless attribute
	}{
		{`<a href="x">`, []string{"x"}},
		{`<a href='x'>`, []string{"x"}},
		{`<a href=x>`, []string{"x"}},
		{`<a href=x/>`, []string{"x/"}},
		{`<a href = "x" >`, []string{"x"}},
		{`<a disabled href="x">`, []string{"-", "x"}},
		{`<a href="">`, []string{""}},
		{`<a HREF="X" Class='Y'>`, []string{"X", "Y"}},
		{"<a href=\"a\nb\">", []string{"a\nb"}},
		{`<a rel="a" rel="b">`, []string{"a", "b"}}, // both physical positions
		{`<img src="a" />`, []string{"a"}},
		{`<a href="a&amp;b">`, []string{"a&amp;b"}}, // raw bytes, not unescaped
	}
	for _, c := range cases {
		raw := []byte(c.raw)
		got := scanAttrs(raw)
		if len(got) != len(c.want) {
			t.Errorf("%s: %d attributes, want %d", c.raw, len(got), len(c.want))
			continue
		}
		for i, a := range got {
			var v string
			if a.ValueStart < 0 {
				v = "-"
			} else {
				v = string(raw[a.ValueStart:a.ValueEnd])
			}
			if v != c.want[i] {
				t.Errorf("%s: attr %d value %q, want %q", c.raw, i, v, c.want[i])
			}
		}
	}
}

package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/net/html"
)

// ---- intra-tag attribute value span scanner -------------------------------
// Input: the raw bytes of a start tag, exactly as Tokenizer.Raw() returned.
// Output: for each attribute, the byte span of its VALUE within raw
// (excluding quotes), plus the quote style.
// Implements the HTML5 tag-open / attribute-name / attribute-value states.

type Attr struct {
	NameStart, NameEnd   int
	ValueStart, ValueEnd int // ValueStart==ValueEnd==-1 => valueless attribute
	Quote                byte // '"', '\'', or 0 for unquoted
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f'
}

func scanAttrs(raw []byte) []Attr {
	var out []Attr
	i := 1 // skip '<'
	// tag name
	for i < len(raw) && !isSpace(raw[i]) && raw[i] != '>' && raw[i] != '/' {
		i++
	}
	for i < len(raw) {
		for i < len(raw) && isSpace(raw[i]) {
			i++
		}
		if i >= len(raw) || raw[i] == '>' {
			break
		}
		if raw[i] == '/' { // self-closing slash, or stray slash
			i++
			continue
		}
		a := Attr{NameStart: i, ValueStart: -1, ValueEnd: -1}
		for i < len(raw) && !isSpace(raw[i]) && raw[i] != '=' && raw[i] != '>' && raw[i] != '/' {
			i++
		}
		a.NameEnd = i
		for i < len(raw) && isSpace(raw[i]) {
			i++
		}
		if i < len(raw) && raw[i] == '=' {
			i++
			for i < len(raw) && isSpace(raw[i]) {
				i++
			}
			if i < len(raw) && (raw[i] == '"' || raw[i] == '\'') {
				q := raw[i]
				i++
				a.Quote, a.ValueStart = q, i
				for i < len(raw) && raw[i] != q {
					i++
				}
				a.ValueEnd = i
				if i < len(raw) {
					i++
				}
			} else {
				a.ValueStart = i
				for i < len(raw) && !isSpace(raw[i]) && raw[i] != '>' {
					i++
				}
				a.ValueEnd = i
			}
		}
		out = append(out, a)
	}
	return out
}

// ---- end of scanner (about 60 lines) --------------------------------------

func main() {
	tags, attrs, mismatch := 0, 0, 0
	for _, pat := range os.Args[1:] {
		files, _ := filepath.Glob(pat)
		if len(files) == 0 { files = []string{pat} }
		for _, f := range files {
			in, err := os.ReadFile(f)
			if err != nil { continue }
			z := html.NewTokenizer(bytes.NewReader(in))
			for {
				tt := z.Next()
				if tt == html.ErrorToken { break }
				if tt != html.StartTagToken && tt != html.SelfClosingTagToken { continue }
				raw := append([]byte(nil), z.Raw()...)
				got := scanAttrs(raw)
				// ground truth from the tokenizer
				var want [][2]string
				z.TagName()
				for {
					k, v, more := z.TagAttr()
					if k != nil { want = append(want, [2]string{string(k), string(v)}) }
					if !more { break }
				}
				tags++
				if len(got) != len(want) {
					mismatch++
					if mismatch <= 5 {
						fmt.Printf("COUNT MISMATCH %s got=%d want=%d raw=%q\n", f, len(got), len(want), raw)
					}
					continue
				}
				for i, a := range got {
					attrs++
					name := string(bytes.ToLower(raw[a.NameStart:a.NameEnd]))
					if name != want[i][0] {
						mismatch++
						if mismatch <= 5 {
							fmt.Printf("NAME MISMATCH %s got=%q want=%q raw=%q\n", f, name, want[i][0], raw)
						}
					}
				}
			}
		}
	}
	fmt.Printf("\nscanned %d start tags, %d attributes, %d mismatches vs tokenizer ground truth\n", tags, attrs, mismatch)
}

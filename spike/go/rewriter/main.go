package main

import (
	"bytes"
	"fmt"
	"io"
	"os"

	"golang.org/x/net/html"
)

// --- the entire rewriter ---------------------------------------------------

type Rewriter struct {
	z       *html.Tokenizer
	from    []byte
	to      []byte
	rawText string // "" or the current raw-text element name
	pend    bytes.Buffer
	done    bool
	Tags    int // counters
	Texts   int
}

func New(r io.Reader, from, to string) *Rewriter {
	return &Rewriter{z: html.NewTokenizer(r), from: []byte(from), to: []byte(to)}
}

func isRawText(n string) bool {
	switch n {
	case "script", "style", "textarea", "title", "iframe", "noembed",
		"noframes", "noscript", "plaintext", "xmp":
		return true
	}
	return false
}

func (w *Rewriter) Read(p []byte) (int, error) {
	for w.pend.Len() == 0 && !w.done {
		tt := w.z.Next()
		if tt == html.ErrorToken {
			w.pend.Write(w.z.Buffered())
			w.done = true
			break
		}
		raw := w.z.Raw()
		switch tt {
		case html.StartTagToken, html.SelfClosingTagToken:
			name, _ := w.z.TagName()
			n := string(name)
			if tt == html.StartTagToken && isRawText(n) {
				w.rawText = n
			}
			if bytes.Contains(raw, w.from) {
				w.Tags++
				w.pend.Write(bytes.ReplaceAll(raw, w.from, w.to))
			} else {
				w.pend.Write(raw)
			}
		case html.EndTagToken:
			w.rawText = ""
			w.pend.Write(raw)
		case html.TextToken:
			if (w.rawText == "script" || w.rawText == "style") && bytes.Contains(raw, w.from) {
				w.Texts++
				w.pend.Write(bytes.ReplaceAll(raw, w.from, w.to))
			} else {
				w.pend.Write(raw) // prose text: never rewritten (test 28)
			}
		default: // Comment, Doctype
			w.pend.Write(raw)
		}
	}
	if w.pend.Len() == 0 && w.done {
		return 0, io.EOF
	}
	return w.pend.Read(p)
}

// --- end of rewriter (about 55 lines) --------------------------------------

func main() {
	from, to := os.Args[1], os.Args[2]
	for _, f := range os.Args[3:] {
		in, err := os.ReadFile(f)
		if err != nil { fmt.Println(f, err); continue }
		w := New(bytes.NewReader(in), from, to)
		out, _ := io.ReadAll(w)
		if from == to {
			same := bytes.Equal(in, out)
			fmt.Printf("%-26s identity: %v (%d -> %d bytes)\n", f, same, len(in), len(out))
			if !same {
				for i := range out { if i >= len(in) || in[i] != out[i] { fmt.Printf("  first diff @%d\n", i); break } }
			}
		} else {
			fmt.Printf("%-26s %d tags, %d script/style texts rewritten, %d -> %d bytes\n", f, w.Tags, w.Texts, len(in), len(out))
			os.WriteFile(f+".go.out", out, 0644)
		}
	}
}

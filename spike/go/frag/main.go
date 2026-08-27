package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"golang.org/x/net/html"
)

type slow struct{ b []byte; n int }
func (s *slow) Read(p []byte) (int, error) {
	if len(s.b) == 0 { return 0, io.EOF }
	k := s.n; if k > len(s.b) { k = len(s.b) }
	copy(p[:min(k, len(p))], s.b[:min(k, len(p))])
	m := min(k, len(p)); s.b = s.b[m:]
	return m, nil
}
func min(a, b int) int { if a < b { return a }; return b }

func main() {
	in, _ := os.ReadFile(os.Args[1])
	for _, chunk := range []int{1, 7, 4096, len(in)} {
		z := html.NewTokenizer(&slow{b: append([]byte(nil), in...), n: chunk})
		var out bytes.Buffer
		textTokens, scriptTokens, maxText := 0, 0, 0
		inScript := false
		for {
			tt := z.Next()
			if tt == html.ErrorToken { out.Write(z.Buffered()); break }
			raw := z.Raw()
			if tt == html.StartTagToken {
				n, _ := z.TagName()
				if string(n) == "script" || string(n) == "style" { inScript = true }
			} else if tt == html.EndTagToken { inScript = false }
			if tt == html.TextToken {
				textTokens++
				if inScript { scriptTokens++; if len(raw) > maxText { maxText = len(raw) } }
			}
			out.Write(raw)
		}
		fmt.Printf("chunk=%-8d identical=%-6v textTokens=%-6d scriptTextTokens=%-5d largestScriptText=%d\n",
			chunk, bytes.Equal(in, out.Bytes()), textTokens, scriptTokens, maxText)
	}
	// does one <script> ever yield more than one text token?
	big := "<html><body><script>" + strings.Repeat("var x='https://www.acmecorp.fi/a';\n", 20000) + "</script></body></html>"
	z := html.NewTokenizer(&slow{b: []byte(big), n: 3})
	n := 0
	inScript := false
	for {
		tt := z.Next()
		if tt == html.ErrorToken { break }
		if tt == html.StartTagToken { t, _ := z.TagName(); inScript = string(t) == "script" }
		if tt == html.TextToken && inScript { n++; fmt.Printf("  big inline script (%d bytes) -> text token #%d of %d bytes\n", len(big), n, len(z.Raw())) }
		if tt == html.EndTagToken { inScript = false }
	}
}

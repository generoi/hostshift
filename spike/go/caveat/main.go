package main

import (
	"bytes"
	"fmt"
	"golang.org/x/net/html"
)

func main() {
	in := []byte(`<A HREF="https://x/?a=1&amp;b=2&#x41;" CLASS='Y'>t</A>`)
	z := html.NewTokenizer(bytes.NewReader(in))
	z.Next()
	rawBefore := append([]byte(nil), z.Raw()...) // defensive copy
	rawAlias := z.Raw()                          // live alias into z.buf
	fmt.Printf("raw before TagAttr : %q\n", rawAlias)
	for {
		k, v, more := z.TagAttr()
		fmt.Printf("  attr key=%q val=%q\n", k, v)
		if !more { break }
	}
	fmt.Printf("raw AFTER  TagAttr : %q\n", rawAlias)
	fmt.Printf("copy taken before  : %q\n", rawBefore)
	fmt.Printf("alias == copy? %v   (docs say it MAY change)\n", bytes.Equal(rawAlias, rawBefore))

	// Token() path
	z2 := html.NewTokenizer(bytes.NewReader(in))
	z2.Next()
	a2 := z2.Raw()
	tok := z2.Token()
	fmt.Printf("\nToken() -> %s\n", tok.String())
	fmt.Printf("raw after Token()  : %q  unchanged=%v\n", a2, bytes.Equal(a2, rawBefore))

	// html.Render round-trip for comparison (the "lossy" reputation)
	doc, _ := html.Parse(bytes.NewReader(in))
	var b bytes.Buffer
	html.Render(&b, doc)
	fmt.Printf("\nhtml.Parse+Render  : %q\n", b.String())
}

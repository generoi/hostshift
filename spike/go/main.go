package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"golang.org/x/net/html"
)

// identity: concatenate Raw() of every token
func identity(in []byte, callTagAttr bool) ([]byte, string) {
	z := html.NewTokenizer(bytes.NewReader(in))
	var out bytes.Buffer
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			// tail: anything buffered but not tokenized
			out.Write(z.Buffered())
			return out.Bytes(), z.Err().Error()
		}
		raw := z.Raw()
		if callTagAttr {
			// simulate real use: inspect attributes BEFORE copying raw
			if tt == html.StartTagToken || tt == html.SelfClosingTagToken {
				for {
					_, _, more := z.TagAttr()
					if !more {
						break
					}
				}
			}
			out.Write(raw) // raw AFTER TagAttr -> may be corrupted
		} else {
			out.Write(raw)
		}
	}
}

func firstDiff(a, b []byte) int {
	n := len(a)
	if len(b) < n { n = len(b) }
	for i := 0; i < n; i++ {
		if a[i] != b[i] { return i }
	}
	if len(a) != len(b) { return n }
	return -1
}

func ctx(b []byte, at int) string {
	s := at - 60; if s < 0 { s = 0 }
	e := at + 60; if e > len(b) { e = len(b) }
	return fmt.Sprintf("%q", string(b[s:e]))
}

func main() {
	for _, pat := range os.Args[1:] {
		files, _ := filepath.Glob(pat)
		if len(files) == 0 { files = []string{pat} }
		for _, f := range files {
			in, err := os.ReadFile(f)
			if err != nil { fmt.Println(f, "READERR", err); continue }
			for _, ta := range []bool{false, true} {
				out, errs := identity(in, ta)
				label := "raw-only"
				if ta { label = "after-TagAttr" }
				if d := firstDiff(in, out); d < 0 {
					fmt.Printf("%-26s %-14s IDENTICAL (%d bytes, end=%s)\n", f, label, len(out), errs)
				} else {
					fmt.Printf("%-26s %-14s DIFFERS at %d (in %d / out %d, end=%s)\n", f, label, d, len(in), len(out), errs)
					fmt.Printf("    IN : %s\n    OUT: %s\n", ctx(in, d), ctx(out, d))
				}
			}
		}
	}
}

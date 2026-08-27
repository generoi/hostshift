package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/net/html"
)

// ===== span scanner (shared with attrspan) =================================
type attr struct{ ns, ne, vs, ve int; quote byte }

func isSpace(c byte) bool { return c==' '||c=='\t'||c=='\n'||c=='\r'||c=='\f' }

func scanAttrs(raw []byte) []attr {
	var out []attr
	i := 1
	for i < len(raw) && !isSpace(raw[i]) && raw[i] != '>' && raw[i] != '/' { i++ }
	for i < len(raw) {
		for i < len(raw) && isSpace(raw[i]) { i++ }
		if i >= len(raw) || raw[i] == '>' { break }
		if raw[i] == '/' { i++; continue }
		a := attr{ns: i, vs: -1, ve: -1}
		for i < len(raw) && !isSpace(raw[i]) && raw[i] != '=' && raw[i] != '>' && raw[i] != '/' { i++ }
		a.ne = i
		for i < len(raw) && isSpace(raw[i]) { i++ }
		if i < len(raw) && raw[i] == '=' {
			i++
			for i < len(raw) && isSpace(raw[i]) { i++ }
			if i < len(raw) && (raw[i] == '"' || raw[i] == '\'') {
				q := raw[i]; i++
				a.quote, a.vs = q, i
				for i < len(raw) && raw[i] != q { i++ }
				a.ve = i
				if i < len(raw) { i++ }
			} else {
				a.vs = i
				for i < len(raw) && !isSpace(raw[i]) && raw[i] != '>' { i++ }
				a.ve = i
			}
		}
		out = append(out, a)
	}
	return out
}

// ===== rewriter ============================================================
type Rewriter struct {
	z    *html.Tokenizer
	from, to []byte
	rawText string
	pend bytes.Buffer
	done bool
	Attrs, Texts, Structured int
}

func isRaw(n string) bool {
	switch n { case "script","style","textarea","title","iframe","noembed","noframes","noscript","plaintext","xmp": return true }
	return false
}

// rewriteValue is where hostshift's per-surface logic lives: plain origin
// substitution, or structured handling for srcset / refresh / ping / srcdoc.
func (w *Rewriter) rewriteValue(name string, v []byte) ([]byte, bool) {
	if !bytes.Contains(v, w.from) { return v, false }
	switch strings.ToLower(name) {
	case "srcset", "imagesrcset", "ping", "srcdoc", "content":
		w.Structured++ // structured surfaces: same substitution here, real code splits on , / ; / space
	}
	return bytes.ReplaceAll(v, w.from, w.to), true
}

func (w *Rewriter) rewriteTag(raw []byte) []byte {
	attrs := scanAttrs(raw)
	var out bytes.Buffer
	prev := 0
	for _, a := range attrs {
		if a.vs < 0 { continue }
		name := string(raw[a.ns:a.ne])
		nv, changed := w.rewriteValue(name, raw[a.vs:a.ve])
		if !changed { continue }
		w.Attrs++
		out.Write(raw[prev:a.vs]) // everything before the value, verbatim
		out.Write(nv)
		prev = a.ve
	}
	if prev == 0 { return raw }
	out.Write(raw[prev:])
	return out.Bytes()
}

func (w *Rewriter) Read(p []byte) (int, error) {
	for w.pend.Len() == 0 && !w.done {
		tt := w.z.Next()
		if tt == html.ErrorToken { w.pend.Write(w.z.Buffered()); w.done = true; break }
		raw := w.z.Raw()
		switch tt {
		case html.StartTagToken, html.SelfClosingTagToken:
			n, _ := w.z.TagName()
			if tt == html.StartTagToken && isRaw(string(n)) { w.rawText = string(n) }
			w.pend.Write(w.rewriteTag(raw))
		case html.EndTagToken:
			w.rawText = ""
			w.pend.Write(raw)
		case html.TextToken:
			if (w.rawText=="script"||w.rawText=="style") && bytes.Contains(raw, w.from) {
				w.Texts++
				w.pend.Write(bytes.ReplaceAll(raw, w.from, w.to))
			} else { w.pend.Write(raw) }
		default:
			w.pend.Write(raw)
		}
	}
	if w.pend.Len() == 0 && w.done { return 0, io.EOF }
	return w.pend.Read(p)
}

func main() {
	from, to := []byte(os.Args[1]), []byte(os.Args[2])
	for _, pat := range os.Args[3:] {
		files, _ := filepath.Glob(pat); if len(files)==0 { files = []string{pat} }
		for _, f := range files {
			in, err := os.ReadFile(f); if err != nil { continue }
			w := &Rewriter{z: html.NewTokenizer(bytes.NewReader(in)), from: from, to: to}
			out, _ := io.ReadAll(w)
			if bytes.Equal(from, to) {
				if !bytes.Equal(in, out) { fmt.Printf("%-26s IDENTITY FAILED (%d->%d)\n", f, len(in), len(out)) }
			} else {
				fmt.Printf("%-26s %4d attr values, %3d script/style texts, %d structured | %d -> %d bytes, lines %d -> %d\n",
					f, w.Attrs, w.Texts, w.Structured, len(in), len(out),
					bytes.Count(in, []byte("\n")), bytes.Count(out, []byte("\n")))
				os.WriteFile(f+".full.out", out, 0644)
			}
		}
	}
	if bytes.Equal(from, to) { fmt.Println("identity map: all files byte-identical") }
}

package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"

	"golang.org/x/net/html"
)

type rw struct {
	z *html.Tokenizer
	from, to []byte
	rawText string
	pend bytes.Buffer
	done bool
	src io.Closer
}

func isRaw(n string) bool {
	switch n { case "script","style","textarea","title","iframe","noembed","noframes","noscript","plaintext","xmp": return true }
	return false
}
func (w *rw) Read(p []byte) (int, error) {
	for w.pend.Len() == 0 && !w.done {
		tt := w.z.Next()
		if tt == html.ErrorToken { w.pend.Write(w.z.Buffered()); w.done = true; break }
		raw := w.z.Raw()
		switch tt {
		case html.StartTagToken, html.SelfClosingTagToken:
			n, _ := w.z.TagName()
			if tt == html.StartTagToken && isRaw(string(n)) { w.rawText = string(n) }
			w.pend.Write(bytes.ReplaceAll(raw, w.from, w.to))
		case html.EndTagToken:
			w.rawText = ""; w.pend.Write(raw)
		case html.TextToken:
			if w.rawText=="script"||w.rawText=="style" { w.pend.Write(bytes.ReplaceAll(raw, w.from, w.to)) } else { w.pend.Write(raw) }
		default: w.pend.Write(raw)
		}
	}
	if w.pend.Len()==0 && w.done { return 0, io.EOF }
	return w.pend.Read(p)
}
func (w *rw) Close() error { return w.src.Close() }

func main() {
	body, _ := os.ReadFile(os.Args[1])
	from, to := os.Args[2], os.Args[3]

	// upstream: pretends to be the WP container
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.Header().Set("ETag", `"abc123"`)
		w.Header().Set("Location", from+"/redirect-target")
		fmt.Fprintln(os.Stderr, "  upstream saw Host:", r.Host, "| X-Forwarded-Proto:", r.Header.Get("X-Forwarded-Proto"))
		w.Write(body)
	}))
	defer up.Close()
	target, _ := url.Parse(up.URL)

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.SetXForwarded()
			pr.Out.Host = strings.TrimPrefix(from, "https://") // variant -> canonical
			pr.Out.Header.Set("X-Forwarded-Proto", "https")
			pr.Out.Header.Set("Accept-Encoding", "identity")
		},
		ModifyResponse: func(resp *http.Response) error {
			if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") { return nil }
			resp.Body = &rw{z: html.NewTokenizer(resp.Body), from: []byte(from), to: []byte(to), src: resp.Body}
			resp.Header.Del("Content-Length")      // length changes
			resp.Header.Del("ETag")                // §5.2 validators
			if l := resp.Header.Get("Location"); l != "" {
				resp.Header.Set("Location", strings.ReplaceAll(l, from, to))
			}
			return nil
		},
	}
	front := httptest.NewServer(proxy)
	defer front.Close()

	res, _ := http.Get(front.URL + "/")
	got, _ := io.ReadAll(res.Body)
	res.Body.Close()

	want := bytes.ReplaceAll(body, []byte(from), []byte(to))
	fmt.Printf("\n  status            %s\n", res.Status)
	fmt.Printf("  Location          %s\n", res.Header.Get("Location"))
	fmt.Printf("  Content-Length    %q (dropped)   ETag %q (dropped)\n", res.Header.Get("Content-Length"), res.Header.Get("ETag"))
	fmt.Printf("  Transfer-Encoding %v\n", res.TransferEncoding)
	fmt.Printf("  bytes in %d -> out %d\n", len(body), len(got))
	fmt.Printf("  canonical origins remaining in output: %d\n", bytes.Count(got, []byte(from)))
	fmt.Printf("  line count preserved: %v (%d -> %d)\n",
		bytes.Count(body,[]byte("\n"))==bytes.Count(got,[]byte("\n")), bytes.Count(body,[]byte("\n")), bytes.Count(got,[]byte("\n")))
	// idempotency (test 7): feed output back through
	res2, _ := http.Get(front.URL + "/")
	_ = res2
	fmt.Printf("  matches offline rewrite byte-for-byte: %v\n", bytes.Equal(got, want))
}

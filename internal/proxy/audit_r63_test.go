package proxy

import (
	"bytes"
	"net/http"
	"testing"
)

// Round 63, end to end: the two foreign-content namespace defects in
// internal/rewrite/html.go reach the browser through the proxy, not merely
// through the filter.
//
// See internal/rewrite/audit_r63_test.go for the mechanism and the html.Parse
// oracle. In short, `w.foreignNS` records a nested foreign element's namespace
// from its own tag name rather than from its parent's, and pops on any
// `</svg>`/`</math>` without checking the name — so hostshift decides "HTML
// rules apply" while the parser is still in foreign content and withholds the
// character-reference decode the browser performs.
//
// The page comes back byte-identical with the canonical origin intact, inside a
// `<script>` the developer's authenticated browser runs against live
// production. That is test 28.
func TestR63ForeignNamespaceLeaksThroughTheProxy(t *testing.T) {
	const ref = `https:&#47;&#47;www.acmecorp.fi/wp-json/wp/v2/users`
	for _, c := range []struct{ name, body string }{
		{"nested math inside svg is SVG, not MathML",
			`<svg><math><mi><script>fetch("` + ref + `")</script></mi></math></svg>`},
		{"stray </math> closes nothing",
			`<svg></math><script>fetch("` + ref + `")</script></svg>`},
		{"nested svg inside math is MathML, not SVG",
			`<math><svg><desc><style>@import url("` + ref + `");</style></desc></svg></math>`},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, acmecorpMap(t), func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.WriteHeader(200)
				_, _ = w.Write([]byte("<!doctype html><html><body>" + c.body + "</body></html>"))
			})
			_, got := h.do(t, "GET", variantHost, "/", "", nil)
			if bytes.Contains(got, []byte("www.acmecorp.fi")) {
				t.Errorf("a dereferenceable production origin reached the browser:\n%s", got)
			}
		})
	}
}

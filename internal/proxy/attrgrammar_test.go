package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The other half of round 74's attribute finding: what must NOT be rewritten.
//
// A control after a host is a boundary in a prose attribute and in a
// whitespace-separated list, which is what
// TestR74BareOriginBeforeAControlInAnAttributeIsNotRewritten pins. In an
// attribute whose entire value is one URL it is the opposite, because the URL
// parser removes TAB, LF and CR before it reads the host. Node's URL, the same
// ada the locator is modelled on, is unambiguous:
//
//	new URL("https://www.r74a.example\nB").host === "www.r74a.exampleb"
//
// So `href="https://www.r74a.example<LF>B"` is a link to `www.r74a.exampleb` —
// a host this map does not name and has no business touching. Rewriting the
// origin inside it produces a link to `wt-a--r74w.ddev.siteb`, sending the
// browser somewhere it was never going.
//
// Without this test the guard is not load-bearing: running the prose pass over
// *every* attribute, href included, passes both the finding above and
// TestR52CrossProduct's 253,680 cells. It is wrong all the same, and this is
// the cell that says so.
func TestASingleURLAttributeStillJoinsAcrossAControl(t *testing.T) {
	const canon = "https://www.r74a.example"

	type cell struct{ name, doc, want string }
	var cells []cell
	for _, sep := range []struct{ name, b string }{
		{"TAB", "\t"}, {"LF", "\n"}, {"CR", "\r"},
	} {
		for _, attr := range []string{"href", "src", "action", "cite", "poster"} {
			// The host the browser actually resolves is canonical + "b", which
			// is not in the map. The value has to come out untouched.
			doc := `<a ` + attr + `="` + canon + sep.b + `B">x</a>`
			cells = append(cells, cell{attr + "/" + sep.name, doc, canon + sep.b + "B"})
		}
	}

	for _, c := range cells {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			io.WriteString(w, "<!doctype html><html><body>"+c.doc+"</body></html>")
		}))
		p := r74proxy(t, up.URL)
		front := httptest.NewServer(p.Handler())

		req, _ := http.NewRequest("GET", front.URL+"/", nil)
		req.Host = "wt-a--r74w.ddev.site"
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		out, _ := io.ReadAll(res.Body)
		res.Body.Close()
		front.Close()
		up.Close()

		if !bytes.Contains(out, []byte("</body>")) {
			t.Fatalf("harness: %s got no document back (%d): %q", c.name, res.StatusCode, out)
		}
		if !bytes.Contains(out, []byte(c.want)) {
			t.Errorf("%s: a single-URL attribute was rewritten across a control. "+
				"The browser resolves that value to a host this map does not name, "+
				"so rewriting it points the link somewhere it was never going.\n"+
				" in:  %s\n out: %s", c.name, c.doc, out)
		}
		// And the variant must not appear: that is the same defect stated the
		// other way round, and it is what a reader will actually see.
		if bytes.Contains(out, []byte("wt-a--r74w.ddev.site")) {
			t.Errorf("%s: the value now names the variant, so the link resolves to "+
				"wt-a--r74w.ddev.siteb\n out: %s", c.name, out)
		}
	}
}
